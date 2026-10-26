package changeset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/workdir"
)

// CorruptError reports that the change set file could not be parsed and was
// moved aside; Load returns an empty set with it.
type CorruptError struct {
	MovedTo string
}

func (e *CorruptError) Error() string {
	return "change set file is corrupt, moved to " + e.MovedTo
}

var quarantineNow = time.Now

// quarantine moves path to <path>.corrupt-<time>[-n] without ever overwriting
// an existing file: os.Link fails when the target exists.
func quarantine(path string) (string, error) {
	base := path + ".corrupt-" + quarantineNow().Format("20060102-150405")
	target := base
	for n := 1; ; n++ {
		err := os.Link(path, target)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return target, nil
}

// PruneCorrupt removes the quarantined copies of path (<path>.corrupt-*)
// last written more than age before now: they hold the staged secret values
// of a change set that could not be read. Only regular files go; a symlink
// is left alone. It returns how many it removed.
func PruneCorrupt(path string, age time.Duration, now time.Time) (int, error) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".corrupt-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < age {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Load reads the change set. A missing file gives an empty set; an
// unparseable one is renamed to <path>.corrupt-<time> and an empty set is
// returned with *CorruptError.
func Load(path string) (ChangeSet, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ChangeSet{}, nil
	}
	if err != nil {
		return ChangeSet{}, err
	}
	var s ChangeSet
	if err := json.Unmarshal(data, &s); err != nil {
		moved, rerr := quarantine(path)
		if rerr != nil {
			return ChangeSet{}, fmt.Errorf("change set corrupt and could not be moved: %w", rerr)
		}
		return ChangeSet{}, &CorruptError{MovedTo: moved}
	}
	return s, nil
}

// Save writes the change set atomically with mode 0600.
func Save(path string, s ChangeSet) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return workdir.WriteFileAtomic(path, data, 0o600)
}
