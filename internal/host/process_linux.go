package host

import "syscall"

func sshfsProcessAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
