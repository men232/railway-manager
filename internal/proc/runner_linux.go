package proc

import "syscall"

// sysProcAttr puts the child in its own process group, and has the kernel
// send it SIGTERM when our thread that started it dies, so a killed app
// leaves no tunnel behind.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
