package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func toolHooksProbe(args []string) error {
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			}
		} `json:"hooks"`
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--settings" {
			if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
				return err
			}
			break
		}
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	var endpoint string
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		entries := cfg.Hooks[event]
		if len(entries) == 0 || len(entries[0].Hooks) == 0 || entries[0].Hooks[0].Type != "http" {
			return fmt.Errorf("missing HTTP hook: %s", event)
		}
		endpoint = entries[0].Hooks[0].URL
		body := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"fake-native-session","tool_use_id":"test-call","tool_name":"Bash","duration_ms":42,"tool_input":{"command":"private-hook-input"},"tool_response":{"stdout":"private-hook-output"}}`, event)
		resp, err := client.Post(endpoint, "application/json", strings.NewReader(body))
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 204 || len(data) != 0 {
			return fmt.Errorf("hook returned a decision")
		}
	}
	if err := os.WriteFile("hook-endpoint", []byte(endpoint), 0600); err != nil {
		return err
	}
	result("tool_hooks", "ok")
	return nil
}
