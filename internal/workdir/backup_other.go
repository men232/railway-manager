//go:build !darwin

package workdir

// excludeFromBackup does nothing: there is no common backup exclusion
// outside macOS.
func excludeFromBackup(string) error { return nil }
