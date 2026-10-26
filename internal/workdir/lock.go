//go:build darwin || linux

package workdir

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LockedError reports that another live process holds the lock.
type LockedError struct {
	PID  int
	Path string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("already running (pid %d, lock %s)", e.PID, e.Path)
}

// Lock is a held pid lock file.
type Lock struct {
	path string
	pid  int
}

// AcquireLock publishes a lock file that already contains our pid (temp file,
// fsync, then link), so the lock is never observed empty. A stale lock (dead or
// corrupt pid) is taken over under a kernel flock guard, by renaming it aside
// rather than removing the shared path. A live owner always yields *LockedError.
func AcquireLock(path string) (*Lock, error) {
	pid := os.Getpid()
	for attempt := 0; attempt < 5; attempt++ {
		err := publish(path, pid)
		if err == nil {
			return &Lock{path: path, pid: pid}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if p, alive := holder(path); alive {
			return nil, &LockedError{PID: p, Path: path}
		}
		if err := takeOver(path); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("lock %s: could not clear stale lock", path)
}

// publish atomically creates path with content pid; fails with ErrExist if
// present. The temp file is written right before the link and never
// rewritten, so the lock's mtime is when it was written (holder).
func publish(path string, pid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(strconv.Itoa(pid))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Link(tmp, path)
}

// takeOver clears a stale lock. Takeovers are serialized by a flock on
// path+".guard" (released by the kernel if we die), and the stale file is
// renamed aside and re-checked, never removed in place.
func takeOver(path string) error {
	g, err := os.OpenFile(path+".guard", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer g.Close()
	if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(g.Fd()), syscall.LOCK_UN)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // gone already; never rename a path that may be a fresh lock
	}
	judged, alive := holder(path)
	if alive {
		return &LockedError{PID: judged, Path: path}
	}
	var r [6]byte
	rand.Read(r[:])
	aside := fmt.Sprintf("%s.stale.%d.%x", path, os.Getpid(), r)
	if err := os.Rename(path, aside); err != nil {
		if os.IsNotExist(err) {
			return nil // already gone; caller retries
		}
		return err
	}
	if p, alive := holder(aside); alive || p != judged {
		// Moved something other than what we judged; put it back if we can.
		if alive {
			os.Link(aside, path)
			os.Remove(aside)
			return &LockedError{PID: p, Path: path}
		}
	}
	return os.Remove(aside)
}

// reuseMargin is how much later than the lock's write time a live
// process must have started for its pid to count as reused. The wall clock
// can step (NTP, a VM resumed, a hand-set clock) between the lock's write
// and the process start reading; a margin toward "held" limits the risk of
// taking over a live holder (the run would execute twice) — it does not
// remove it: on Linux, procps derives lstart from the boot time plus the
// process's start ticks, so a forward step larger than the margin after
// the lock was written still makes a live holder look started later. The
// cost on the other side is a stale lock whose pid was reused within
// minutes staying held.
const reuseMargin = 5 * time.Minute

// holder returns the pid in the lock file and whether that process holds
// it: alive, and not a reused pid. A pid is reused only when its process
// started more than reuseMargin after the lock was written (the lock
// file's mtime); when either time cannot be read the live process holds
// it. An unreadable or invalid pid counts as not alive.
func holder(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	err = syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return pid, false // ESRCH or anything else: treat as dead
	}
	if pid == os.Getpid() {
		return pid, true
	}
	st, err := os.Stat(path)
	if err != nil {
		return pid, true
	}
	started, err := processStart(pid)
	if err != nil {
		return pid, true
	}
	return pid, !started.After(st.ModTime().Add(reuseMargin))
}

// psName and psTimeout are the ps processStart runs and how long it may
// take; tests replace them.
var (
	psName    = "ps"
	psTimeout = 5 * time.Second
)

// processStart is when process pid started, to the second (ps lstart). ps
// runs in UTC so the zoneless time it prints is never ambiguous (a DST
// fall-back repeats an hour of local time). A ps that does not answer within
// psTimeout is an error: the start is unknown and the lock reads held.
var processStart = func(pid int) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psName, "-o", "lstart=", "-p", strconv.Itoa(pid))
	// Only what ps needs: never a token of ours.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "LANG=C", "TZ=UTC0"}
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	return parseLstart(string(out))
}

// parseLstart parses ps's lstart column printed in UTC.
func parseLstart(s string) (time.Time, error) {
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.UTC)
}

// HeldByOther reports whether another live process (not this one) holds
// the lock at path (holder), and its pid.
func HeldByOther(path string) (pid int, held bool) {
	pid, alive := holder(path)
	return pid, alive && pid != os.Getpid()
}

// Release removes the lock file if it still holds our pid.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	if pid, _ := holder(l.path); pid != l.pid {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
