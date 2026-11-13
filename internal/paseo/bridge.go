package paseo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const maxBody = 16 << 20

// Broker lives on trusted A. A request cannot select its upstream, path,
// caller identity, or real authentication headers.
type Broker struct {
	endpoint *Endpoint
	client   *http.Client
	ctx      context.Context
	cancel   context.CancelFunc
	slots    chan struct{}
	mu       sync.Mutex
	sessions map[string]bool
}

func NewBroker(endpoint *Endpoint) *Broker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Broker{endpoint: endpoint, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 32), sessions: map[string]bool{}, client: &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 8},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *Broker) Close() {
	b.cancel()
	b.client.CloseIdleConnections()
}

func writeError(w io.Writer, status int) {
	text := http.StatusText(status)
	r := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(bytes.NewBufferString(text)), ContentLength: int64(len(text)), ProtoMajor: 1, ProtoMinor: 1, Close: true}
	r.Write(w)
}

// Serve handles one HTTP exchange carried by one yamux stream. MCP remains
// end-to-end between Claude and Paseo, including SSE and cancellation messages.
func (b *Broker) Serve(stream io.ReadWriteCloser) {
	defer stream.Close()
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		writeError(stream, 429)
		return
	}
	if d, ok := stream.(interface{ SetReadDeadline(time.Time) error }); ok {
		d.SetReadDeadline(time.Now().Add(30 * time.Second))
		defer d.SetReadDeadline(time.Time{})
	}
	r, err := http.ReadRequest(bufio.NewReader(io.LimitReader(stream, maxBody+(64<<10))))
	if err != nil {
		writeError(stream, 400)
		return
	}
	defer r.Body.Close()
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(stream, 405)
		return
	}
	if r.URL.Path != Path || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.IsAbs() || r.Host != "waldo-paseo" {
		writeError(stream, 403)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeError(stream, 413)
		return
	}
	if d, ok := stream.(interface{ SetReadDeadline(time.Time) error }); ok {
		d.SetReadDeadline(time.Time{})
	}
	sid := r.Header.Get("Mcp-Session-Id")
	if sid != "" {
		b.mu.Lock()
		known := b.sessions[sid]
		b.mu.Unlock()
		if !known {
			writeError(stream, 403)
			return
		}
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Minute)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, r.Method, b.endpoint.URL.String(), bytes.NewReader(body))
	if err != nil {
		writeError(stream, 502)
		return
	}
	for _, k := range []string{"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} {
		if v := r.Header.Values(k); len(v) > 0 {
			up.Header[k] = append([]string(nil), v...)
		}
	}
	for k, v := range b.endpoint.Headers {
		up.Header[k] = append([]string(nil), v...)
	}
	resp, err := b.client.Do(up)
	if err != nil {
		writeError(stream, 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		writeError(stream, 502)
		return
	}
	if next := resp.Header.Get("Mcp-Session-Id"); next != "" {
		b.mu.Lock()
		b.sessions[next] = true
		b.mu.Unlock()
	}
	if (r.Method == http.MethodDelete && resp.StatusCode < 300) || resp.StatusCode == 404 {
		b.mu.Lock()
		delete(b.sessions, sid)
		b.mu.Unlock()
	}
	// Return only protocol headers, never upstream cookies or redirect targets.
	headers := make(http.Header)
	for _, k := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Retry-After", "Cache-Control"} {
		if v := resp.Header.Values(k); len(v) > 0 {
			headers[k] = append([]string(nil), v...)
		}
	}
	resp.Header = headers
	resp.Close = true
	resp.Write(stream)
}

// Gateway is B's authenticated, ephemeral loopback listener. The only way
// back to A is an explicitly typed stream on the already established link.
type Gateway struct {
	server     *http.Server
	listener   net.Listener
	URL, Token string
}

func StartGateway(open func() (io.ReadWriteCloser, error)) (*Gateway, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		ln.Close()
		return nil, err
	}
	g := &Gateway{listener: ln, URL: "http://" + ln.Addr().String(), Token: hex.EncodeToString(token)}
	g.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + g.Token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "Forbidden", 403)
			return
		}
		if r.URL.Path != Path || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.Error(w, "Forbidden", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != g.URL {
			http.Error(w, "Forbidden", 403)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			http.Error(w, "Method not allowed", 405)
			return
		}
		stream, err := open()
		if err != nil {
			http.Error(w, "Paseo bridge unavailable", 502)
			return
		}
		defer stream.Close()
		stop := context.AfterFunc(r.Context(), func() { stream.Close() })
		defer stop()
		out := r.Clone(r.Context())
		out.URL.Scheme = ""
		out.URL.Host = ""
		out.Host = "waldo-paseo"
		out.RequestURI = ""
		out.Header = r.Header.Clone()
		out.Header.Del("Authorization")
		out.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := out.Write(stream); err != nil {
			http.Error(w, "Paseo bridge request failed", 502)
			return
		}
		resp, err := http.ReadResponse(bufio.NewReader(stream), out)
		if err != nil {
			http.Error(w, "Paseo bridge response failed", 502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.Copy(flushWriter{w}, resp.Body)
	})}
	go g.server.Serve(ln)
	return g, nil
}

func (g *Gateway) Close() { g.server.Close() }

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (e *Endpoint) String() string {
	return fmt.Sprintf("Paseo MCP for %s", e.URL.Query().Get("callerAgentId"))
}
