// Package hostbash exposes sandbox tools as a stdio MCP, routing operations
// to A over the existing exec channel or locally inside B's capsule.
package hostbash

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"waldo/internal/proto"
)

const ServerName = "waldo"
const ToolName = proto.ToolHostBash

// IsProbe identifies the CLI discovery commands used by provider diagnostics.
// They never open an agent session; auth subcommands reject agent-only flags.
func IsProbe(args []string) bool {
	return len(args) > 0 && (args[0] == "--version" || args[0] == "-v" || args[0] == "version" || (args[0] == "auth" && len(args) > 1 && args[1] == "status"))
}

// Configs resolves explicit MCP files on A, preserving all entries except that
// the injected server name is reserved. No persistent configuration is changed.
func Configs(args []string) ([]string, error) {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		if out[i] == "--" {
			break
		}
		value, inline := strings.CutPrefix(out[i], "--mcp-config=")
		if !inline {
			if out[i] != "--mcp-config" {
				continue
			}
			i++
			if i == len(out) {
				return nil, fmt.Errorf("--mcp-config 缺少配置")
			}
			value = out[i]
		}
		data := []byte(value)
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			var err error
			data, err = os.ReadFile(value)
			if err != nil {
				return nil, fmt.Errorf("读取 MCP 配置失败")
			}
		}
		var root map[string]json.RawMessage
		var servers map[string]json.RawMessage
		if len(data) > 8<<20 || json.Unmarshal(data, &root) != nil || json.Unmarshal(root["mcpServers"], &servers) != nil || servers == nil {
			return nil, fmt.Errorf("MCP 配置需要有效的 mcpServers 对象")
		}
		if _, ok := servers[ServerName]; ok {
			return nil, fmt.Errorf("MCP 名称 %s 保留给 Waldo", ServerName)
		}
		out[i] = string(data)
		if inline {
			out[i] = "--mcp-config=" + out[i]
		}
	}
	return out, nil
}

// Inject adds a separate, session-only config; Claude merges it with existing
// MCP servers (including Paseo). The fixed executable is inside B's capsule.
func Inject(args []string, shell, cwd string, fileTools ...bool) []string {
	if IsProbe(args) {
		return append([]string(nil), args...)
	}
	serverArgs := []string{"host-bash-mcp", "--shell", shell, "--cwd", cwd}
	if len(fileTools) > 0 && fileTools[0] {
		serverArgs = append(serverArgs, "--file-tools")
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		ServerName: map[string]any{"type": "stdio", "command": "/run/waldo/waldo", "args": serverArgs},
	}})
	end := len(args)
	for i, arg := range args {
		if arg == "--" {
			end = i
			break
		}
	}
	out := append([]string(nil), args[:end]...)
	out = append(out, "--mcp-config", string(data))
	return append(out, args[end:]...)
}

// DisableHostTools routes shell and filesystem work through the host MCP.
func DisableHostTools(args []string) []string {
	if IsProbe(args) {
		return append([]string(nil), args...)
	}
	return append([]string{"--disallowedTools=Bash,Read,Edit,Write,Glob,Grep,NotebookEdit"}, args...)
}
