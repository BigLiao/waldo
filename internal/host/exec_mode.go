package host

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"waldo/internal/hostbash"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// configureExecMode does not grant any new access to A: Bash speaks the
// same exec protocol as a command stub and runs under the same A sandbox.
func (h *host) configureExecMode() error {
	if h.hello.ExecMode == "" {
		h.hello.ExecMode = proto.ExecHost
	}
	if h.hello.ExecMode == proto.ExecHost {
		return nil
	}
	if h.hello.ExecMode != proto.ExecRemote && h.hello.ExecMode != proto.ExecHostMCP {
		return fmt.Errorf("invalid exec mode %q", h.hello.ExecMode)
	}
	if h.hello.Profile.MCPConfigFlag != "--mcp-config" {
		return fmt.Errorf("MCP execution requires an MCP-capable profile")
	}
	if h.hello.ExecMode == proto.ExecRemote && !util.Exists("/bin/bash") {
		return fmt.Errorf("remote execution requires /bin/bash on B")
	}
	shell := ""
	for _, pd := range h.hello.Facts.PathDirs {
		if !pd.OnPath || !filepath.IsAbs(pd.Dir) {
			continue
		}
		for _, name := range pd.Names {
			if name == "bash" {
				shell = filepath.Join(pd.Dir, name)
				break
			}
		}
		if shell != "" {
			break
		}
	}
	if shell == "" {
		return fmt.Errorf("Bash requires bash on A's sandbox PATH")
	}
	cwd := h.hello.Cwd
	if cwd == "" {
		cwd = h.hello.Project
	}
	args := append(append([]string(nil), h.hello.Profile.Args...), h.hello.Argv...)
	h.hello.Profile.Args, h.hello.Argv = nil, hostbash.Inject(args, shell, cwd, h.hello.ExecMode == proto.ExecHostMCP)
	if h.hello.ExecMode == proto.ExecHostMCP {
		h.hello.Argv = hostbash.DisableHostTools(h.hello.Argv)
	}
	log.Printf("session %s: exec_mode=%s; Bash uses existing sandbox channel", h.hello.Session, h.hello.ExecMode)
	return nil
}

// remoteRuntimeArgs exports B's standard system tools, never its HOME or root.
// Preserve merged-/usr symlinks so absolute shell paths and PATH agree.
func remoteRuntimeArgs() []string {
	var args []string
	for _, p := range []string{"/usr/bin", "/usr/sbin", "/usr/include", "/usr/local", "/bin", "/sbin"} {
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(p); err == nil {
				args = append(args, "--symlink", target, p)
			}
		} else if st.IsDir() {
			args = append(args, "--ro-bind", p, p)
		}
	}
	return args
}

func remoteToolchainVar(k string) bool {
	return util.IsToolchainVar(k) || strings.HasPrefix(k, "WALDO_")
}

// Keep only Claude's startup Git probes forwarded in host-mcp mode. Native
// shells and their absolute shebang paths must agree with B's private /tmp.
func forwardPath(dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return fmt.Sprintf("/run/waldo/forward/%x", sum[:8])
}
