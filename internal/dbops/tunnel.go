package dbops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
)

// hooks are the network, clock and signal edges of tunnels; tests replace them.
type hooks struct {
	dial         func(ctx context.Context, socket string) error
	readyTimeout time.Duration
	pollEvery    time.Duration
	settle       time.Duration                           // after a ready dial, how long an exiting ssh is waited for
	reapGrace    time.Duration                           // SIGTERM to SIGKILL in Reap
	signal       func(pid int, sig syscall.Signal) error // to the process group
}

var netHooks = hooks{
	dial:         dial,
	readyTimeout: 30 * time.Second,
	pollEvery:    200 * time.Millisecond,
	settle:       100 * time.Millisecond,
	reapGrace:    3 * time.Second,
	signal:       func(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) },
}

func dial(ctx context.Context, socket string) error {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	return c.Close()
}

const (
	tunnelDirPrefix = "rm-tunnel-"
	maxSocketPath   = 103 // sun_path is 104 bytes on macOS, 108 on Linux
)

// DefaultSocketName is the socket file name Open uses when given none.
const DefaultSocketName = "s"

// Tunnel is an ssh forward from a Unix socket to a port inside a service's
// container. The socket lives alone in a fresh 0700 directory, so only ssh can
// create it and only this user can connect: no other local process can stand
// in for the tunnel or read what a client sends through it. Socket and Pid are
// what a run records so the next one can reap it; neither is a secret.
type Tunnel struct {
	Socket string
	Pid    int
	// Member is the 1-based number errors name this tunnel by among its
	// database's places, when some places have no tunnel; 0 means its
	// position in the slice given.
	Member int

	p proc.Process
}

// number is how errors name the tunnel at position i.
func (t Tunnel) number(i int) int {
	if t.Member > 0 {
		return t.Member
	}
	return i + 1
}

// Dir is the private directory that holds the socket.
func (t Tunnel) Dir() string { return filepath.Dir(t.Socket) }

// forwardSpec is the part of an ssh command line that marks a tunnel of ours
// (adapted to a socket forward).
func forwardSpec(socket string) string { return "-L " + socket + ":" }

// tempRoots are the directories tunnel directories are made in: $TMPDIR when
// it is absolute, then /tmp.
func tempRoots() []string {
	roots := []string{}
	if t := filepath.Clean(os.TempDir()); filepath.IsAbs(t) && t != "/tmp" {
		roots = append(roots, t)
	}
	return append(roots, "/tmp")
}

// tunnelDir makes a fresh 0700 directory whose socket path fits sun_path,
// falling back to /tmp when $TMPDIR is relative, unusable or too deep.
func tunnelDir(socketName string) (string, error) {
	var errs []error
	for _, root := range tempRoots() {
		dir, err := os.MkdirTemp(root, tunnelDirPrefix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(filepath.Join(dir, socketName)) <= maxSocketPath {
			return dir, nil
		}
		_ = os.Remove(dir)
		errs = append(errs, errors.New("socket path too long"))
	}
	return "", fmt.Errorf("tunnel directory: %w", errors.Join(errs...))
}

// removeDir deletes a tunnel directory, but only one this package could have
// made: a "rm-tunnel-" directory right inside a temp root. Anything else, such
// as a tampered record, is left alone and reported.
func removeDir(socket string) error {
	dir := filepath.Dir(socket)
	if socket != "" && filepath.IsAbs(socket) && filepath.Clean(socket) == socket &&
		strings.HasPrefix(filepath.Base(dir), tunnelDirPrefix) && slices.Contains(tempRoots(), filepath.Dir(dir)) {
		return os.RemoveAll(dir)
	}
	return fmt.Errorf("not a tunnel directory: %s", text.Sanitize(dir))
}

// Open forwards a Unix socket named socketName (DefaultSocketName when empty;
// psql wants ".s.PGSQL.<port>") in a fresh private directory to
// remoteHost:remotePort inside the container behind userHost. remoteHost is
// 127.0.0.1 except where a database trusts that address and a password must
// be proved on the container's network address. ssh runs in its own process
// group with stdin closed. onStart (nil: none) gets the tunnel right after
// ssh starts, before anything waits on it, so the caller can record it; if it
// fails, ssh is stopped and its error returned. The tunnel is ready once the
// socket accepts a connection and ssh is still running; Open fails with ssh's
// last stderr line if ssh exits first, and after 30 s otherwise. On any
// failure ssh is stopped and the directory removed. ssh trusts only the host
// keys of knownHosts (KnownHostsFile): a host it does not hold yet gets no
// connection, and Open returns its keys as an *UntrustedHostError for the
// operator to compare and trust. ssh never forwards the agent or X11.
func Open(ctx context.Context, r proc.Runner, knownHosts, userHost string, remotePort int, remoteHost, socketName string, onStart func(Tunnel) error) (*Tunnel, error) {
	if knownHosts == "" {
		return nil, errors.New("no known hosts file: ssh would trust any host")
	}
	host := userHost[strings.LastIndex(userHost, "@")+1:]
	ok, err := Trusted(knownHosts, host)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", knownHosts, err)
	}
	if !ok {
		hk, err := ScanHostKeys(ctx, r, host)
		if err != nil {
			return nil, err
		}
		return nil, &UntrustedHostError{HostKeys: hk}
	}
	if socketName == "" {
		socketName = DefaultSocketName
	}
	if socketName != filepath.Base(socketName) || socketName == "." || socketName == ".." {
		return nil, fmt.Errorf("bad socket name %q", text.Sanitize(socketName))
	}
	dir, err := tunnelDir(socketName)
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, socketName)
	p, err := r.Start(ctx, proc.Cmd{Name: "ssh", Args: []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ServerAliveInterval=30",
		"-o", "StreamLocalBindUnlink=yes",
		"-L", socket + ":" + remoteHost + ":" + strconv.Itoa(remotePort),
		"--", userHost,
	}})
	if err != nil {
		_ = removeDir(socket)
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	t := &Tunnel{Socket: socket, Pid: p.Pid(), p: p}
	if onStart != nil {
		if err := onStart(Tunnel{Socket: t.Socket, Pid: t.Pid}); err != nil {
			_ = t.Close()
			return nil, err
		}
	}
	if err := t.waitReady(ctx); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

func (t *Tunnel) waitReady(ctx context.Context) error {
	exited := make(chan error, 1)
	go func() { exited <- t.p.Wait() }()
	deadline := time.NewTimer(netHooks.readyTimeout)
	defer deadline.Stop()
	for {
		if netHooks.dial(ctx, t.Socket) == nil {
			select {
			case err := <-exited:
				return sshExited(err)
			case <-time.After(netHooks.settle):
				return nil
			}
		}
		select {
		case err := <-exited:
			return sshExited(err)
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("tunnel not ready after 30s")
		case <-time.After(netHooks.pollEvery):
		}
	}
}

func sshExited(err error) error {
	var ee *proc.ExitError
	if errors.As(err, &ee) && ee.LastStderr != "" {
		return fmt.Errorf("ssh exited: %s", text.Sanitize(ee.LastStderr))
	}
	if err != nil {
		return fmt.Errorf("ssh exited: %s", text.Sanitize(err.Error()))
	}
	return errors.New("ssh exited")
}

// Close stops ssh (SIGTERM to its group, SIGKILL after 3 s) and removes the
// socket's directory. It is safe to call more than once. A Tunnel only
// recorded, with no process of this run, is for Reap.
func (t *Tunnel) Close() error {
	var err error
	if t.p != nil {
		err = t.p.Stop()
	}
	return errors.Join(err, removeDir(t.Socket))
}

// Reap stops the ssh of each tunnel a killed run recorded, but only while the
// pid's command line still carries our forward spec for that socket, so a
// reused pid is never signalled: SIGTERM to the group,
// then SIGKILL after 3 s. Then it removes the tunnel's directory. A gone
// process is skipped. A live pid whose command line cannot be read is
// unknown: it is reported and its directory left. It returns those and the
// signal failures other than "no such process"; callers keep the records
// of the tunnels it reported (reap them one by one to tell which).
func Reap(r proc.Runner, recorded []Tunnel) error {
	var errs []error
	for _, t := range recorded {
		if t.Pid > 0 {
			line, err := r.Cmdline(t.Pid)
			if err != nil && !gone(t.Pid) {
				// Alive, or not known gone, with no readable command line:
				// whether it is our ssh is unknown, so it is neither
				// signalled nor forgotten.
				errs = append(errs, fmt.Errorf("ssh %d: command line unreadable, tunnel kept", t.Pid))
				continue
			}
			if err == nil && strings.Contains(line, forwardSpec(t.Socket)) {
				if err := terminate(t.Pid); err != nil {
					errs = append(errs, err)
					continue
				}
			}
		}
		if err := removeDir(t.Socket); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// gone reports whether no process is left in pid's group.
func gone(pid int) bool { return errors.Is(netHooks.signal(pid, 0), syscall.ESRCH) }

func terminate(pid int) error {
	if err := netHooks.signal(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("stop ssh %d: %w", pid, err)
	}
	deadline := time.Now().Add(netHooks.reapGrace)
	for time.Now().Before(deadline) {
		if errors.Is(netHooks.signal(pid, 0), syscall.ESRCH) {
			return nil
		}
		time.Sleep(netHooks.pollEvery)
	}
	if err := netHooks.signal(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill ssh %d: %w", pid, err)
	}
	return nil
}

const (
	retryAttempts = 4
	retryGap      = 2 * time.Second
)

// transient is client output right after a tunnel opens that is worth a retry.
var transient = regexp.MustCompile(`(?i)connection (reset|refused|closed)|ECONNRESET|ECONNREFUSED|server closed the connection|could not connect|timed out`)

// Sleep waits d or until ctx ends; it is Retry's real sleep.
func Sleep(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Retry runs f up to 4 times, 2 s apart, while it fails with output (or an
// error) that looks transient; an error wrapping
// ErrIncomplete never is (its outcome is unknown). It returns f's last
// result, or ctx's error when ctx ends during a wait.
func Retry(ctx context.Context, sleep func(context.Context, time.Duration) error, f func() (string, error)) (string, error) {
	out, err := f()
	for i := 1; i < retryAttempts && err != nil && !errors.Is(err, ErrIncomplete) && (transient.MatchString(out) || transient.MatchString(err.Error())); i++ {
		if serr := sleep(ctx, retryGap); serr != nil {
			return out, serr
		}
		out, err = f()
	}
	return out, err
}
