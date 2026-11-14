package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolHooksStayOnBAndCloseWithAgent(t *testing.T) {
	e, agent := setup(t)
	e.WriteConfig("[agent.claude]\nbin = " + strconvQuote(agent) + "\nstate = []\nshared = []\ntelemetry = \"none\"\n")
	out, err := e.Cmd("claude", "--observe", "tool-hooks").CombinedOutput()
	if err != nil || parseResults(string(out))["tool_hooks"] != "ok" {
		t.Fatalf("hook probe: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(e.BData, "log", "tool-timing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-hook-") || strings.Contains(string(data), "/hooks/") {
		t.Fatal("hook content leaked into log")
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["kind"] == "tool-end" && event["status"] == "completed" && event["duration_ms"] == float64(42) && event["matched_start"] == true && event["hook_elapsed_ms"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing paired timing: %s", data)
	}
	audit, _ := os.ReadFile(filepath.Join(e.AData, "audit.log"))
	if strings.Contains(string(audit), "fake-native-session") || strings.Contains(string(audit), "private-hook-") || strings.Contains(string(audit), "/hooks/") {
		t.Fatal("B hook data or endpoint transferred to A audit")
	}
	address, _ := os.ReadFile(filepath.Join(e.Project, "hook-endpoint"))
	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Post(string(address), "application/json", strings.NewReader(`{}`)); err == nil {
		resp.Body.Close()
		t.Fatal("hook listener survived agent exit")
	}
	waitNoMounts(t)
}
