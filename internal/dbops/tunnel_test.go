package dbops

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/men232/railway-manager/internal/proc"
)

// fakeNet swaps the package's dial and clock hooks for the test.
func fakeNet(t *testing.T, dial func(socket string) error) {
	t.Helper()
	old := netHooks
	t.Cleanup(func() { netHooks = old })
	netHooks.dial = func(_ context.Context, socket string) error { return dial(socket) }
	netHooks.readyTimeout = 300 * time.Millisecond
	netHooks.pollEvery = time.Millisecond
	netHooks.settle = 50 * time.Millisecond
}

func noRecord(Tunnel) error { return nil }

func assertGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tunnel dir %s left behind: %v", dir, err)
	}
}

// testHostKey is a dummy ed25519 host key and its SHA256 fingerprint as
// ssh-keygen -l prints it.
const (
	testHostKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIC/3HMGJU6ZTAKmx4j6OvVVZMxv/6Wx2e+nrJWCfN5xu"
	testHostKeyFP = "SHA256:u1QuOrYHORJv/vPkg03o7Lm+Tnjj+8j0VzPomvJuDOU"
)

// knownHosts is a known hosts file trusting hosts with testHostKey.
func knownHosts(t *testing.T, hosts ...string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "known_hosts")
	var b strings.Builder
	for _, h := range hosts {
		b.WriteString(h + " " + testHostKey + "\n")
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func sshArgs(known, socket, remote string) []string {
	return []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + known,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ServerAliveInterval=30",
		"-o", "StreamLocalBindUnlink=yes",
		"-L", socket + ":" + remote,
		"--", "u@ssh.railway.com",
	}
}

// A host the known hosts file does not hold yet is never connected to: Open
// reads its keys with ssh-keyscan and returns them for the operator to
// compare, and no ssh starts.
func TestOpenRefusesUntrustedHost(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh-keyscan", nil, proc.FakeReply{Stdout: "# ssh.railway.com:22 SSH-2.0-OpenSSH\nssh.railway.com " + testHostKey + "\n"})
	f.On("ssh", nil, proc.FakeReply{Pid: 4242})
	known := knownHosts(t, "other.example")
	_, err := Open(context.Background(), f, known, "u@ssh.railway.com", 6379, "127.0.0.1", "", noRecord)
	var ue *UntrustedHostError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want an untrusted host", err)
	}
	if ue.Host != "ssh.railway.com" || !slices.Equal(ue.Fingerprints, []string{testHostKeyFP}) ||
		!slices.Equal(ue.Keys, []string{"ssh.railway.com " + testHostKey}) {
		t.Errorf("untrusted = %+v", ue.HostKeys)
	}
	if !strings.Contains(err.Error(), testHostKeyFP) {
		t.Errorf("the error does not show the fingerprint: %v", err)
	}
	for _, c := range f.Calls() {
		if c.Name == "ssh" {
			t.Fatal("ssh started for an untrusted host")
		}
	}
	if err := TrustHostKeys(known, ue.HostKeys); err != nil {
		t.Fatal(err)
	}
	if ok, err := Trusted(known, "ssh.railway.com"); !ok || err != nil {
		t.Errorf("trusted after TrustHostKeys = %v, %v", ok, err)
	}
	if ok, _ := Trusted(known, "other.example"); !ok {
		t.Error("TrustHostKeys dropped an entry it already held")
	}
	if st, err := os.Stat(known); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("known hosts mode = %v, %v", st.Mode().Perm(), err)
	}
}

// No known hosts file is no trust at all.
func TestOpenWithoutKnownHosts(t *testing.T) {
	f := &proc.Fake{}
	if _, err := Open(context.Background(), f, "", "u@h", 6379, "127.0.0.1", "", noRecord); err == nil || len(f.Calls()) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(f.Calls()))
	}
}

func TestOpenArgvAndReady(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{Pid: 4242})
	dials := 0
	var dialed string
	var recorded []Tunnel
	fakeNet(t, func(socket string) error {
		dials++
		dialed = socket
		if len(recorded) != 1 {
			t.Error("dialed before onStart")
		}
		if dials < 3 {
			return errors.New("refused")
		}
		return nil
	})
	known := knownHosts(t, "ssh.railway.com")
	tun, err := Open(context.Background(), f, known, "u@ssh.railway.com", 6379, "127.0.0.1", "", func(t Tunnel) error {
		recorded = append(recorded, t)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := tun.Dir()
	if tun.Pid != 4242 || filepath.Base(tun.Socket) != "s" || dialed != tun.Socket ||
		!strings.HasPrefix(filepath.Base(dir), "rm-tunnel-") || len(tun.Socket) > maxSocketPath {
		t.Fatalf("tun=%+v dialed=%q", tun, dialed)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 || !fi.IsDir() {
		t.Fatalf("dir %s: %v %v", dir, fi, err)
	}
	if len(recorded) != 1 || recorded[0].Socket != tun.Socket || recorded[0].Pid != 4242 {
		t.Fatalf("recorded=%+v", recorded)
	}
	c := f.Calls()[0]
	if c.Name != "ssh" || !slices.Equal(c.Args, sshArgs(known, tun.Socket, "127.0.0.1:6379")) || c.Stdin != "" {
		t.Fatalf("cmd=%+v", c)
	}
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.Processes()[0].Stopped() {
		t.Fatal("Close did not stop ssh")
	}
	assertGone(t, dir)
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenPostgresSocketAndRemoteHost(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	fakeNet(t, func(string) error { return nil })
	known := knownHosts(t, "ssh.railway.com")
	tun, err := Open(context.Background(), f, known, "u@ssh.railway.com", 5432, "postgres-4khz.railway.internal", ".s.PGSQL.5432", noRecord)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	if filepath.Base(tun.Socket) != ".s.PGSQL.5432" ||
		!slices.Equal(f.Calls()[0].Args, sshArgs(known, tun.Socket, "postgres-4khz.railway.internal:5432")) {
		t.Fatalf("socket=%q args=%v", tun.Socket, f.Calls()[0].Args)
	}
}

func TestOpenBadSocketName(t *testing.T) {
	for _, name := range []string{"a/b", "..", "."} {
		if _, err := Open(context.Background(), &proc.Fake{}, knownHosts(t, "h"), "u@h", 1, "127.0.0.1", name, noRecord); err == nil {
			t.Errorf("%q: want error", name)
		}
	}
}

func TestOpenOnStartFails(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	fakeNet(t, func(string) error { t.Error("dialed after a failed record"); return nil })
	boom := errors.New("state write failed")
	var dir string
	_, err := Open(context.Background(), f, knownHosts(t, "h"), "u@h", 6379, "127.0.0.1", "", func(t Tunnel) error {
		dir = t.Dir()
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if !f.Processes()[0].Stopped() {
		t.Fatal("ssh left running")
	}
	assertGone(t, dir)
}

// openFailing opens a tunnel expected to fail, checks ssh was stopped and its
// directory removed, and returns the error.
func openFailing(ctx context.Context, t *testing.T, f *proc.Fake) error {
	t.Helper()
	var dir string
	_, err := Open(ctx, f, knownHosts(t, "h"), "u@h", 6379, "127.0.0.1", "", func(t Tunnel) error { dir = t.Dir(); return nil })
	if !f.Processes()[0].Stopped() {
		t.Fatal("Close not called: ssh left running")
	}
	assertGone(t, dir)
	return err
}

func TestOpenSSHExits(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	fakeNet(t, func(string) error {
		f.Processes()[0].Exit(&proc.ExitError{Code: 255, LastStderr: "Permission denied (publickey).\x1b[31m"})
		return errors.New("refused")
	})
	if err := openFailing(context.Background(), t, f); err == nil || err.Error() != "ssh exited: Permission denied (publickey)." {
		t.Fatalf("err=%v", err)
	}
}

// ssh answering once and then exiting (a forward failure) is a failure.
func TestOpenSSHExitsAfterReadyDial(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	fakeNet(t, func(string) error {
		f.Processes()[0].Exit(&proc.ExitError{Code: 255, LastStderr: "administratively prohibited: open failed"})
		return nil
	})
	if err := openFailing(context.Background(), t, f); err == nil || err.Error() != "ssh exited: administratively prohibited: open failed" {
		t.Fatalf("err=%v", err)
	}
	if len(f.Processes()) != 1 {
		t.Fatalf("ssh started %d times", len(f.Processes()))
	}
}

func TestOpenNotReady(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	fakeNet(t, func(string) error { return errors.New("refused") })
	if err := openFailing(context.Background(), t, f); err == nil || err.Error() != "tunnel not ready after 30s" {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenCancelled(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{})
	ctx, cancel := context.WithCancel(context.Background())
	fakeNet(t, func(string) error { cancel(); return errors.New("refused") })
	if err := openFailing(ctx, t, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestOpenStartFails(t *testing.T) {
	f := &proc.Fake{}
	f.On("ssh", nil, proc.FakeReply{Err: errors.New("exec: \"ssh\": not found")})
	fakeNet(t, func(string) error { return nil })
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "rm-tunnel-*"))
	if _, err := Open(context.Background(), f, knownHosts(t, "h"), "u@h", 6379, "127.0.0.1", "", noRecord); err == nil {
		t.Fatal("want error")
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "rm-tunnel-*"))
	if len(after) > len(before) {
		t.Fatalf("tunnel dir left behind: %v", after)
	}
}

func TestDialReal(t *testing.T) {
	dir, err := os.MkdirTemp("", "rmt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	if err := netHooks.dial(context.Background(), socket); err == nil {
		t.Fatal("dial of a missing socket succeeded")
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	if err := netHooks.dial(context.Background(), socket); err != nil {
		t.Fatalf("dial: %v", err)
	}
}

type sig struct {
	pid int
	sig syscall.Signal
}

// fakeSignals scripts process groups: alive pids answer signal 0 until the
// signal in dies kills them; errs makes every signal to a pid fail.
func fakeSignals(t *testing.T, alive map[int]bool, dies map[int]syscall.Signal, errs map[int]error) *[]sig {
	t.Helper()
	old := netHooks
	t.Cleanup(func() { netHooks = old })
	var sent []sig
	netHooks.reapGrace = 30 * time.Millisecond
	netHooks.pollEvery = time.Millisecond
	netHooks.signal = func(pid int, s syscall.Signal) error {
		if s != 0 {
			sent = append(sent, sig{pid, s})
		}
		if err := errs[pid]; err != nil {
			return err
		}
		if !alive[pid] {
			return syscall.ESRCH
		}
		if s != 0 && dies[pid] == s {
			alive[pid] = false
		}
		return nil
	}
	return &sent
}

// recordedDir makes a tunnel directory as Open would and returns its socket path.
func recordedDir(t *testing.T) string {
	t.Helper()
	dir, err := tunnelDir("s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestReap(t *testing.T) {
	s1, s2, s3, s4, s5 := recordedDir(t), recordedDir(t), recordedDir(t), recordedDir(t), recordedDir(t)
	f := &proc.Fake{}
	f.SetCmdline(100, "ssh -N -o StreamLocalBindUnlink=yes -L "+s1+":127.0.0.1:5432 u@h")
	f.SetCmdline(200, "/usr/bin/vim notes.txt")               // pid reused by an unrelated process
	f.SetCmdline(300, "ssh -N -L "+s1+"x:127.0.0.1:5432 u@h") // another socket
	f.SetCmdline(500, "ssh -N -L "+s5+":127.0.0.1:6379 u@h")  // ignores SIGTERM
	// 400: gone (no command line)
	sent := fakeSignals(t,
		map[int]bool{100: true, 200: true, 300: true, 500: true},
		map[int]syscall.Signal{100: syscall.SIGTERM, 500: syscall.SIGKILL}, nil)

	err := Reap(f, []Tunnel{{Socket: s1, Pid: 100}, {Socket: s2, Pid: 200}, {Socket: s3, Pid: 300}, {Socket: s4, Pid: 400}, {Socket: s5, Pid: 500}})
	if err != nil {
		t.Fatal(err)
	}
	want := []sig{{100, syscall.SIGTERM}, {500, syscall.SIGTERM}, {500, syscall.SIGKILL}}
	if !slices.Equal(*sent, want) {
		t.Fatalf("sent=%v", *sent)
	}
	for _, s := range []string{s1, s2, s3, s4, s5} {
		assertGone(t, filepath.Dir(s))
	}
}

// A live pid whose command line cannot be read is unknown: not signalled,
// reported, its directory kept.
func TestReapKeepsUnknown(t *testing.T) {
	s1 := recordedDir(t)
	sent := fakeSignals(t, map[int]bool{600: true}, nil, nil)
	err := Reap(&proc.Fake{}, []Tunnel{{Socket: s1, Pid: 600}})
	if err == nil || !strings.Contains(err.Error(), "600") {
		t.Fatalf("err=%v", err)
	}
	if len(*sent) != 0 {
		t.Errorf("signalled %v", *sent)
	}
	if _, err := os.Stat(filepath.Dir(s1)); err != nil {
		t.Errorf("directory removed: %v", err)
	}
}

func TestReapReportsKillErrors(t *testing.T) {
	s1, s2 := recordedDir(t), recordedDir(t)
	f := &proc.Fake{}
	f.SetCmdline(100, "ssh -N -L "+s1+":127.0.0.1:5432 u@h")
	f.SetCmdline(200, "ssh -N -L "+s2+":127.0.0.1:6379 u@h") // exits between Cmdline and the signal
	fakeSignals(t, map[int]bool{100: true}, nil, map[int]error{100: syscall.EPERM})
	err := Reap(f, []Tunnel{{Socket: s1, Pid: 100}, {Socket: s2, Pid: 200}})
	if !errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "200") {
		t.Fatalf("err=%v", err)
	}
	assertGone(t, filepath.Dir(s2))
}

func TestRemoveDirOnlyOurs(t *testing.T) {
	foreign := filepath.Join(t.TempDir(), "rm-tunnel-x") // right name, wrong parent
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	plain, err := os.MkdirTemp(os.TempDir(), "other-") // right parent, wrong name
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(plain)
	for _, socket := range []string{
		filepath.Join(foreign, "s"),
		filepath.Join(plain, "s"),
		"rm-tunnel-x/s",
		"",
		filepath.Join(os.TempDir(), "rm-tunnel-x", "..", "..", "s"),
	} {
		if err := removeDir(socket); err == nil {
			t.Errorf("%q: want a refusal", socket)
		}
	}
	for _, dir := range []string{foreign, plain} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("removed %s: %v", dir, err)
		}
	}
	ours := recordedDir(t)
	if err := removeDir(ours); err != nil {
		t.Fatal(err)
	}
	assertGone(t, filepath.Dir(ours))
}

func TestReapReportsForeignDir(t *testing.T) {
	foreign := filepath.Join(t.TempDir(), "rm-tunnel-x")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeSignals(t, nil, nil, nil)
	if err := Reap(&proc.Fake{}, []Tunnel{{Socket: filepath.Join(foreign, "s"), Pid: 0}}); err == nil {
		t.Fatal("want a refusal")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("removed %s: %v", foreign, err)
	}
}

func TestTunnelDirFallsBackToTmp(t *testing.T) {
	for name, tmpdir := range map[string]string{
		"relative": "relative-tmp",
		"missing":  "/nonexistent-rm-test-dir",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", tmpdir)
			dir, err := tunnelDir("s")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			if filepath.Dir(dir) != "/tmp" {
				t.Fatalf("dir=%s", dir)
			}
			if err := removeDir(filepath.Join(dir, "s")); err != nil {
				t.Fatal(err)
			}
			assertGone(t, dir)
		})
	}
	if _, err := os.Stat("relative-tmp"); err == nil {
		t.Fatal("made a relative temp dir")
	}
}

func TestForwardSpec(t *testing.T) {
	if got := forwardSpec("/tmp/rm-tunnel-1/s"); got != "-L /tmp/rm-tunnel-1/s:" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains("-L /tmp/rm-tunnel-1/sx:127.0.0.1:1", forwardSpec("/tmp/rm-tunnel-1/s")) {
		t.Fatal("prefix socket matched")
	}
}

func TestRetryTransient(t *testing.T) {
	patterns := []string{
		"connection reset by peer",
		"Connection refused",
		"connection closed",
		"read ECONNRESET",
		"connect ECONNREFUSED 127.0.0.1:5000",
		"server closed the connection unexpectedly",
		"psql: error: could not connect to server",
		"operation timed out",
	}
	for _, out := range patterns {
		t.Run(out, func(t *testing.T) {
			var slept []time.Duration
			sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
			n := 0
			got, err := Retry(context.Background(), sleep, func() (string, error) {
				n++
				if n < 3 {
					return out, errors.New("exit status 1")
				}
				return "ok", nil
			})
			if err != nil || got != "ok" || n != 3 {
				t.Fatalf("got=%q err=%v n=%d", got, err, n)
			}
			if !slices.Equal(slept, []time.Duration{2 * time.Second, 2 * time.Second}) {
				t.Fatalf("slept=%v", slept)
			}
		})
	}
}

func TestRetryGivesUpAfterFourAttempts(t *testing.T) {
	n := 0
	sleep := func(context.Context, time.Duration) error { return nil }
	_, err := Retry(context.Background(), sleep, func() (string, error) {
		n++
		return "", errors.New("ECONNRESET")
	})
	if err == nil || n != 4 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestRetryNotTransient(t *testing.T) {
	n := 0
	sleep := func(context.Context, time.Duration) error { t.Fatal("slept"); return nil }
	_, err := Retry(context.Background(), sleep, func() (string, error) {
		n++
		return "password authentication failed", errors.New("exit status 2")
	})
	if err == nil || n != 1 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	sleep := func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	_, err := Retry(ctx, sleep, func() (string, error) {
		n++
		return "connection refused", errors.New("exit status 1")
	})
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}
