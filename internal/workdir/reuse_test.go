package workdir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeLock writes a pid-only lock whose mtime is written.
func writeLock(t *testing.T, p string, pid int, written time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, written, written); err != nil {
		t.Fatal(err)
	}
}

// fakeStart makes every live pid look started at start.
func fakeStart(t *testing.T, start time.Time, err error) {
	t.Helper()
	restore := processStart
	t.Cleanup(func() { processStart = restore })
	processStart = func(int) (time.Time, error) { return start, err }
}

// A live pid counts as reused only when its process started more than five
// minutes after the lock was written; earlier, or within the margin (a
// clock step), it holds the lock, whatever its command line.
func TestLockReuseMargin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	parent := os.Getppid()
	written := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, c := range []struct {
		name    string
		started time.Time
		held    bool
	}{
		{"before", written.Add(-time.Minute), true},
		{"1 min after", written.Add(time.Minute), true},
		{"10 min after", written.Add(10 * time.Minute), false},
	} {
		fakeStart(t, c.started, nil)
		writeLock(t, p, parent, written)
		if pid, held := HeldByOther(p); held != c.held || pid != parent {
			t.Errorf("%s: HeldByOther = %d, %v, want held %v", c.name, pid, held, c.held)
		}
		l, err := AcquireLock(p)
		var le *LockedError
		switch {
		case c.held && !errors.As(err, &le):
			t.Errorf("%s: a holder was taken over: %v", c.name, err)
		case !c.held && err != nil:
			t.Errorf("%s: a reused pid was not taken over: %v", c.name, err)
		}
		if l != nil {
			_ = l.Release()
		}
	}
	// an unreadable start time: held
	fakeStart(t, time.Time{}, errors.New("no ps"))
	writeLock(t, p, parent, written)
	if _, err := AcquireLock(p); err == nil {
		t.Errorf("unreadable start time: taken over")
	}
}

// The real ps: a live process started long after the lock was written is
// taken over; one that started before it is never.
func TestLockReusedPIDRealPs(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	p := filepath.Join(t.TempDir(), "run.lock")
	writeLock(t, p, cmd.Process.Pid, time.Now().Add(-time.Hour))
	l, err := AcquireLock(p)
	if err != nil {
		t.Fatalf("reused pid not taken over: %v", err)
	}
	_ = l.Release()

	writeLock(t, p, os.Getppid(), time.Now())
	var le *LockedError
	if _, err := AcquireLock(p); !errors.As(err, &le) {
		t.Errorf("an earlier holder was taken over: %v", err)
	}
	// our own pid is never another holder
	writeLock(t, p, os.Getpid(), time.Now())
	if _, held := HeldByOther(p); held {
		t.Errorf("our own lock reads as another's")
	}
}

// ps prints lstart zoneless; it runs in UTC, so the time parses as UTC
// whatever the local zone (no DST fall-back ambiguity).
func TestParseLstartUTC(t *testing.T) {
	got, err := parseLstart("Sun Nov  1 01:30:00 2026\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("parseLstart = %v, want %v", got, want)
	}
}

// A new lock holds the pid only, as older binaries read it.
func TestLockPIDOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	l, err := AcquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if b, _ := os.ReadFile(p); string(b) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock = %q", b)
	}
}

// A ps that hangs is cut off after its timeout: the start time is unknown
// and the lock reads held (M4).
func TestProcessStartTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ps")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldName, oldTimeout := psName, psTimeout
	t.Cleanup(func() { psName, psTimeout = oldName, oldTimeout })
	psName, psTimeout = script, 200*time.Millisecond
	begin := time.Now()
	if _, err := processStart(os.Getppid()); err == nil {
		t.Fatal("a hanging ps gave a start time")
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("ps ran %v, want cut at its timeout", d)
	}
	p := filepath.Join(dir, "run.lock")
	writeLock(t, p, os.Getppid(), time.Now().Add(-time.Hour))
	if _, held := HeldByOther(p); !held {
		t.Error("an unknown start time did not read held")
	}
}

// ps gets no Railway token: it needs none, and a child is handed only what
// it needs.
func TestProcessStartNoToken(t *testing.T) {
	script := filepath.Join(t.TempDir(), "ps")
	body := "#!/bin/sh\n[ -n \"$RAILWAY_API_TOKEN$RAILWAY_TOKEN\" ] && exit 7\necho 'Mon Oct 26 10:00:00 2026'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	oldName := psName
	t.Cleanup(func() { psName = oldName })
	psName = script
	t.Setenv("RAILWAY_API_TOKEN", "fixture-token-a")
	t.Setenv("RAILWAY_TOKEN", "fixture-token-b")
	if _, err := processStart(os.Getpid()); err != nil {
		t.Errorf("ps saw a token: %v", err)
	}
}
