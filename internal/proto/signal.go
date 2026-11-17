package proto

import (
	"runtime"
	"syscall"
)

// Wire signals use Linux numbers, independent of the sender's OS.
var portableSignals = map[int]syscall.Signal{
	1: syscall.SIGHUP, 2: syscall.SIGINT, 3: syscall.SIGQUIT,
	4: syscall.SIGILL, 5: syscall.SIGTRAP, 6: syscall.SIGABRT,
	7: syscall.SIGBUS, 8: syscall.SIGFPE, 9: syscall.SIGKILL,
	10: syscall.SIGUSR1, 11: syscall.SIGSEGV, 12: syscall.SIGUSR2,
	13: syscall.SIGPIPE, 14: syscall.SIGALRM, 15: syscall.SIGTERM,
	17: syscall.SIGCHLD, 18: syscall.SIGCONT, 19: syscall.SIGSTOP,
	20: syscall.SIGTSTP, 21: syscall.SIGTTIN, 22: syscall.SIGTTOU,
	23: syscall.SIGURG, 24: syscall.SIGXCPU, 25: syscall.SIGXFSZ,
	26: syscall.SIGVTALRM, 27: syscall.SIGPROF, 28: syscall.SIGWINCH,
	29: syscall.SIGIO, 31: syscall.SIGSYS,
}

// EncodeSignal returns zero for signals with no supported wire representation.
func EncodeSignal(sig syscall.Signal) int {
	if runtime.GOOS == "linux" {
		if sig > 0 && sig <= 64 {
			return int(sig)
		}
		return 0
	}
	for wire, local := range portableSignals {
		if local == sig {
			return wire
		}
	}
	return 0
}

// DecodeSignal never treats an unknown wire number as a Darwin signal.
func DecodeSignal(wire int) (syscall.Signal, bool) {
	if runtime.GOOS == "linux" {
		return syscall.Signal(wire), wire > 0 && wire <= 64
	}
	sig, ok := portableSignals[wire]
	return sig, ok
}
