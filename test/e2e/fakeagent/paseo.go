package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func paseoProbe(args []string) error {
	var cfg struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--mcp-config" {
			if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
				return err
			}
			break
		}
	}
	server, ok := cfg.Servers["paseo"]
	if !ok {
		return fmt.Errorf("missing injected Paseo config")
	}
	result("paseo_source_token_hidden", !strings.Contains(strings.Join(args, " "), "e2e-source-secret"))
	result("paseo_caller_hidden", !strings.Contains(server.URL, "callerAgentId"))
	r, _ := http.NewRequest("POST", server.URL, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	for k, v := range server.Headers {
		r.Header.Set(k, v)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	result("paseo_http", fmt.Sprintf("%d/%s", resp.StatusCode, strings.TrimSpace(string(data))))
	if err := os.WriteFile("paseo-endpoint", []byte(server.URL), 0600); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".claude", "skills", "paseo", "SKILL.md")
	data, err = os.ReadFile(p)
	result("paseo_skill_read", err == nil && strings.Contains(string(data), "test-paseo-skill"))
	result("paseo_skill_readonly", os.WriteFile(p, []byte("bad"), 0600) != nil)
	support := filepath.Join(filepath.Dir(p), "reference.txt")
	data, err = os.ReadFile(support)
	result("paseo_skill_support", err == nil && string(data) == "support")
	_, err = os.Stat(filepath.Join(home, ".claude", "skills", "unrelated", "SKILL.md"))
	result("paseo_unrelated_hidden", os.IsNotExist(err))
	_, code := run("", "bash", "-c", `test -r "$HOME/.claude/skills/paseo/SKILL.md" && ! printf bad > "$HOME/.claude/skills/paseo/SKILL.md"`)
	result("paseo_skill_shell_readonly", code == 0)
	return nil
}
