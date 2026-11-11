// Package sandbox builds the A-side sandbox. The backend is an interface so
// that other platforms (seatbelt on macOS, a VM) can be added later; v1 ships
// bubblewrap on Linux.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// InnerExe is where the waldo binary appears inside the sandbox.
const InnerExe = "/run/waldo/waldo"

// Spec is the input of a sandbox backend.
type Spec struct {
	Home        string   // HOME path string (same as the host's)
	SandboxHome string   // persistent directory backing HOME inside the sandbox
	Project     string   // read-write project directory
	ExposeRO    []string // extra read-only paths (toolchains, user config)
	Exe         string   // waldo binary on the host
	Env         []string // initial environment of sandbox-serve
	ContextDir  string   // private host directory containing the session guide
}

// Backend starts a process that runs `waldo sandbox-serve` inside a sandbox.
type Backend interface {
	Command(spec Spec) (*exec.Cmd, error)
}

// Default returns the backend for this platform.
func Default() Backend { return Bwrap{} }

// Toolchains lists home-relative directories that are mounted read-only when
// they exist. Directories that tools want to write to (module caches,
// registries) are deliberately not listed so they land in the sandbox HOME.
var Toolchains = []string{
	".nvm", ".volta", ".fnm", ".bun/bin", ".deno/bin",
	".cargo/bin", ".rustup",
	"go/bin", "sdk", ".local/go",
	".local/share/mise", ".local/share/rtx", ".asdf", ".pyenv", ".rbenv", ".nodenv", ".goenv",
	".local/share/pnpm", ".sdkman", ".local/bin",
}

// DetectToolchains returns the toolchain directories that exist under home.
func DetectToolchains(home string) []string {
	var out []string
	for _, t := range Toolchains {
		p := filepath.Join(home, t)
		if util.IsDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// ProtectedNames are project paths made read-only: things the host runs
// automatically later (git hooks/config, direnv, editor tasks).
var ProtectedNames = []string{".git/hooks", ".git/config", ".git/info", ".envrc", ".env.local.sh", ".vscode", ".idea", ".devcontainer"}

// Protected returns the protected paths under project that currently exist.
func Protected(project string) []string {
	var out []string
	for _, n := range ProtectedNames {
		p := filepath.Join(project, n)
		if util.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// Bwrap is the bubblewrap backend.
type Bwrap struct{}

func (Bwrap) Command(s Spec) (*exec.Cmd, error) {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("找不到 bwrap（bubblewrap），请先安装：%w", err)
	}
	args, err := BwrapArgs(s)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bw, args...)
	cmd.Env = []string{}
	return cmd, nil
}

// BwrapArgs builds the bubblewrap command line (without the binary name).
func BwrapArgs(s Spec) ([]string, error) {
	if s.Project == "" || s.Home == "" || s.SandboxHome == "" || s.Exe == "" {
		return nil, fmt.Errorf("sandbox: incomplete spec")
	}
	a := []string{
		"--unshare-all", // user, ipc, pid, net, uts, cgroup
		"--die-with-parent",
		"--new-session",
		"--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr",
	}
	for _, d := range []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"} {
		st, err := os.Lstat(d)
		if err != nil {
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			t, err := os.Readlink(d)
			if err == nil {
				a = append(a, "--symlink", t, d)
			}
		} else if st.IsDir() {
			a = append(a, "--ro-bind", d, d)
		}
	}
	a = append(a,
		"--ro-bind", "/etc", "/etc",
		"--ro-bind-try", "/opt", "/opt",
		"--ro-bind-try", "/nix", "/nix",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/var/tmp",
		"--tmpfs", "/run",
		"--bind", s.SandboxHome, s.Home,
	)
	// Read-only extras, outermost first so that nested paths stack correctly.
	ro := append([]string(nil), s.ExposeRO...)
	sort.Slice(ro, func(i, j int) bool { return depth(ro[i]) < depth(ro[j]) })
	for _, p := range ro {
		if util.Within(s.Project, p) || p == "/" {
			continue // never shadow the project with a read-only parent
		}
		if util.Exists(p) {
			a = append(a, "--ro-bind", p, p)
		}
	}
	a = append(a, "--bind", s.Project, s.Project)
	// Binding .git onto itself makes it a mount point, so it cannot be renamed
	// away and replaced by a directory with fresh hooks.
	if util.IsDir(filepath.Join(s.Project, ".git")) {
		a = append(a, "--bind", filepath.Join(s.Project, ".git"), filepath.Join(s.Project, ".git"))
	}
	for _, p := range Protected(s.Project) {
		a = append(a, "--ro-bind", p, p)
	}
	if s.ContextDir != "" {
		a = append(a, "--ro-bind", s.ContextDir, proto.ContextDir)
	}
	a = append(a, "--ro-bind", s.Exe, InnerExe, "--chdir", s.Project, "--clearenv")
	for _, e := range s.Env {
		k, v, ok := strings.Cut(e, "=")
		if ok && k != "" {
			a = append(a, "--setenv", k, v)
		}
	}
	a = append(a, InnerExe, "sandbox-serve")
	return a, nil
}

func depth(p string) int { return strings.Count(filepath.Clean(p), "/") }
