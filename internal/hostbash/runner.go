package hostbash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"waldo/internal/hosttools"
	"waldo/internal/util"
)

type routedRunner struct {
	observer           *executionObserver
	router             *pathRouter
	socket, shell, cwd string
	wg                 sync.WaitGroup
	mu                 sync.Mutex
	versions           map[string]string
	jobs               map[string]*job
	ctx                context.Context
	cancel             context.CancelFunc
}

func newRunner(router *pathRouter, socket, shell, cwd string) *routedRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &routedRunner{observer: newExecutionObserver(os.Getenv("WALDO_TOOL_OBSERVER")), router: router, socket: socket, shell: shell, cwd: cwd, versions: map[string]string{}, jobs: map[string]*job{}, ctx: ctx, cancel: cancel}
}
func (r *routedRunner) Close() { r.cancel(); r.wg.Wait(); r.observer.close() }
func (r *routedRunner) Run(ctx context.Context, c Call) (Result, error) {
	if c.Tool == "TaskOutput" || c.Tool == "TaskStop" {
		return r.task(ctx, c)
	}
	if c.Tool == "" && c.Background {
		return r.background(c)
	}
	return r.execute(ctx, c)
}
func (r *routedRunner) execute(ctx context.Context, c Call) (res Result, err error) {
	res.ExitCode = 255
	if c.DisableSandbox {
		return res, fmt.Errorf("dangerouslyDisableSandbox is unsupported: Waldo sandbox restrictions always apply")
	}
	side := "host"
	key := ""
	if c.Tool != "" {
		a, e := hosttools.Decode(c.Tool, c.Arguments)
		if e != nil {
			return res, e
		}
		path, selected, e := r.router.route(a.Path, a.Location, c.Tool == "Grep" || c.Tool == "Glob")
		if e != nil {
			return res, e
		}
		side = selected
		a.Path = path
		a.FilePath = ""
		a.NotebookPath = ""
		a.Location = ""
		key = side + ":" + path
		if c.Tool == "Edit" || c.Tool == "Write" || c.Tool == "NotebookEdit" {
			r.mu.Lock()
			a.SHA256 = r.versions[key]
			r.mu.Unlock()
			if c.Tool != "Write" && a.SHA256 == "" {
				return res, fmt.Errorf("Read this file in location=%s before editing", side)
			}
		}
		c.Arguments, _ = json.Marshal(a)
	} else if c.Location == "agent" {
		side = "agent"
	}
	started := time.Now()
	defer func() { r.observer.record(c.Tool, side, time.Since(started), err != nil || res.ExitCode != 0) }()
	if side == "host" {
		res, err = Execute(ctx, r.socket, r.shell, r.cwd, c)
	} else {
		res, err = r.local(ctx, c)
	}
	res.Location = side
	if err == nil && res.ExitCode == 0 && key != "" {
		var v struct {
			SHA256 string `json:"sha256"`
		}
		if json.Unmarshal([]byte(res.Stdout), &v) == nil && v.SHA256 != "" {
			r.mu.Lock()
			if len(r.versions) >= 4096 {
				for k := range r.versions {
					delete(r.versions, k)
					break
				}
			}
			r.versions[key] = v.SHA256
			r.mu.Unlock()
		}
	}
	return res, err
}

type outputBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if len(p) > left {
		b.truncated = true
		p = p[:left]
	}
	b.Buffer.Write(p)
	return n, nil
}
func (r *routedRunner) local(parent context.Context, c Call) (res Result, err error) {
	timeout := c.TimeoutMS
	if timeout == 0 {
		timeout = 120000
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	exe, e := os.Executable()
	if e != nil {
		return res, e
	}
	var cmd *exec.Cmd
	if c.Tool != "" {
		cmd = exec.CommandContext(ctx, exe, "host-file-tool", c.Tool, "--rg", "/usr/bin/rg")
		cmd.Stdin = bytes.NewReader(c.Arguments)
		cmd.Dir = "/"
	} else {
		cmd = exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", c.Command)
		cmd.Env = util.EnvFilter(os.Environ(), util.IsToolchainVar)
		cmd.Env = util.EnvSet(cmd.Env, "PATH", util.NativePath)
		cmd.Env = util.EnvSet(cmd.Env, "SHELL", "/bin/bash")
		cmd.Dir = r.cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
	outputLimit := 128 << 10
	if c.Tool != "" {
		outputLimit = 8 << 20
	}
	out := &outputBuffer{limit: outputLimit}
	stderr := &outputBuffer{limit: 128 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err = cmd.Run()
	res = Result{Stdout: out.String(), Stderr: stderr.String(), Truncated: out.truncated || stderr.truncated, ExitCode: 0, Location: "agent"}
	if ctx.Err() != nil {
		res.ExitCode = 255
		return res, ctx.Err()
	}
	if err != nil {
		if ex, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ex.ExitCode()
			return res, nil
		}
		res.ExitCode = 255
		return res, err
	}
	return res, nil
}
