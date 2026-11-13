// Package paseo implements the opt-in, session-scoped Paseo MCP bridge.
// The trusted A process owns the real endpoint and credentials. B only gets
// an authenticated temporary endpoint backed by the existing yamux session.
package paseo

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const Path = "/mcp/agents"
const Placeholder = "http://127.0.0.1:1/waldo-paseo"

var agentID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

type Endpoint struct {
	URL     *url.URL
	Headers http.Header
}

// Configs rewrites only the injected 'paseo' entry. File arguments are read
// on A and converted to inline JSON so no host configuration file is shared.
func Configs(args []string) ([]string, *Endpoint, error) {
	out := append([]string(nil), args...)
	var endpoint *Endpoint
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
				return nil, nil, fmt.Errorf("--mcp-config 缺少配置")
			}
			value = out[i]
		}
		data := []byte(value)
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			var err error
			data, err = os.ReadFile(value)
			if err != nil {
				return nil, nil, fmt.Errorf("读取 MCP 配置失败")
			}
		}
		if len(data) > 8<<20 {
			return nil, nil, fmt.Errorf("MCP 配置过大")
		}
		var root map[string]json.RawMessage
		if json.Unmarshal(data, &root) != nil {
			return nil, nil, fmt.Errorf("MCP 配置不是有效 JSON")
		}
		var servers map[string]json.RawMessage
		if json.Unmarshal(root["mcpServers"], &servers) != nil {
			return nil, nil, fmt.Errorf("MCP 配置缺少 mcpServers")
		}
		if raw, ok := servers["paseo"]; ok {
			if endpoint != nil {
				return nil, nil, fmt.Errorf("Paseo MCP 配置重复")
			}
			var entry struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			}
			if json.Unmarshal(raw, &entry) != nil || entry.Type != "http" {
				return nil, nil, fmt.Errorf("Paseo MCP 必须使用 HTTP")
			}
			u, err := url.Parse(entry.URL)
			if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.Path != Path || u.RawPath != "" || u.Opaque != "" {
				return nil, nil, fmt.Errorf("Paseo MCP 地址无效")
			}
			ip := net.ParseIP(u.Hostname())
			if u.Hostname() == "localhost" {
				ip = net.ParseIP("127.0.0.1")
			}
			if ip == nil || !ip.IsLoopback() || u.Port() == "" {
				return nil, nil, fmt.Errorf("Paseo MCP 必须指向显式端口的本机回环地址")
			}
			q, err := url.ParseQuery(u.RawQuery)
			if err != nil || len(q) != 1 || len(q["callerAgentId"]) != 1 || !agentID.MatchString(q.Get("callerAgentId")) {
				return nil, nil, fmt.Errorf("Paseo MCP 缺少合法 callerAgentId")
			}
			u.Host = net.JoinHostPort(ip.String(), u.Port())
			headers := make(http.Header)
			for k, v := range entry.Headers {
				if strings.ContainsAny(k+v, "\r\n") {
					return nil, nil, fmt.Errorf("Paseo MCP 请求头无效")
				}
				headers.Set(k, v)
			}
			if !strings.HasPrefix(headers.Get("Authorization"), "Bearer ") || strings.TrimSpace(strings.TrimPrefix(headers.Get("Authorization"), "Bearer ")) == "" {
				return nil, nil, fmt.Errorf("Paseo MCP 缺少 Bearer 认证")
			}
			endpoint = &Endpoint{URL: u, Headers: headers}
			// Preserve no original credential-bearing fields on B.
			servers["paseo"], _ = json.Marshal(map[string]string{"type": "http", "url": Placeholder})
			root["mcpServers"], _ = json.Marshal(servers)
		}
		encoded, _ := json.Marshal(root)
		out[i] = string(encoded)
		if inline {
			out[i] = "--mcp-config=" + out[i]
		}
	}
	if endpoint == nil {
		return nil, nil, fmt.Errorf("--paseo 需要由 Paseo 启动，并传入带 callerAgentId 和认证的 Paseo MCP 配置")
	}
	return out, endpoint, nil
}

// RemoteConfig replaces the credential-free placeholder after B allocates
// its temporary listener. Only this generated entry can be changed.
func RemoteConfig(args []string, address, token string) ([]string, error) {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		value, inline := strings.CutPrefix(out[i], "--mcp-config=")
		if !inline {
			if out[i] != "--mcp-config" {
				continue
			}
			i++
			if i == len(out) {
				break
			}
			value = out[i]
		}
		var root map[string]json.RawMessage
		var servers map[string]json.RawMessage
		if json.Unmarshal([]byte(value), &root) != nil || json.Unmarshal(root["mcpServers"], &servers) != nil {
			continue
		}
		var entry struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(servers["paseo"], &entry) != nil || entry.URL != Placeholder {
			continue
		}
		servers["paseo"], _ = json.Marshal(map[string]any{"type": "http", "url": address + Path, "headers": map[string]string{"Authorization": "Bearer " + token}})
		root["mcpServers"], _ = json.Marshal(servers)
		data, _ := json.Marshal(root)
		out[i] = string(data)
		if inline {
			out[i] = "--mcp-config=" + out[i]
		}
		return out, nil
	}
	return nil, fmt.Errorf("missing Paseo bridge placeholder")
}

// AuditArgs keeps MCP credentials and temporary bridge tokens out of logs.
func AuditArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out); i++ {
		if out[i] == "--mcp-config" && i+1 < len(out) {
			i++
			out[i] = "[MCP config redacted]"
		} else if strings.HasPrefix(out[i], "--mcp-config=") {
			out[i] = "--mcp-config=[redacted]"
		}
	}
	return out
}

type Skill struct{ Source, Target string }

// Skills exports only installed Paseo skills, with the native agent directory
// taking precedence. Directory bind mounts keep their supporting files live.
func Skills(home, destination string) ([]Skill, error) {
	seen := map[string]bool{}
	var skills []Skill
	for _, root := range []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".agents", "skills")} {
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if seen[name] || (name != "paseo" && !strings.HasPrefix(name, "paseo-")) {
				continue
			}
			source, err := filepath.EvalSymlinks(filepath.Join(root, name))
			if err != nil {
				return nil, err
			}
			st, err := os.Stat(filepath.Join(source, "SKILL.md"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("Paseo SKILL.md 必须为普通文件")
			}
			seen[name] = true
			skills = append(skills, Skill{Source: source, Target: filepath.Join(destination, name)})
		}
	}
	return skills, nil
}
