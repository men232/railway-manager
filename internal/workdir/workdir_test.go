package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestDirOverride(t *testing.T) {
	get := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if got := Dir(get(map[string]string{"RAILWAY_MANAGER_HOME": "/x"}), "/home/u"); got != "/x" {
		t.Errorf("override: got %q", got)
	}
	if got := Dir(get(nil), "/home/u"); got != "/home/u/.railway-manager" {
		t.Errorf("default: got %q", got)
	}
}

func TestEnsureMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w")
	if err := Ensure(p); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	// existing dir with loose mode is tightened
	q := filepath.Join(t.TempDir(), "e")
	if err := os.Mkdir(q, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(q); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(q)
	if st.Mode().Perm() != 0o700 {
		t.Errorf("existing mode %v", st.Mode().Perm())
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.json")
	if err := WriteFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one" {
		t.Errorf("content %q", b)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Errorf("leftover tmp: %v", m)
	}

	// Interrupted write: directory not writable, original survives.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := WriteFileAtomic(p, []byte("two"), 0o600); err == nil {
		t.Error("expected error")
	}
	b, _ = os.ReadFile(p)
	if string(b) != "one" {
		t.Errorf("original clobbered: %q", b)
	}
}

func TestLockDeadPID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(p, []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock holds %q", b)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock file not removed: %v", err)
	}
}

func TestLockGarbagePID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(p, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock holds %q", b)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("lock file not removed: %v", err)
	}
}

func TestLockLivePIDOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(p, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := AcquireLock(p)
	var le *LockedError
	if !errors.As(err, &le) || le.PID != 1 {
		t.Fatalf("want LockedError pid 1, got %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "1" {
		t.Errorf("lock replaced: %q", b)
	}
}

func TestLockConcurrentStaleTakeover(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(p, []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, locked := 0, 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := AcquireLock(p)
			mu.Lock()
			defer mu.Unlock()
			var le *LockedError
			switch {
			case err == nil && l != nil:
				wins++
			case errors.As(err, &le):
				locked++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 || locked != n-1 {
		t.Errorf("wins=%d locked=%d", wins, locked)
	}
	if b, _ := os.ReadFile(p); string(b) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock holds %q", b)
	}
}

func TestLockLivePID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.lock")
	l, err := AcquireLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, err = AcquireLock(p)
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("want *LockedError, got %v", err)
	}
	if le.PID != os.Getpid() || le.Path != p {
		t.Errorf("got %+v", le)
	}
}
