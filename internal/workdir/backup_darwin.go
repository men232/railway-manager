package workdir

import "golang.org/x/sys/unix"

// backupExclude is the binary plist of the string "com.apple.backupd", the
// value `tmutil addexclusion` writes.
var backupExclude = []byte("bplist00_\x10\x11com.apple.backupd\x08\x00\x00\x00\x00\x00\x00\x01\x01\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x1c")

// excludeFromBackup marks dir as excluded from Time Machine.
func excludeFromBackup(dir string) error {
	return unix.Setxattr(dir, "com.apple.metadata:com_apple_backup_excludeItem", backupExclude, 0)
}
