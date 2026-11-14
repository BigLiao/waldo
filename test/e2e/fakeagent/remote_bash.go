package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"waldo/internal/hostbash"
)

func remoteBashProbe(args []string) error {
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	result("remote_agent_netns", ns)
	out, code := run("", "bash", "--noprofile", "--norc", "-c", `readlink /proc/self/ns/net; printf remote > remote-write`)
	result("remote_bash_netns", out)
	result("remote_bash_code", code)
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
		if entry, ok := cfg.Servers[hostbash.ServerName]; ok {
			if err := json.Unmarshal(entry, &server); err != nil {
				return err
			}
		}
	}
	if server.Command != "/run/waldo/waldo" {
		return fmt.Errorf("missing injected host Bash MCP")
	}
	cmd := exec.Command(server.Command, server.Args...)
	in, err := cmd.StdinPipe()
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
	defer func() { in.Close(); cmd.Wait() }()
	enc, dec := json.NewEncoder(in), json.NewDecoder(stdout)
	rpc := func(id int, method string, params any) (json.RawMessage, error) {
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		var response struct {
			Result json.RawMessage
			Error  any
		}
		if err := dec.Decode(&response); err != nil {
			return nil, err
		}
		if response.Error != nil {
			return nil, fmt.Errorf("MCP error: %v", response.Error)
		}
		return response.Result, nil
	}
	if _, err := rpc(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "fakeagent", "version": "1"}}); err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return err
	}
	if _, err := rpc(2, "tools/list", map[string]any{}); err != nil {
		return err
	}
	raw, err := rpc(3, "tools/call", map[string]any{"name": hostbash.ToolName, "arguments": hostbash.Call{Command: `readlink /proc/self/ns/net; printf host > host-write; printf '%s' "$E2E_HOST_ONLY"; printf stderr >&2; exit 7`}})
	if err != nil {
		return err
	}
	var callResult struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := json.Unmarshal(raw, &callResult); err != nil || len(callResult.Content) != 1 {
		return fmt.Errorf("bad call result: %s", raw)
	}
	var output hostbash.Result
	if err := json.Unmarshal([]byte(callResult.Content[0].Text), &output); err != nil {
		return err
	}
	lines := strings.SplitN(strings.TrimSpace(output.Stdout), "\n", 2)
	result("host_bash_netns", lines[0])
	result("host_bash_env", len(lines) == 2 && lines[1] == "host-environment")
	result("host_bash_exit", output.ExitCode)
	result("host_bash_stderr", output.Stderr)
	result("host_bash_iserror", callResult.IsError)
	return nil
}
