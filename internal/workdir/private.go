package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Private keeps dir out of indexes and backups, where the secret values it
// holds would outlive their files: Spotlight skips a directory holding
// .metadata_never_index, and on macOS Time Machine skips one marked with its
// exclusion attribute. Other backup tools are the user's to configure.
func Private(dir string) error {
	marker := filepath.Join(dir, ".metadata_never_index")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(f.Close(), excludeFromBackup(dir))
}

// CleanTemps removes the temporary files of WriteFileAtomic older than age
// in dir and its subdirectories two levels down (the runs): a crash between
// write and rename leaves one behind, a full copy of what it was writing.
// It walks dir itself, never a pattern built from its path (a "[" in it
// would match other directories). It returns how many it removed.
func CleanTemps(dir string, age time.Duration, now time.Time) (int, error) {
	var errs []error
	n := 0
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		entries, err := os.ReadDir(d)
		if err != nil {
			if d == dir {
				errs = append(errs, err)
			}
			return
		}
		for _, e := range entries {
			p := filepath.Join(d, e.Name())
			if e.IsDir() { // a symlink is not followed
				if depth < 2 {
					walk(p, depth+1)
				}
				continue
			}
			if !strings.HasSuffix(e.Name(), ".tmp") {
				continue
			}
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < age {
				continue
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	walk(dir, 0)
	return n, errors.Join(errs...)
}
