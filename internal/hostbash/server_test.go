package hostbash

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type rpcClient struct {
	net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	done chan error
}

func startRPC(t *testing.T, run Runner) *rpcClient {
	t.Helper()
	a, b := net.Pipe()
	a.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), b, b, run); b.Close() }()
	c := &rpcClient{a, json.NewEncoder(a), json.NewDecoder(a), done}
	t.Cleanup(func() {
		a.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("MCP did not stop on stdin EOF")
		}
	})
	return c
}

func (c *rpcClient) send(t *testing.T, id any, method string, params any) {
	t.Helper()
	r := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		r["id"] = id
	}
	if err := c.enc.Encode(r); err != nil {
		t.Fatal(err)
	}
}

func (c *rpcClient) receive(t *testing.T) map[string]any {
	t.Helper()
	var r map[string]any
	if err := c.dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMCPHandshakeDiscoveryAndToolResult(t *testing.T) {
	c := startRPC(t, func(ctx context.Context, call Call) (Result, error) {
		if call.Command != "exit 7" || call.Cwd != "/project" {
			t.Errorf("bad call: %+v", call)
		}
		return Result{Stdout: "out", Stderr: "err", ExitCode: 7}, nil
	})
	c.send(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	r := c.receive(t)["result"].(map[string]any)
	if r["protocolVersion"] != "2025-06-18" || r["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatal(r)
	}
	c.send(t, nil, "notifications/initialized", map[string]any{})
	c.send(t, 2, "tools/list", map[string]any{})
	tools := c.receive(t)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != ToolName {
		t.Fatal(tools)
	}
	c.send(t, 3, "tools/call", map[string]any{"name": ToolName, "arguments": Call{Command: "exit 7", Cwd: "/project"}})
	r = c.receive(t)["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	var result Result
	if json.Unmarshal([]byte(text), &result) != nil || result.ExitCode != 7 || result.Stdout != "out" || result.Stderr != "err" || r["isError"] != true {
		t.Fatalf("bad tool result: %v", r)
	}
	for _, arguments := range []any{map[string]any{"command": "id", "cwd": "relative"}, map[string]any{"command": "id", "timeout_ms": 600001}, map[string]any{"command": "id", "env": map[string]string{"SECRET": "no"}}, map[string]any{}} {
		c.send(t, 4, "tools/call", map[string]any{"name": ToolName, "arguments": arguments})
		if c.receive(t)["error"] == nil {
			t.Fatal("accepted invalid tool arguments")
		}
	}
}

func TestMCPCancellationAndEOFStopCommands(t *testing.T) {
	started := make(chan string, 2)
	c := startRPC(t, func(ctx context.Context, call Call) (Result, error) {
		started <- call.Command
		<-ctx.Done()
		return Result{ExitCode: 255}, ctx.Err()
	})
	c.send(t, "cancel-me", "tools/call", map[string]any{"name": ToolName, "arguments": Call{Command: "sleep"}})
	<-started
	c.send(t, nil, "notifications/cancelled", map[string]any{"requestId": "cancel-me"})
	result := c.receive(t)
	if result["result"].(map[string]any)["isError"] != true {
		t.Fatal(result)
	}
	c.send(t, "eof", "tools/call", map[string]any{"name": ToolName, "arguments": Call{Command: "wait"}})
	<-started
	c.Close()
	// Cleanup asserts Serve returns, which requires cancellation of the runner.
}

func TestMCPRejectsMalformedMessagesWithoutExecuting(t *testing.T) {
	var output strings.Builder
	err := Serve(context.Background(), strings.NewReader("{bad}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"unknown\"}\n"), &output, func(context.Context, Call) (Result, error) { t.Error("unexpected command"); return Result{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(strings.NewReader(output.String()))
	for _, code := range []float64{-32700, -32601} {
		var r map[string]any
		if err := d.Decode(&r); err != nil || r["error"].(map[string]any)["code"] != code {
			t.Fatalf("bad response: %v %v", r, err)
		}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		t.Fatal("non-protocol stdout")
	}
}
