//go:build !linux

package proc

import "syscall"

// sysProcAttr puts the child in its own process group.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
