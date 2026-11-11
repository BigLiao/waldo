package session

import (
	"fmt"
	"os"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/stub"
)

// DevSandbox runs one command inside the A sandbox, with the same policy as
// a real session but without B. It exists for debugging and for the sandbox
// regression tests (test/attack).
func DevSandbox(cfg *config.Config, argv []string) int {
	if len(argv) == 0 {
		warnf("用法: waldo dev-sandbox -- <命令> [参数...]")
		return 2
	}
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	project := ProjectDir(cwd)
	if err := CheckProject(project, home); err != nil {
		warnf("%v", err)
		return 2
	}
	pol, err := cfg.PolicyFor(project)
	if err != nil {
		warnf("%v", err)
		return 2
	}
	dataDir := config.DataDir()
	id := newID()
	al, err := audit.Open(dataDir, id)
	if err != nil {
		warnf("%v", err)
		return 1
	}
	defer al.Close()
	snap := sandbox.TakeSnapshot(project)
	sb, err := StartSandbox(SandboxOptions{Home: home, Project: project, Policy: pol, Audit: al, DataDir: dataDir,
		OnPorts: func(p []int) {
			if os.Getenv("WALDO_DEBUG_PORTS") == "1" {
				fmt.Fprintf(os.Stderr, "waldo: forwarded ports %v\n", p)
			}
		}})
	if err != nil {
		warnf("%v", err)
		return 1
	}
	defer sb.Close()
	path := sb.LookPath(argv[0])
	if path == "" {
		warnf("%s: 在沙箱的 PATH 中找不到", argv[0])
		return 127
	}
	st, err := sb.sess.OpenStream()
	if err != nil {
		warnf("%v", err)
		return 1
	}
	code := stub.Relay(st, &proto.ExecReq{Path: path, Argv: argv, Cwd: cwd, Env: sb.Facts.Env})
	sb.Close()
	reportProtected(snap, al)
	return code
}
