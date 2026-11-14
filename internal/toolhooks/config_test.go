package toolhooks

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConfigPreservesSDKSettingsAndExistingHooks(t *testing.T) {
	settings := `{"model":"existing-model","permissions":{"deny":["Bash(rm *)"]},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"existing-hook"}]}]}}`
	for _, equals := range []bool{false, true} {
		args := []string{"--verbose", "--settings", settings, "--", "--settings", "literal"}
		valueIndex := 2
		if equals {
			args = []string{"--verbose", "--settings=" + settings, "--", "--settings", "literal"}
			valueIndex = 1
		}
		original := append([]string(nil), args...)
		out, err := Config("claude", "http://127.0.0.1:12345/hooks/secret", args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(args, original) || !reflect.DeepEqual(out[valueIndex+1:], args[valueIndex+1:]) {
			t.Fatal("modified original args or literal arguments")
		}
		var got struct {
			Model       string
			Permissions map[string][]string
			Hooks       map[string][]struct {
				Hooks []map[string]any
			}
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(out[valueIndex], "--settings=")), &got); err != nil {
			t.Fatal(err)
		}
		if got.Model != "existing-model" || !reflect.DeepEqual(got.Permissions["deny"], []string{"Bash(rm *)"}) || len(got.Hooks["PreToolUse"]) != 2 {
			t.Fatalf("lost user settings: %+v", got)
		}
		if got.Hooks["PreToolUse"][0].Hooks[0]["command"] != "existing-hook" {
			t.Fatal("lost existing hook")
		}
		for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
			entries := got.Hooks[event]
			hook := entries[len(entries)-1].Hooks[0]
			if hook["type"] != "http" || hook["onFailure"] != "continue" || hook["timeout"] != float64(1) || hook["command"] != nil {
				t.Fatalf("hook can execute a shell or block a tool: %+v", hook)
			}
		}
	}
}

func TestConfigInsertsBeforeLiteralArguments(t *testing.T) {
	out, err := Config("claude", "http://127.0.0.1:12345/hooks/secret", []string{"-p", "prompt", "--", "--settings", "literal"})
	if err != nil || len(out) != 7 || out[2] != "--settings" || out[4] != "--" || out[6] != "literal" {
		t.Fatalf("args=%v err=%v", out, err)
	}
}

func TestConfigDoesNotOverrideDisabledOrFileSettings(t *testing.T) {
	for _, settings := range []string{`{"disableAllHooks":true}`, `/private/settings.json`, `{"hooks":false}`, `{"hooks":null}`, `{"broken"}`} {
		args := []string{"--settings", settings}
		if _, err := Config("claude", "http://127.0.0.1:12345/hooks/secret", args); err == nil {
			t.Fatalf("accepted %s", settings)
		}
		if args[1] != settings {
			t.Fatal("changed caller settings")
		}
	}
	if _, err := Config("claude", "http://127.0.0.1:12345/hooks/secret", []string{"--settings"}); err == nil {
		t.Fatal("accepted missing settings")
	}
}

func TestConfigRejectsNonLocalEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://example.com/hooks/secret", "http://localhost:123/hooks/secret", "http://127.0.0.1/hooks/secret", "http://127.0.0.1:123/hooks/secret?q=1"} {
		if _, err := Config("claude", endpoint, nil); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
}
