package hostbash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Only timing and routing metadata leave this MCP, never tool arguments/results.
// A bounded queue makes observation independent of tool latency.
type executionObserver struct {
	queue chan []byte
	done  chan struct{}
	seq   atomic.Uint64
}

func newExecutionObserver(endpoint string) *executionObserver {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" || !strings.HasPrefix(u.Path, "/hooks/") {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil
	}
	o := &executionObserver{queue: make(chan []byte, 128), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		defer client.CloseIdleConnections()
		for data := range o.queue {
			resp, e := client.Post(endpoint, "application/json", bytes.NewReader(data))
			if e == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
				resp.Body.Close()
			}
		}
	}()
	return o
}
func (o *executionObserver) record(tool, side string, elapsed time.Duration, failed bool) {
	if o == nil {
		return
	}
	if tool == "" {
		tool = ToolName
	}
	status := "completed"
	if failed {
		status = "failed"
	}
	b, _ := json.Marshal(map[string]any{"hook_event_name": "WaldoToolExecution", "session_id": "waldo-mcp", "tool_use_id": fmt.Sprintf("mcp-%d-%d", time.Now().UnixNano(), o.seq.Add(1)), "tool_name": "mcp__waldo__" + tool, "location": side, "duration_ms": float64(elapsed) / float64(time.Millisecond), "status": status})
	select {
	case o.queue <- b:
	default:
	}
}
func (o *executionObserver) close() {
	if o == nil {
		return
	}
	close(o.queue)
	select {
	case <-o.done:
	case <-time.After(300 * time.Millisecond):
	}
}
