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

	"waldo/internal/proto"
	"waldo/internal/stub"
)

// Main runs only as an explicitly injected MCP subprocess inside B's capsule.
func Main(args []string) int {
	fs := flag.NewFlagSet("host-bash-mcp", flag.ContinueOnError)
	shell := fs.String("shell", "", "absolute bash path on A")
	cwd := fs.String("cwd", "", "initial working directory on A")
	if fs.Parse(args) != nil || fs.NArg() != 0 || !filepath.IsAbs(*shell) || !filepath.IsAbs(*cwd) {
		return 2
	}
	err := Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, call Call) (Result, error) {
		return Execute(ctx, stub.Socket, *shell, *cwd, call)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo host MCP:", err)
		return 1
	}
	return 0
}

// Execute reuses the stub socket and framed exec protocol. Closing that socket
// on timeout/cancellation kills the command's process group in A's sandbox.
func Execute(parent context.Context, socket, shell, cwd string, call Call) (res Result, err error) {
	res.ExitCode = 255
	if !validCall(call) {
		return res, fmt.Errorf("invalid host_bash arguments")
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
		return res, fmt.Errorf("A sandbox unavailable: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req := proto.ExecReq{Tool: ToolName, Path: shell, Argv: []string{shell, "--noprofile", "--norc", "-c", call.Command}, Cwd: cwd}
	if err = proto.WriteMsg(conn, proto.Header{Kind: proto.KindExec, Exec: &req}); err != nil {
		return res, err
	}
	if err = proto.NewFrameWriter(conn).Write(proto.FStdinEOF, nil); err != nil {
		return res, err
	}
	var stdout, stderr strings.Builder
	defer func() { res.Stdout, res.Stderr = stdout.String(), stderr.String() }()
	appendOutput := func(b *strings.Builder, p []byte) {
		remaining := (128 << 10) - b.Len()
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
