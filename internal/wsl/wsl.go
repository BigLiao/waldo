// Package wsl provides Windows-host integration for a Linux A running in WSL2.
package wsl

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func IsWSL2() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft-standard-wsl2")
}

// NetworkMode reports what WSL actually started, including fallback to none.
func NetworkMode() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/wslinfo", "--networking-mode").Output()
	if err == nil {
		switch mode := strings.TrimSpace(string(out)); mode {
		case "nat", "mirrored", "virtioproxy", "consomme", "bridged", "none":
			return mode
		}
	}
	return "unknown"
}
