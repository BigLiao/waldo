package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaseoBridgeAndReadOnlySkills(t *testing.T) {
	e, agent := setup(t)
	home := filepath.Join(e.Root, "home")
	for _, name := range []string{"paseo", "unrelated"} {
		d := filepath.Join(home, ".claude", "skills", name)
		os.MkdirAll(d, 0700)
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("test-paseo-skill"), 0600)
		os.WriteFile(filepath.Join(d, "reference.txt"), []byte("support"), 0600)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/agents" || r.URL.RawQuery != "callerAgentId=e2e-agent" || r.Header.Get("Authorization") != "Bearer e2e-source-secret" {
			t.Errorf("unexpected upstream request identity")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer upstream.Close()
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"paseo": map[string]any{"type": "http", "url": upstream.URL + "/mcp/agents?callerAgentId=e2e-agent", "headers": map[string]string{"Authorization": "Bearer e2e-source-secret"}}}})
	e.WriteConfig("[agent.claude]\nbin = " + strconvQuote(agent) + "\nstate = []\nshared = []\n")
	cmd := e.Cmd("claude", "--paseo", "paseo", "--mcp-config", string(cfg))
	cmd.Env = append(cmd.Env, "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Paseo probe: %v\n%s", err, out)
	}
	r := parseResults(string(out))
	for _, k := range []string{"paseo_source_token_hidden", "paseo_caller_hidden", "paseo_skill_read", "paseo_skill_readonly", "paseo_skill_support", "paseo_unrelated_hidden", "paseo_skill_shell_readonly"} {
		if r[k] != "true" {
			t.Errorf("%s = %q\n%s", k, r[k], out)
		}
	}
	if r["paseo_http"] != `200/{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("HTTP bridge: %q", r["paseo_http"])
	}
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "paseo", "SKILL.md"))
	if string(data) != "test-paseo-skill" {
		t.Fatal("source skill changed")
	}
	log, _ := os.ReadFile(filepath.Join(e.AData, "audit.log"))
	if strings.Contains(string(log), "e2e-source-secret") {
		t.Fatal("Paseo credential in audit log")
	}
	if !strings.Contains(string(log), `"kind":"paseo-mcp"`) {
		t.Fatal("missing MCP audit event")
	}
	address, _ := os.ReadFile(filepath.Join(e.Project, "paseo-endpoint"))
	if resp, err := http.Get(string(address)); err == nil {
		resp.Body.Close()
		t.Fatal("Paseo gateway survived session exit")
	}
	waitNoMounts(t)
}

func strconvQuote(s string) string { data, _ := json.Marshal(s); return string(data) }
