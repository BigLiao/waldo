package host

import "syscall"

// B runs only on Linux. This exists so the shared command builds on Darwin.
func sshfsProcessAttrs() *syscall.SysProcAttr { return nil }
