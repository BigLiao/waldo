package host

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"waldo/internal/hostbash"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// configureExecMode does not grant any new access to A: host_bash speaks the
// same exec protocol as a command stub and runs under the same A sandbox.
func (h *host) configureExecMode() error {
	if h.hello.ExecMode == "" {
		h.hello.ExecMode = proto.ExecHost
	}
	if h.hello.ExecMode == proto.ExecHost {
		return nil
	}
	if h.hello.ExecMode != proto.ExecRemote {
		return fmt.Errorf("invalid exec mode %q", h.hello.ExecMode)
	}
	if h.hello.Profile.MCPConfigFlag != "--mcp-config" {
		return fmt.Errorf("remote execution requires an MCP-capable profile")
	}
	if !util.Exists("/bin/bash") {
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
		return fmt.Errorf("host_bash requires bash on A's sandbox PATH")
	}
	cwd := h.hello.Cwd
	if cwd == "" {
		cwd = h.hello.Project
	}
	args := append(append([]string(nil), h.hello.Profile.Args...), h.hello.Argv...)
	h.hello.Profile.Args, h.hello.Argv = nil, hostbash.Inject(args, shell, cwd)
	log.Printf("session %s: exec_mode=remote; host_bash uses existing sandbox channel", h.hello.Session)
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
	switch k {
	case "PATH", "SHELL", "NVM_DIR", "NVM_BIN", "NVM_INC", "VOLTA_HOME", "PNPM_HOME", "BUN_INSTALL", "RUSTUP_HOME", "GOROOT", "PYENV_ROOT", "JAVA_HOME", "SDKMAN_DIR", "MISE_DATA_DIR", "ASDF_DIR", "ASDF_DATA_DIR":
		return true
	}
	return strings.HasPrefix(k, "WALDO_")
}
