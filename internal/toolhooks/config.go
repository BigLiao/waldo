// Package toolhooks observes public agent hook events on B. It never changes
// tool inputs, permissions or results, and never sends hook payloads to A.
package toolhooks

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config adds HTTP hooks to this invocation's inline settings. Existing hooks
// and unrelated settings are retained. A settings filename is left alone: it
// may name a path visible only inside the capsule, not to the host preparing it.
func Config(adapter, endpoint string, argv []string) ([]string, error) {
	if adapter != "claude" {
		return nil, fmt.Errorf("unsupported tool hook adapter")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Port() == "" || !strings.HasPrefix(u.Path, "/hooks/") {
		return nil, fmt.Errorf("invalid local hook endpoint")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("hook endpoint must be loopback")
	}
	args := append([]string(nil), argv...)
	index, equals, stop := -1, false, len(args)
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			stop = i
			break
		}
		if args[i] == "--settings" {
			if i+1 == len(args) || args[i+1] == "--" {
				return nil, fmt.Errorf("missing settings value")
			}
			i++
			index, equals = i, false
		} else if strings.HasPrefix(args[i], "--settings=") {
			index, equals = i, true
		}
	}
	settings := map[string]json.RawMessage{}
	if index >= 0 {
		value := args[index]
		if equals {
			value = strings.TrimPrefix(value, "--settings=")
		}
		if !strings.HasPrefix(strings.TrimSpace(value), "{") {
			return nil, fmt.Errorf("tool observation requires inline --settings; settings file left unchanged")
		}
		if err := json.Unmarshal([]byte(value), &settings); err != nil || settings == nil {
			return nil, fmt.Errorf("invalid inline settings; settings left unchanged")
		}
	}
	if string(settings["disableAllHooks"]) == "true" {
		return nil, fmt.Errorf("hooks explicitly disabled")
	}
	hooks := map[string][]json.RawMessage{}
	if raw := settings["hooks"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
			return nil, fmt.Errorf("invalid hooks settings; hooks left unchanged")
		}
	}
	entry, _ := json.Marshal(map[string]any{"hooks": []any{map[string]any{
		"type": "http", "url": endpoint, "timeout": 1, "onFailure": "continue",
	}}})
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		hooks[event] = append(hooks[event], entry)
	}
	settings["hooks"], _ = json.Marshal(hooks)
	value, _ := json.Marshal(settings)
	if index >= 0 {
		args[index] = string(value)
		if equals {
			args[index] = "--settings=" + args[index]
		}
		return args, nil
	}
	out := append([]string(nil), args[:stop]...)
	out = append(out, "--settings", string(value))
	return append(out, args[stop:]...), nil
}
