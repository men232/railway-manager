// Package workdir manages the on-disk work directory, durable file writes and run locks.
package workdir

import (
	"os"
	"path/filepath"
)

// Dir returns the work directory: $RAILWAY_MANAGER_HOME or <home>/.railway-manager.
func Dir(getenv func(string) string, home string) string {
	if v := getenv("RAILWAY_MANAGER_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".railway-manager")
}

// Ensure creates dir (and parents) with mode 0700 and tightens an existing one to 0700.
func Ensure(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
