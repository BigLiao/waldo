package modelstats

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Config changes only this agent invocation. It never edits the persistent
// agent configuration or sends telemetry to a remote collector.
func Config(provider, endpoint string, argv, env []string) ([]string, []string, error) {
	if !validProvider(provider) {
		return nil, nil, fmt.Errorf("unsupported model telemetry provider %q", provider)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Port() == "" || u.RawPath != "" || !strings.HasSuffix(u.Path, "/v1/logs") {
		return nil, nil, fmt.Errorf("invalid local model telemetry endpoint")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, nil, fmt.Errorf("model telemetry endpoint must be loopback")
	}
	args := append([]string(nil), argv...)
	outEnv := append([]string(nil), env...)
	if provider == "codex" {
		overrides := []string{
			"otel.exporter={otlp-http={endpoint=" + strconv.Quote(endpoint) + ",protocol=\"json\"}}",
			"otel.log_user_prompt=false",
			"otel.log_agent_responses=false",
			"otel.tool_result.max_bytes=0",
		}
		var prefix []string
		for _, override := range overrides {
			prefix = append(prefix, "-c", override)
		}
		return append(prefix, args...), outEnv, nil
	}
	// Raw bodies and beta tracing can bypass the ordinary log destination or
	// introduce content-bearing events. They are not part of this lightweight
	// observer, regardless of the caller's inherited environment.
	drop := map[string]bool{"OTEL_LOG_RAW_API_BODIES": true, "BETA_TRACING_ENDPOINT": true, "ENABLE_BETA_TRACING_DETAILED": true}
	filtered := outEnv[:0]
	for _, e := range outEnv {
		k, _, _ := strings.Cut(e, "=")
		if !drop[k] {
			filtered = append(filtered, e)
		}
	}
	outEnv = filtered
	settings := []struct{ key, value string }{
		{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"},
		{"OTEL_LOGS_EXPORTER", "otlp"},
		{"OTEL_METRICS_EXPORTER", "none"},
		{"OTEL_TRACES_EXPORTER", "none"},
		{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/json"},
		{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", endpoint},
		{"OTEL_LOG_USER_PROMPTS", "0"},
		{"OTEL_LOG_ASSISTANT_RESPONSES", "0"},
		{"OTEL_LOG_TOOL_DETAILS", "0"},
		{"OTEL_LOG_TOOL_CONTENT", "0"},
		{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "0"},
		{"ENABLE_ENHANCED_TELEMETRY_BETA", "0"},
	}
	for _, setting := range settings {
		next := outEnv[:0]
		for _, e := range outEnv {
			if k, _, _ := strings.Cut(e, "="); k != setting.key {
				next = append(next, e)
			}
		}
		outEnv = append(next, setting.key+"="+setting.value)
	}
	return args, outEnv, nil
}
