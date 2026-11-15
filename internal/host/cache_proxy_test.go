package host

import (
	"io"
	"os/exec"
	"testing"
	"time"
)

func TestCachePipeDoesNotHoldProcessWaitOpen(t *testing.T) {
	stdin, stdout, proxy, err := cacheProcessPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdout.Close()
	defer proxy.Close()
	cmd := exec.Command("sh", "-c", "printf ready")
	cmd.Stdin, cmd.Stdout = stdin, stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	stdout.Close()
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process Wait depends on closing the open SFTP proxy")
	}
	data, err := io.ReadAll(proxy)
	if err != nil || string(data) != "ready" {
		t.Fatalf("process output: %q, %v", data, err)
	}
}

func TestProxyMountHasNoIndependentKernelCache(t *testing.T) {
	options := sshfsProxyCacheOptions()
	want := map[string]bool{
		"dir_cache=no": true, "attr_timeout=0": true, "entry_timeout=0": true,
		"negative_timeout=0": true, "direct_io": true,
	}
	for _, option := range options {
		if !want[option] {
			t.Errorf("unexpected independent cache option %q", option)
		}
		delete(want, option)
	}
	if len(want) != 0 {
		t.Errorf("missing options that prevent stale kernel results: %v", want)
	}
}
