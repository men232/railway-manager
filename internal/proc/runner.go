package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Cmd describes one child process. Secrets travel in Env and Stdin, never in
// Args: argv is world-readable through ps.
type Cmd struct {
	Name  string
	Args  []string
	Env   []string // added to the parent environment
	Unset []string // names of parent variables the child does not get (nor railwayTokens, but for railway)
	Stdin string   // empty means stdin is closed
}

// Runner starts external programs. OS is the real one, Fake the scripted one.
type Runner interface {
	// Run waits for the program and returns its output.
	Run(ctx context.Context, c Cmd) (stdout, stderr string, err error)
	// Start launches a long-lived program (an ssh tunnel). Stdout is discarded;
	// the last 4 KiB of stderr are kept, and Process.Wait reports a non-zero
	// exit as *ExitError with its last stderr line.
	Start(ctx context.Context, c Cmd) (Process, error)
	LookPath(name string) (string, error)
	// Cmdline returns the command line of a running pid. An error or an empty
	// line means the pid is not ours to signal.
	Cmdline(pid int) (string, error)
}

// Process is a started program in its own process group.
type Process interface {
	Pid() int
	Wait() error
	// Stop sends SIGTERM to the process group, then SIGKILL after 3 s.
	Stop() error
}

// ExitError is a program that ran and exited non-zero. Stderr is untrusted:
// callers scrub it before it leaves their package.
type ExitError struct {
	Code       int
	LastStderr string
}

func (e *ExitError) Error() string {
	if e.LastStderr == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, e.LastStderr)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

const stopGrace = 3 * time.Second

// OS runs real programs. Run never logs or echoes Cmd.Env or Cmd.Stdin.
type OS struct{}

var _ Runner = OS{}

// railwayTokens are the Railway credentials in our environment; only the
// railway CLI gets them.
var railwayTokens = []string{"RAILWAY_API_TOKEN", "RAILWAY_TOKEN"}

func (OS) command(c Cmd) *exec.Cmd {
	cmd := exec.Command(c.Name, c.Args...)
	unset := c.Unset
	if filepath.Base(c.Name) != "railway" {
		unset = append(slices.Clone(c.Unset), railwayTokens...)
	}
	cmd.Env = append(environ(unset), c.Env...)
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = 2 * time.Second // an escaped descendant holding a pipe must not block Wait
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	return cmd
}

// environ is the parent environment without the variables named in unset.
func environ(unset []string) []string {
	env := os.Environ()
	if len(unset) == 0 {
		return env
	}
	return slices.DeleteFunc(env, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(unset, k)
	})
}

func (o OS) Run(ctx context.Context, c Cmd) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	cmd := o.command(c)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start %s: %w", c.Name, unwrapped(err))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		killGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return stdout.String(), stderr.String(), ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), stderr.String(), &ExitError{Code: ee.ExitCode(), LastStderr: lastLine(stderr.String())}
		}
		return stdout.String(), stderr.String(), fmt.Errorf("run %s: %w", c.Name, err)
	}
	return stdout.String(), stderr.String(), nil
}

// Start launches the program with stdout discarded and the last 4 KiB of
// stderr kept, so Wait reports a non-zero exit as *ExitError with its last
// stderr line. The process is not bound to ctx: it lives until Stop or its own exit.
func (o OS) Start(ctx context.Context, c Cmd) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := o.command(c)
	tail := &tailWriter{max: stderrTail}
	cmd.Stdout, cmd.Stderr = nil, tail // stdout to /dev/null
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Name, unwrapped(err))
	}
	p := &osProcess{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = &ExitError{Code: ee.ExitCode(), LastStderr: lastLine(tail.String())}
		}
		p.err = err
		close(p.done)
	}()
	return p, nil
}

func (OS) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (OS) Cmdline(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")), nil
	}
	out, err := psOutput("-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", fmt.Errorf("no process %d", pid)
	}
	return strings.TrimSpace(string(out)), nil
}

// psName and psTimeout are the ps Cmdline runs and how long it may take;
// tests replace them.
var (
	psName    = "ps"
	psTimeout = 5 * time.Second
)

// psOutput runs ps with args, cut off after psTimeout: a ps that does not
// answer is an error (the caller then knows nothing of the process).
func psOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psName, args...)
	cmd.Env = environ(railwayTokens) // ps needs no token of ours
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

const stderrTail = 4 << 10

// tailWriter keeps the last max bytes written to it. Writes never block or
// fail, so a chatty child cannot stall on its stderr pipe.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	if over := len(w.buf) - w.max; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	return len(b), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

type osProcess struct {
	pid  int
	done chan struct{}
	err  error // valid after done is closed
}

func (p *osProcess) Pid() int { return p.pid }

func (p *osProcess) Wait() error {
	<-p.done
	return p.err
}

func (p *osProcess) Stop() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	killGroup(p.pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		killGroup(p.pid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}

func unwrapped(err error) error {
	if u := errors.Unwrap(err); u != nil {
		return u
	}
	return err
}

func killGroup(pid int, sig syscall.Signal) { _ = syscall.Kill(-pid, sig) }
