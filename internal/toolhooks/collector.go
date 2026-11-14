package toolhooks

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxBody    = 4 << 20
	queueSize  = 256
	maxPending = 4096
)

// Input deliberately has no tool input, tool output, error text or paths.
// Unknown fields in the public hook payload are consumed but never retained.
type input struct {
	Event     string   `json:"hook_event_name"`
	Session   string   `json:"session_id"`
	Call      string   `json:"tool_use_id"`
	Tool      string   `json:"tool_name"`
	Duration  *float64 `json:"duration_ms"`
	Interrupt bool     `json:"is_interrupt"`
	at        time.Time
}

type record struct {
	Time        string   `json:"t"`
	Session     string   `json:"s"`
	Kind        string   `json:"kind"`
	Agent       string   `json:"agent_session_id,omitempty"`
	Call        string   `json:"tool_use_id,omitempty"`
	Tool        string   `json:"tool,omitempty"`
	Status      string   `json:"status,omitempty"`
	Duration    *float64 `json:"duration_ms,omitempty"`
	HookElapsed *float64 `json:"hook_elapsed_ms,omitempty"`
	Matched     *bool    `json:"matched_start,omitempty"`
	Dropped     uint64   `json:"dropped,omitempty"`
}

type callKey struct{ session, call string }

// Collector responds without waiting for disk I/O. Queue overload, malformed
// input and shutdown drop observation only; the hook never returns a decision.
type Collector struct {
	URL     string
	path    string
	session string
	server  *http.Server
	slots   chan struct{}
	queue   chan input
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	dropped atomic.Uint64
	once    sync.Once
}

func Start(logDir, session string) (*Collector, error) {
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(logDir, "tool-timing.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.Close()
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		ln.Close()
		f.Close()
		return nil, err
	}
	c := newCollector(f, session)
	c.path = "/hooks/" + hex.EncodeToString(token[:])
	c.URL = "http://" + ln.Addr().String() + c.path
	c.server = &http.Server{
		Handler: c, ReadHeaderTimeout: 250 * time.Millisecond, ReadTimeout: 500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go c.server.Serve(ln)
	return c, nil
}

func newCollector(w io.WriteCloser, session string) *Collector {
	c := &Collector{session: session, queue: make(chan input, queueSize), done: make(chan struct{}), slots: make(chan struct{}, 8)}
	go c.writeLoop(w)
	return c
}

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	at := time.Now()
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(c.path)) != 1 {
		http.NotFound(w, r)
		return
	}
	// A successful empty response has no effect on permissions or context.
	defer w.WriteHeader(http.StatusNoContent)
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		c.dropped.Add(1)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var in input
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || !validInput(in) {
		c.dropped.Add(1)
		return
	}
	in.at = at
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		select {
		case c.queue <- in:
		default:
			c.dropped.Add(1)
		}
	}
}

func validInput(in input) bool {
	if !validID(in.Session) || !validID(in.Call) || !validID(in.Tool) {
		return false
	}
	if in.Event != "PreToolUse" && in.Event != "PostToolUse" && in.Event != "PostToolUseFailure" {
		return false
	}
	return in.Duration == nil || (!math.IsNaN(*in.Duration) && !math.IsInf(*in.Duration, 0) && *in.Duration >= 0 && *in.Duration <= 1e12)
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.' || ch == ':' || ch == '/') {
			return false
		}
	}
	return true
}

func (c *Collector) writeLoop(w io.WriteCloser) {
	defer close(c.done)
	defer w.Close()
	pending := make(map[callKey]input)
	emit := func(r record) {
		r.Session = c.session
		r.Dropped += c.dropped.Swap(0)
		data, _ := json.Marshal(r)
		if _, err := w.Write(append(data, '\n')); err != nil {
			c.dropped.Add(r.Dropped + 1)
		}
	}
	for in := range c.queue {
		key := callKey{in.Session, in.Call}
		r := record{Time: in.at.Format(time.RFC3339Nano), Agent: in.Session, Call: in.Call, Tool: in.Tool}
		if in.Event == "PreToolUse" {
			r.Kind = "tool-start"
			if _, exists := pending[key]; !exists && len(pending) < maxPending {
				pending[key] = in
			} else {
				c.dropped.Add(1)
			}
		} else {
			r.Kind, r.Status, r.Duration = "tool-end", "completed", in.Duration
			if in.Event == "PostToolUseFailure" {
				r.Status = "failed"
				if in.Interrupt {
					r.Status = "interrupted"
				}
			}
			prev, ok := pending[key]
			matched := ok && prev.Tool == in.Tool
			r.Matched = &matched
			if matched {
				elapsed := float64(in.at.Sub(prev.at)) / float64(time.Millisecond)
				if elapsed >= 0 {
					r.HookElapsed = &elapsed
				}
				delete(pending, key)
			}
		}
		emit(r)
	}
	// A missing post hook is not a completed tool or an estimated duration.
	for _, in := range pending {
		emit(record{Time: time.Now().Format(time.RFC3339Nano), Kind: "tool-incomplete", Agent: in.Session, Call: in.Call, Tool: in.Tool, Status: "missing_post"})
	}
	if dropped := c.dropped.Swap(0); dropped != 0 {
		emit(record{Time: time.Now().Format(time.RFC3339Nano), Kind: "tool-stats-dropped", Dropped: dropped})
	}
}

func (c *Collector) Close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.server != nil {
			c.server.Close()
		}
		c.mu.Lock()
		c.closed = true
		close(c.queue)
		c.mu.Unlock()
		select {
		case <-c.done:
		case <-time.After(100 * time.Millisecond):
		}
	})
}
