package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"waldo/internal/util"
)

// Probe checks enforcement rather than just the existence of an executable.
func Probe() error {
	if runtime.GOOS != "darwin" {
		out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "true").CombinedOutput()
		if err != nil {
			return fmt.Errorf("bwrap: %w: %s", err, out)
		}
		return nil
	}
	root, err := os.MkdirTemp("", "waldo-doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	for _, sub := range []string{"project/.git", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0700); err != nil {
			return err
		}
	}
	secret := filepath.Join(root, "secret")
	protected := filepath.Join(root, "project/.git/config")
	if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(protected, []byte("protected"), 0600); err != nil {
		return err
	}
	spec := Spec{Project: filepath.Join(root, "project"), SandboxHome: filepath.Join(root, "home"), TempDir: filepath.Join(root, "tmp"), Exe: "/bin/sh", ProxyPort: 39339}
	policy, err := SeatbeltProfile(spec)
	if err != nil {
		return err
	}
	script := "printf ok > " + util.ShellQuote(filepath.Join(spec.Project, "probe")) + " && ! /bin/cat " + util.ShellQuote(secret) + " 2>/dev/null && ! /bin/sh -c " + util.ShellQuote("printf changed > "+util.ShellQuote(protected)) + " 2>/dev/null"
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/sh", "-c", script)
	cmd.Dir = spec.Project
	cmd.Env = []string{"HOME=" + spec.SandboxHome, "PATH=/usr/bin:/bin", "TMPDIR=" + spec.TempDir}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Seatbelt enforcement probe: %w: %s", err, out)
	}
	return nil
}
