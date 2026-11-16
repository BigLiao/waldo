package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// crossPlatformProbe is also runnable against a real remote B. It avoids Linux
// namespace assumptions about A and fails if a tool reaches the wrong machine.
func crossPlatformProbe(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("cross-platform requires expected host OS")
	}
	expected := args[0]
	marker, err := os.ReadFile("README")
	if err != nil {
		return err
	}
	result("cross_pid", os.Getpid())
	result("cross_readme", strings.TrimSpace(string(marker)))
	if err := os.WriteFile("cross-agent.txt", marker, 0600); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	shared := filepath.Join(home, ".waldo-cross-shared")
	if b, err := os.ReadFile(shared); err == nil {
		result("cross_shared_before", strings.TrimSpace(string(b)))
	}
	if err := os.WriteFile(shared, marker, 0600); err != nil {
		return err
	}
	var server struct {
		Command string
		Args    []string
	}
	for i := 0; i < len(args); i++ {
		value, inline := strings.CutPrefix(args[i], "--mcp-config=")
		if !inline {
			if args[i] != "--mcp-config" || i+1 == len(args) {
				continue
			}
			i++
			value = args[i]
		}
		var cfg struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(value), &cfg); err != nil {
			return err
		}
		if entry, ok := cfg.Servers["waldo"]; ok {
			if err := json.Unmarshal(entry, &server); err != nil {
				return err
			}
		}
	}
	if server.Command == "" {
		out, code := run("", "bash", "-c", "uname -s; printf forwarded > cross-host.txt")
		result("cross_host_os", out)
		if code != 0 || strings.TrimSpace(out) != expected {
			return fmt.Errorf("wrong host: %s/%d, want %s", out, code, expected)
		}
	} else {
		cmd := exec.Command(server.Command, server.Args...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		defer func() { stdin.Close(); cmd.Wait() }()
		enc, dec := json.NewEncoder(stdin), json.NewDecoder(stdout)
		rpc := func(id int, method string, params any) (json.RawMessage, error) {
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
				return nil, err
			}
			var reply struct {
				Result json.RawMessage
				Error  any
			}
			if err := dec.Decode(&reply); err != nil {
				return nil, err
			}
			if reply.Error != nil {
				return nil, fmt.Errorf("MCP error: %v", reply.Error)
			}
			return reply.Result, nil
		}
		if _, err := rpc(1, "initialize", map[string]any{}); err != nil {
			return err
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
		raw, err := rpc(2, "tools/call", map[string]any{"name": "Bash", "arguments": map[string]any{"command": "uname -s; printf forwarded > cross-host.txt"}})
		if err != nil {
			return err
		}
		result("cross_mcp_bash", string(raw))
		if !strings.Contains(string(raw), expected) {
			return fmt.Errorf("MCP host OS missing %s: %s", expected, raw)
		}
		if strings.Contains(strings.Join(server.Args, " "), "--file-tools") {
			cwd, _ := os.Getwd()
			raw, err = rpc(3, "tools/call", map[string]any{"name": "Read", "arguments": map[string]any{"file_path": filepath.Join(cwd, "README")}})
			if err != nil || !strings.Contains(string(raw), strings.TrimSpace(string(marker))) {
				return fmt.Errorf("MCP Read wrong content: %s %v", raw, err)
			}
			result("cross_mcp_read", true)
			if _, e := os.Stat(filepath.Join(cwd, "cross-mcp.txt")); e == nil {
				if _, err := rpc(4, "tools/call", map[string]any{"name": "Read", "arguments": map[string]any{"file_path": filepath.Join(cwd, "cross-mcp.txt")}}); err != nil {
					return err
				}
			}
			raw, err = rpc(5, "tools/call", map[string]any{"name": "Write", "arguments": map[string]any{"file_path": filepath.Join(cwd, "cross-mcp.txt"), "content": string(marker)}})
			if err != nil || strings.Contains(string(raw), `"isError":true`) {
				return fmt.Errorf("MCP Write failed: %s %v", raw, err)
			}
			result("cross_mcp_write", true)
		}
	}
	result("cross_ready", true)
	for _, arg := range args {
		if arg == "hold" {
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(".cross-done"); err == nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	result("cross_ok", true)
	return nil
}
