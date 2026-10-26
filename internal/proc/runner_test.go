package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestOSRunEchoesStdin(t *testing.T) {
	out, _, err := OS{}.Run(context.Background(), Cmd{Name: "cat", Stdin: "hello"})
	if err != nil || out != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestOSRunEnvAndExitError(t *testing.T) {
	out, _, err := OS{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "printf %s \"$RM_TEST_VAR\""}, Env: []string{"RM_TEST_VAR=x1"}})
	if err != nil || out != "x1" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	_, _, err = OS{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo a >&2; echo boom >&2; exit 3"}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.LastStderr != "boom" {
		t.Fatalf("err=%v", err)
	}
}

func groupGone(pid int) bool {
	for range 20 {
		if syscall.Kill(-pid, 0) != nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestOSStartStopKillsGroup(t *testing.T) {
	p, err := OS{}.Start(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 60 & wait"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if !groupGone(p.Pid()) {
		t.Fatalf("process group %d still alive", p.Pid())
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestOSStopEscalatesToKill(t *testing.T) {
	p, err := OS{}.Start(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "trap '' TERM; sleep 60 & wait"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 2*time.Second || d > 6*time.Second {
		t.Fatalf("Stop took %v, want about 3s", d)
	}
	if !groupGone(p.Pid()) {
		t.Fatal("group survived SIGKILL")
	}
}

func TestOSRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := OS{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 60 & wait"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Run took %v", d)
	}
}

func TestOSCmdline(t *testing.T) {
	got, err := OS{}.Cmdline(os.Getpid())
	if err != nil || got == "" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := (OS{}).Cmdline(1 << 30); err == nil {
		t.Fatal("want error for a missing pid")
	}
}

func TestFakeScripts(t *testing.T) {
	f := &Fake{}
	f.On("psql", func(c Cmd) bool { return strings.Contains(c.Stdin, "bad") }, FakeReply{Stderr: "nope", Err: &ExitError{Code: 2, LastStderr: "nope"}})
	f.On("psql", nil, FakeReply{Stdout: "ok"})
	f.On("ssh", nil, FakeReply{Pid: 77})
	f.SetPath("psql", "/bin/psql")
	f.SetCmdline(77, "ssh -L 1:x")

	out, _, err := f.Run(context.Background(), Cmd{Name: "psql", Env: []string{"PGPASSWORD=s"}, Stdin: "select 1"})
	if err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, _, err := f.Run(context.Background(), Cmd{Name: "psql", Stdin: "bad"}); err == nil {
		t.Fatal("want scripted error")
	}
	if _, _, err := f.Run(context.Background(), Cmd{Name: "mongosh"}); err == nil {
		t.Fatal("want unscripted error")
	}
	p, err := f.Start(context.Background(), Cmd{Name: "ssh"})
	if err != nil || p.Pid() != 77 {
		t.Fatalf("p=%v err=%v", p, err)
	}
	done := make(chan error)
	go func() { done <- p.Wait() }()
	_ = p.Stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !f.Processes()[0].Stopped() {
		t.Fatal("not stopped")
	}
	if got, _ := f.LookPath("psql"); got != "/bin/psql" {
		t.Fatalf("got %q", got)
	}
	if _, err := f.LookPath("nope"); err == nil {
		t.Fatal("want error")
	}
	if l, _ := f.Cmdline(77); l != "ssh -L 1:x" {
		t.Fatalf("got %q", l)
	}
	calls := f.Calls()
	if len(calls) != 4 || calls[0].Env[0] != "PGPASSWORD=s" || calls[1].Stdin != "bad" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestFakeConcurrent(t *testing.T) {
	f := &Fake{}
	f.On("x", nil, FakeReply{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = f.Run(context.Background(), Cmd{Name: "x"})
			_ = f.Calls()
		}()
	}
	wg.Wait()
	if len(f.Calls()) != 20 {
		t.Fatal("lost calls")
	}
}

func TestOSStartWaitKeepsLastStderrLine(t *testing.T) {
	p, err := OS{}.Start(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "head -c 9000 /dev/zero | tr '\\0' x >&2; echo >&2; echo 'Permission denied (publickey).' >&2; exit 255"}})
	if err != nil {
		t.Fatal(err)
	}
	var ee *ExitError
	if err := p.Wait(); !errors.As(err, &ee) || ee.Code != 255 || ee.LastStderr != "Permission denied (publickey)." {
		t.Fatalf("err=%v", err)
	}
}

func TestTailWriterKeepsLastBytes(t *testing.T) {
	w := &tailWriter{max: 4}
	_, _ = w.Write([]byte("abc"))
	_, _ = w.Write([]byte("defg"))
	if got := w.String(); got != "defg" {
		t.Fatalf("got %q", got)
	}
}

// Unset drops parent variables from the child's environment; Env still adds.
func TestOSRunUnset(t *testing.T) {
	t.Setenv("RM_TEST_PARENT", "p1")
	t.Setenv("RM_TEST_KEEP", "k1")
	out, _, err := OS{}.Run(context.Background(), Cmd{
		Name:  "sh",
		Args:  []string{"-c", `printf '%s|%s|%s' "${RM_TEST_PARENT-unset}" "$RM_TEST_KEEP" "$RM_TEST_ADDED"`},
		Env:   []string{"RM_TEST_ADDED=a1"},
		Unset: []string{"RM_TEST_PARENT"},
	})
	if err != nil || out != "unset|k1|a1" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	f := &Fake{}
	f.On("x", nil, FakeReply{})
	c := Cmd{Name: "x", Unset: []string{"A"}}
	_, _, _ = f.Run(context.Background(), c)
	c.Unset[0] = "B"
	if got := f.Calls()[0].Unset; len(got) != 1 || got[0] != "A" {
		t.Fatalf("fake recorded %q", got)
	}
}

// The Railway tokens reach the railway CLI only.
func TestOSRunRailwayTokensOnlyToRailway(t *testing.T) {
	t.Setenv("RAILWAY_API_TOKEN", "api1")
	t.Setenv("RAILWAY_TOKEN", "proj1")
	t.Setenv("RM_TEST_KEEP", "k1")
	script := `printf '%s|%s|%s' "${RAILWAY_API_TOKEN-unset}" "${RAILWAY_TOKEN-unset}" "$RM_TEST_KEEP"`
	out, _, err := OS{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", script}, Unset: []string{"X"}})
	if err != nil || out != "unset|unset|k1" {
		t.Fatalf("sh got out=%q err=%v", out, err)
	}
	dir := t.TempDir()
	railway := filepath.Join(dir, "railway")
	if err := os.WriteFile(railway, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"railway", railway} {
		out, _, err := OS{}.Run(context.Background(), Cmd{Name: name})
		if err != nil || out != "api1|proj1|k1" {
			t.Fatalf("%s got out=%q err=%v", name, out, err)
		}
	}
}

// A ps that hangs is cut off after its timeout: the command line is
// unknown, an error (M4).
func TestPSTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ps")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldName, oldTimeout := psName, psTimeout
	t.Cleanup(func() { psName, psTimeout = oldName, oldTimeout })
	psName, psTimeout = script, 200*time.Millisecond
	begin := time.Now()
	if _, err := psOutput("-o", "command=", "-p", "1"); err == nil {
		t.Fatal("a hanging ps gave output")
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("ps ran %v, want cut at its timeout", d)
	}
}

// ps gets no Railway token.
func TestPSNoToken(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ps")
	body := "#!/bin/sh\n[ -n \"$RAILWAY_API_TOKEN$RAILWAY_TOKEN\" ] && exit 7\necho ok\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	oldName := psName
	t.Cleanup(func() { psName = oldName })
	psName = script
	t.Setenv("RAILWAY_API_TOKEN", "fixture-token-a")
	t.Setenv("RAILWAY_TOKEN", "fixture-token-b")
	if out, err := psOutput("-p", "1"); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("ps saw a token: %q, %v", out, err)
	}
}
