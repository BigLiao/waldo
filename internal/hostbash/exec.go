package hostbash

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"waldo/internal/hosttools"

	"waldo/internal/proto"
	"waldo/internal/stub"
)

// Main runs only as an explicitly injected MCP subprocess inside B's capsule.
func Main(args []string) int {
	fs := flag.NewFlagSet("host-bash-mcp", flag.ContinueOnError)
	shell := fs.String("shell", "", "absolute bash path on A")
	cwd := fs.String("cwd", "", "initial working directory on A")
	files := fs.Bool("file-tools", false, "expose host file tools")
	if fs.Parse(args) != nil || fs.NArg() != 0 || !filepath.IsAbs(*shell) || !filepath.IsAbs(*cwd) {
		return 2
	}
	router, err := newRouter(*cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runner := newRunner(router, stub.Socket, *shell, *cwd)
	defer runner.Close()
	err = Serve(context.Background(), os.Stdin, os.Stdout, runner.Run, *files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo host MCP:", err)
		return 1
	}
	return 0
}

// Execute reuses the stub socket and framed exec protocol. Closing that socket
// on timeout/cancellation kills the command's process group on A.
func Execute(parent context.Context, socket, shell, cwd string, call Call) (res Result, err error) {
	res.ExitCode = 255
	if (call.Tool == "" && !validCall(call)) || (call.Tool != "" && !hosttools.Known(call.Tool)) {
		return res, fmt.Errorf("invalid Bash arguments")
	}
	if call.Cwd != "" {
		cwd = call.Cwd
	}
	timeout := call.TimeoutMS
	if timeout == 0 {
		timeout = 120000
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return res, fmt.Errorf("A command channel unavailable: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req := proto.ExecReq{Tool: ToolName, Path: shell, Argv: []string{shell, "--noprofile", "--norc", "-c", call.Command}, Cwd: cwd}
	if call.Tool != "" {
		if _, err = hosttools.Decode(call.Tool, call.Arguments); err != nil {
			return res, err
		}
		req.Tool = call.Tool
		req.Path = "/run/waldo/waldo"
		req.Argv = []string{req.Path, "host-file-tool", call.Tool}
	}
	if err = proto.WriteMsg(conn, proto.Header{Kind: proto.KindExec, Exec: &req}); err != nil {
		return res, err
	}
	fw := proto.NewFrameWriter(conn)
	for data := []byte(call.Arguments); len(data) > 0; {
		n := min(len(data), 32<<10)
		if err = fw.Write(proto.FStdin, data[:n]); err != nil {
			return res, err
		}
		data = data[n:]
	}
	if err = fw.Write(proto.FStdinEOF, nil); err != nil {
		return res, err
	}
	var stdout, stderr strings.Builder
	defer func() { res.Stdout, res.Stderr = stdout.String(), stderr.String() }()
	appendOutput := func(b *strings.Builder, p []byte) {
		capacity := 128 << 10
		if call.Tool != "" {
			capacity = 8 << 20
		}
		remaining := capacity - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
			res.Truncated = true
		}
		b.Write(p)
	}
	res.ExitCode = 255
	for {
		typ, payload, readErr := proto.ReadFrame(conn)
		if readErr != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			return res, fmt.Errorf("A command channel closed before exit: %w", readErr)
		}
		switch typ {
		case proto.FStdout:
			appendOutput(&stdout, payload)
		case proto.FStderr, proto.FError:
			appendOutput(&stderr, payload)
		case proto.FExit:
			var exit proto.Exit
			if err := proto.DecodeJSON(payload, &exit); err != nil {
				return res, err
			}
			res.ExitCode, res.Signal = exit.Code, exit.Signal
			return res, nil
		}
	}
}
