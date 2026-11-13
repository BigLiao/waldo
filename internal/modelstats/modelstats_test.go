package modelstats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func testCollector(provider string) *Collector {
	return &Collector{provider: provider, path: "/session-secret/v1/logs", slots: make(chan struct{}, 4)}
}

func payload(records ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{"scopeLogs": []any{map[string]any{"logRecords": records}}}}})
	return string(b)
}

func event(name string, duration any, extras ...map[string]any) map[string]any {
	attrs := []map[string]any{{"key": "duration_ms", "value": duration}}
	attrs = append(attrs, extras...)
	return map[string]any{"body": map[string]any{"stringValue": name}, "attributes": attrs}
}

func post(c *Collector, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+c.path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	return w
}

func TestClaudeTimingsOnly(t *testing.T) {
	c := testCollector("claude")
	w := post(c, payload(
		event("claude_code.api_request", map[string]any{"intValue": "120"}),
		event("ignored-body", map[string]any{"doubleValue": 25.5}, map[string]any{"key": "event.name", "value": map[string]any{"stringValue": "api_error"}}),
		event("claude_code.tool_result", map[string]any{"intValue": "900"}, map[string]any{"key": "tool_input", "value": map[string]any{"stringValue": "secret file content"}}),
		event("codex.api_request", map[string]any{"intValue": "500"}),
	))
	if w.Code != http.StatusOK || w.Body.String() != "{}" {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	if got := c.Snapshot(); got != (Stats{Requests: 2, Errors: 1, RequestMS: 145.5}) {
		t.Fatalf("stats = %+v", got)
	}
	// An invalid batch cannot partially publish its earlier records.
	if post(c, payload(event("api_request", map[string]any{"intValue": "9"}))+"{}").Code != http.StatusBadRequest || c.Snapshot().Requests != 2 {
		t.Fatal("invalid batch published statistics")
	}
}

func TestCodexRequestsAndStreamEventsAreSeparate(t *testing.T) {
	c := testCollector("codex")
	post(c, payload(
		event("codex.api_request", map[string]any{"doubleValue": 12.5}, map[string]any{"key": "success", "value": map[string]any{"boolValue": true}}),
		event("codex.api_request", map[string]any{"intValue": "18"}, map[string]any{"key": "success", "value": map[string]any{"boolValue": false}}),
		event("codex.sse_event", map[string]any{"intValue": "80"}),
		map[string]any{"eventName": "codex.websocket_request", "attributes": []any{map[string]any{"key": "duration_ms", "value": map[string]any{"doubleValue": 2.5}}}},
		event("codex.websocket_event", map[string]any{"stringValue": "10.5"}),
		event("codex.tool_result", map[string]any{"intValue": "500"}),
		event("codex.api_error", map[string]any{"intValue": "500"}),
	))
	if got := c.Snapshot(); got != (Stats{Requests: 3, Errors: 1, RequestMS: 33, StreamEventMS: 90.5}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestInvalidDurationsCannotPoisonAggregate(t *testing.T) {
	c := testCollector("claude")
	for _, duration := range []any{
		map[string]any{"doubleValue": -1},
		map[string]any{"stringValue": "NaN"},
		map[string]any{"stringValue": "+Inf"},
		map[string]any{"intValue": "86400001"},
		map[string]any{"stringValue": "5 seconds"},
		nil,
	} {
		if w := post(c, payload(event("claude_code.api_request", duration))); w.Code != http.StatusOK {
			t.Fatalf("response %d", w.Code)
		}
	}
	if got := c.Snapshot(); got.Requests != 6 || got.RequestMS != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestCollectorRejectsWrongEndpointAndUnboundedInput(t *testing.T) {
	c := testCollector("claude")
	for _, tc := range []struct {
		method, path, media, encoding, body string
		want                                int
	}{
		{"POST", "/v1/logs", "application/json", "", "{}", 404},
		{"POST", c.path + "?token=x", "application/json", "", "{}", 404},
		{"GET", c.path, "application/json", "", "", 405},
		{"POST", c.path, "application/x-protobuf", "", "{}", 415},
		{"POST", c.path, "application/json", "gzip", "{}", 415},
		{"POST", c.path, "application/json", "", "invalid json", 400},
		{"POST", c.path, "application/json", "", `{"ignored":"` + strings.Repeat("x", maxBody) + `"}`, 413},
	} {
		r := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.media)
		r.Header.Set("Content-Encoding", tc.encoding)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: code %d, want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
	if c.Snapshot() != (Stats{}) {
		t.Fatal("invalid request changed statistics")
	}
	for i := 0; i < cap(c.slots); i++ {
		c.slots <- struct{}{}
	}
	if post(c, "{}").Code != http.StatusServiceUnavailable {
		t.Fatal("collector accepted unbounded concurrent handlers")
	}
}

func TestConfigIsInvocationScopedAndContentDisabled(t *testing.T) {
	url := "http://127.0.0.1:12345/session-token/v1/logs"
	args := []string{"exec", "--", "hello"}
	env := []string{"KEEP=value", "OTEL_LOG_USER_PROMPTS=1", "OTEL_LOG_USER_PROMPTS=1", "OTEL_LOG_RAW_API_BODIES=file:/tmp/raw", "BETA_TRACING_ENDPOINT=http://elsewhere", "ENABLE_BETA_TRACING_DETAILED=1"}
	argsBefore, envBefore := append([]string(nil), args...), append([]string(nil), env...)
	gotArgs, gotEnv, err := Config("claude", url, args, env)
	if err != nil || !reflect.DeepEqual(gotArgs, args) {
		t.Fatalf("config: %v %v", gotArgs, err)
	}
	values := map[string]string{}
	for _, e := range gotEnv {
		k, v, _ := strings.Cut(e, "=")
		if _, duplicate := values[k]; duplicate {
			t.Fatalf("duplicate key %s", k)
		}
		values[k] = v
	}
	if values["KEEP"] != "value" || values["OTEL_LOG_USER_PROMPTS"] != "0" || values["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != url || values["OTEL_EXPORTER_OTLP_LOGS_PROTOCOL"] != "http/json" {
		t.Fatalf("env = %v", values)
	}
	for _, key := range []string{"OTEL_LOG_RAW_API_BODIES", "BETA_TRACING_ENDPOINT", "ENABLE_BETA_TRACING_DETAILED"} {
		if _, ok := values[key]; ok {
			t.Fatalf("content-bearing override survived: %s", key)
		}
	}
	gotArgs, gotEnv, err = Config("codex", url, args, env)
	if err != nil || !reflect.DeepEqual(gotEnv, env) || !reflect.DeepEqual(gotArgs[len(gotArgs)-len(args):], args) {
		t.Fatalf("codex config: %v %v %v", gotArgs, gotEnv, err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), `protocol="json"`) || !strings.Contains(strings.Join(gotArgs, " "), "otel.tool_result.max_bytes=0") {
		t.Fatalf("codex overrides = %v", gotArgs)
	}
	if !reflect.DeepEqual(argsBefore, args) || !reflect.DeepEqual(envBefore, env) {
		t.Fatal("modified caller configuration")
	}
}

func TestConfigRejectsExternalDestinations(t *testing.T) {
	for _, endpoint := range []string{
		"https://127.0.0.1:123/v1/logs", "http://example.com:123/v1/logs", "http://127.0.0.1/v1/logs",
		"http://user:pass@127.0.0.1:123/v1/logs", "http://127.0.0.1:123/v1/logs?x=1", "http://127.0.0.1:123/v1/traces",
	} {
		if _, _, err := Config("claude", endpoint, nil, nil); err == nil {
			t.Fatalf("accepted endpoint %s", endpoint)
		}
	}
	if _, _, err := Config("unknown", "http://127.0.0.1:123/v1/logs", nil, nil); err == nil {
		t.Fatal("accepted unknown provider")
	}
}
