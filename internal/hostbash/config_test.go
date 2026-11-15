package hostbash

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigPreservesExistingServersAndLiteralArguments(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mcp.json")
	data := `{"mcpServers":{"paseo":{"type":"http","url":"http://127.0.0.1:1/mcp/agents","headers":{"Authorization":"Bearer retained"}}}}`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--mcp-config=" + file, "--", "--mcp-config", "literal"}
	before := append([]string(nil), args...)
	normalized, err := Configs(args)
	if err != nil {
		t.Fatal(err)
	}
	got := Inject(normalized, "/bin/bash", "/project")
	if !reflect.DeepEqual(args, before) || !strings.Contains(got[0], "Bearer retained") || !reflect.DeepEqual(got[len(got)-3:], args[len(args)-3:]) {
		t.Fatalf("arguments changed: %v", got)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got[2]), &cfg); err != nil {
		t.Fatal(err)
	}
	entry := cfg.Servers[ServerName]
	if entry.Command != "/run/waldo/waldo" || !reflect.DeepEqual(entry.Args, []string{"host-bash-mcp", "--shell", "/bin/bash", "--cwd", "/project"}) {
		t.Fatalf("wrong MCP process: %+v", entry)
	}
	unchanged, _ := os.ReadFile(file)
	if string(unchanged) != data {
		t.Fatal("modified persistent MCP file")
	}
}

func TestConfigRejectsReservedNameAndInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{"--mcp-config"}, {"--mcp-config", "/nonexistent/config.json"},
		{"--mcp-config", `{"mcpServers":{"waldo":{}}}`},
		{"--mcp-config", `{"mcpServers":null}`}, {"--mcp-config", `{broken`},
	} {
		if _, err := Configs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestDiscoveryCommandsDoNotReceiveAgentOnlyMCPFlags(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}, {"version"}, {"auth", "status"}} {
		if !IsProbe(args) || !reflect.DeepEqual(Inject(args, "/bin/bash", "/project"), args) {
			t.Fatalf("added agent-only flags to discovery command %v", args)
		}
	}
	for _, args := range [][]string{{"-p", "--version"}, {"--", "--version"}, {"auth", "login"}} {
		if IsProbe(args) {
			t.Fatalf("misclassified agent arguments: %v", args)
		}
	}
}

func TestDisableHostToolsPreservesArguments(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"auth", "status"}} {
		got := DisableHostTools(args)
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("changed probe: %v", got)
		}
	}
	args := []string{"--disallowedTools=Edit", "--", "prompt"}
	got := DisableHostTools(args)
	want := append([]string{"--disallowedTools=Bash,Read,Edit,Write,Glob,Grep,NotebookEdit"}, args...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lost arguments: %v", got)
	}
}
