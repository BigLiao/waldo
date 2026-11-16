package proto

import (
	"syscall"
	"testing"
)

func TestWireSignalsUseLinuxNumbers(t *testing.T) {
	for _, tc := range []struct {
		local syscall.Signal
		wire  int
	}{
		{syscall.SIGUSR1, 10}, {syscall.SIGUSR2, 12}, {syscall.SIGWINCH, 28},
		{syscall.SIGTERM, 15}, {syscall.SIGCHLD, 17}, {syscall.SIGCONT, 18}, {syscall.SIGSTOP, 19},
	} {
		if got := EncodeSignal(tc.local); got != tc.wire {
			t.Errorf("encode %s=%d want %d", tc.local, got, tc.wire)
		}
		if got, ok := DecodeSignal(tc.wire); !ok || got != tc.local {
			t.Errorf("decode %d=%s,%v", tc.wire, got, ok)
		}
	}
	for _, bad := range []int{0, -1, 65, 255} {
		if _, ok := DecodeSignal(bad); ok {
			t.Errorf("accepted invalid signal %d", bad)
		}
	}
}
