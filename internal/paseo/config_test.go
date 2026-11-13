package paseo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFor(address string) string {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"paseo": map[string]any{"type": "http", "url": address, "headers": map[string]string{"Authorization": "Bearer source-secret"}},
		"other": map[string]any{"type": "stdio", "command": "node", "args": []string{"server.js"}},
	}})
	return string(b)
}

func TestConfigsBindIdentityAndKeepCredentialsOnA(t *testing.T) {
	for _, inline := range []bool{false, true} {
		config := configFor("http://localhost:6767/mcp/agents?callerAgentId=agent-one")
		args := []string{"--mcp-config", config, "--model", "opus"}
		if inline {
			args = []string{"--mcp-config=" + config, "--model", "opus"}
		}
		out, e, err := Configs(args)
		if err != nil {
			t.Fatal(err)
		}
		if e.URL.Query().Get("callerAgentId") != "agent-one" || e.Headers.Get("Authorization") != "Bearer source-secret" {
			t.Fatal("lost original identity")
		}
		if e.URL.Host != "127.0.0.1:6767" {
			t.Fatal("localhost must not use remote DNS")
		}
		joined := strings.Join(out, " ")
		if strings.Contains(joined, "source-secret") || strings.Contains(joined, "callerAgentId") || !strings.Contains(joined, "server.js") {
			t.Fatal("configuration leaked or other server changed")
		}
		out, err = RemoteConfig(out, "http://127.0.0.1:12345", "bridge-secret")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(out, " "), "bridge-secret") {
			t.Fatal("missing bridge authentication")
		}
		if strings.Contains(strings.Join(AuditArgs(out), " "), "bridge-secret") {
			t.Fatal("audit leaked bridge credential")
		}
	}
}

func TestConfigsRejectUnscopedAndNonlocalTargets(t *testing.T) {
	for _, address := range []string{
		"http://example.com:6767/mcp/agents?callerAgentId=one",
		"http://127.0.0.1:6767/api/agents?callerAgentId=one",
		"http://127.0.0.1:6767/mcp/agents",
		"http://127.0.0.1:6767/mcp/agents?callerAgentId=one&callerAgentId=two",
		"http://127.0.0.1:6767/mcp/agents?callerAgentId=one&admin=true",
		"http://user:pass@127.0.0.1:6767/mcp/agents?callerAgentId=one",
	} {
		if _, _, err := Configs([]string{"--mcp-config", configFor(address)}); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
	if _, _, err := Configs([]string{"-p", "hi"}); err == nil {
		t.Fatal("missing MCP must fail rather than silently exporting only skills")
	}
}

func TestConfigFileAndSkillDiscovery(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "mcp.json")
	os.WriteFile(p, []byte(configFor("http://127.0.0.1:6767/mcp/agents?callerAgentId=one")), 0600)
	if out, _, err := Configs([]string{"--mcp-config", p}); err != nil || strings.Contains(strings.Join(out, " "), p) {
		t.Fatal("file config not inlined", err)
	}
	for _, name := range []string{"paseo", "paseo-advisor", "unrelated"} {
		d := filepath.Join(home, ".agents", "skills", name)
		os.MkdirAll(d, 0700)
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(name), 0600)
	}
	d := filepath.Join(home, ".claude", "skills", "paseo")
	os.MkdirAll(d, 0700)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("native"), 0600)
	skills, err := Skills(home, filepath.Join(home, ".claude", "skills"))
	if err != nil || len(skills) != 2 {
		t.Fatalf("skills: %v, %v", skills, err)
	}
	if skills[0].Source != d {
		t.Fatal("native directory must win")
	}
}
