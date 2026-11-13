package paseo

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgePreservesStreamsAndBindsAuthority(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != Path || r.URL.RawQuery != "callerAgentId=agent-one" || r.Header.Get("Authorization") != "Bearer source-secret" {
			t.Error("wrong upstream identity")
		}
		if r.Header.Get("X-Untrusted") != "" {
			t.Error("untrusted header forwarded")
		}
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			io.WriteString(w, "data: second\n\n")
			return
		}
		w.Header().Set("Mcp-Session-Id", "session-one")
		w.Header().Set("Set-Cookie", "private=value")
		io.Copy(w, r.Body)
	}))
	defer upstream.Close()
	_, endpoint, err := Configs([]string{"--mcp-config", configFor(upstream.URL + Path + "?callerAgentId=agent-one")})
	if err != nil {
		t.Fatal(err)
	}
	broker := NewBroker(endpoint)
	defer broker.Close()
	gateway, err := StartGateway(func() (io.ReadWriteCloser, error) { a, b := net.Pipe(); go broker.Serve(a); return b, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	request := func(method, path, sid, token string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, gateway.URL+path, bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Untrusted", "bad")
		if sid != "" {
			r.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := request("POST", Path, "", gateway.Token)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !json.Valid(data) || resp.Header.Get("Mcp-Session-Id") != "session-one" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("response %d: %s", resp.StatusCode, data)
	}
	for _, tc := range []struct{ path, sid, token string }{{"/api/agents", "", gateway.Token}, {Path + "?callerAgentId=other", "", gateway.Token}, {Path, "other-session", gateway.Token}, {Path, "", "wrong"}} {
		resp = request("POST", tc.path, tc.sid, tc.token)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("expected denial, got %d", resp.StatusCode)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("denied request reached upstream")
	}
	resp = request("GET", Path, "session-one", gateway.Token)
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: first\n\n" {
		t.Fatal("SSE first event", err)
	}
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(rest), "second") {
		t.Fatal("missing second SSE event")
	}
	address := gateway.URL
	gateway.Close()
	if resp, err := http.Get(address + Path); err == nil {
		resp.Body.Close()
		t.Fatal("gateway survived close")
	}
}
