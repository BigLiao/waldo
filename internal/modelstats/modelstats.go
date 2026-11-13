// Package modelstats receives only the small model timing subset of the
// agents' native, asynchronously batched OTLP/HTTP JSON logs. It neither
// intercepts model requests nor retains prompts, responses, or tool contents.
//
// Configuration sources:
// https://learn.chatgpt.com/docs/config-file/config-advanced
// https://learn.chatgpt.com/docs/config-file/config-reference
// https://code.claude.com/docs/en/monitoring-usage
package modelstats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 1 << 20

// Stats contains cumulative agent-reported durations in milliseconds. In
// particular, Codex HTTP request time and stream-event processing time are separate:
// neither is advertised as the complete model-generation duration.
type Stats struct {
	Requests      uint64  `json:"requests"`
	Errors        uint64  `json:"errors"`
	RequestMS     float64 `json:"request_ms"`
	StreamEventMS float64 `json:"stream_event_ms"`
}

type Collector struct {
	URL string

	provider string
	path     string
	server   *http.Server
	mu       sync.Mutex
	stats    Stats
	slots    chan struct{}
	once     sync.Once
}

func validProvider(provider string) bool { return provider == "claude" || provider == "codex" }

// Start listens only on loopback and scopes the endpoint to this session with
// a random path. Export failures remain the agent's asynchronous best effort;
// the collector never calls back into an agent or its model API.
func Start(provider string) (*Collector, error) {
	if !validProvider(provider) {
		return nil, fmt.Errorf("unsupported model telemetry provider %q", provider)
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c := &Collector{provider: provider, path: "/" + hex.EncodeToString(token[:]) + "/v1/logs", slots: make(chan struct{}, 4)}
	c.URL = "http://" + ln.Addr().String() + c.path
	c.server = &http.Server{
		Handler: c, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go c.server.Serve(ln)
	return c, nil
}

func (c *Collector) Snapshot() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *Collector) Close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		if c.server.Shutdown(ctx) != nil {
			c.server.Close()
		}
	})
}

type value struct {
	String string          `json:"stringValue"`
	Int    json.RawMessage `json:"intValue"`
	Double json.RawMessage `json:"doubleValue"`
	Bool   *bool           `json:"boolValue"`
}

type attribute struct {
	Key   string `json:"key"`
	Value value  `json:"value"`
}

type record struct {
	Name       string      `json:"eventName"`
	Body       value       `json:"body"`
	Attributes []attribute `json:"attributes"`
}

// The ignored resource metadata and record fields can contain identity or
// content. Only event names, numeric durations and success flags survive this
// HTTP request; none of the original payload is stored or logged.
type export struct {
	Resources []struct {
		Scopes []struct {
			Records []record `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != c.path || r.URL.RawPath != "" || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if media, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(media) != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	defer r.Body.Close()
	var data export
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&data)
	if err == nil {
		var trailing any
		if err = decoder.Decode(&trailing); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing JSON")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	var delta Stats
	for _, resource := range data.Resources {
		for _, scope := range resource.Scopes {
			for _, record := range scope.Records {
				c.addRecord(&delta, record)
			}
		}
	}
	c.mu.Lock()
	c.stats.Requests += delta.Requests
	c.stats.Errors += delta.Errors
	c.stats.RequestMS += delta.RequestMS
	c.stats.StreamEventMS += delta.StreamEventMS
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

func (c *Collector) addRecord(stats *Stats, r record) {
	name := r.Name
	if name == "" {
		name = r.Body.String
	}
	var duration float64
	failed := false
	for _, a := range r.Attributes {
		switch a.Key {
		case "event.name":
			if a.Value.String != "" {
				name = a.Value.String
			}
		case "duration_ms":
			duration = milliseconds(a.Value)
		case "success":
			failed = a.Value.Bool != nil && !*a.Value.Bool || a.Value.String == "false"
		}
	}
	prefix := "codex."
	if c.provider == "claude" {
		prefix = "claude_code."
	}
	name = strings.TrimPrefix(name, prefix)
	switch name {
	case "api_request":
		stats.Requests++
		stats.RequestMS += duration
		if failed {
			stats.Errors++
		}
	case "api_error":
		if c.provider == "claude" {
			stats.Requests++
			stats.Errors++
			stats.RequestMS += duration
		}
	case "websocket_request":
		if c.provider == "codex" {
			stats.Requests++
			stats.RequestMS += duration
			if failed {
				stats.Errors++
			}
		}
	case "sse_event", "websocket_event":
		if c.provider == "codex" {
			stats.StreamEventMS += duration
		}
	}
}

func milliseconds(v value) float64 {
	raw := v.Double
	if len(raw) == 0 {
		raw = v.Int
	}
	text := string(raw)
	if len(text) > 0 && text[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return 0
		}
	}
	if len(raw) == 0 {
		text = v.String
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > float64((24*time.Hour)/time.Millisecond) {
		return 0
	}
	return n
}
