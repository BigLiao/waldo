package netpolicy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"waldo/internal/proto"
)

// HTTPProxy is a session-authenticated native loopback proxy. The token is
// intentionally absent from Facts and the A/B protocol.
type HTTPProxy struct {
	Port      int
	URL       string
	server    *http.Server
	transport *http.Transport
	active    sync.Map
	once      sync.Once
	mu        sync.Mutex
	closed    bool
}

func StartHTTPProxy(policy *Policy) (*HTTPProxy, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(token[:])
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &HTTPProxy{Port: ln.Addr().(*net.TCPAddr).Port}
	p.URL = (&url.URL{Scheme: "http", Host: ln.Addr().String(), User: url.UserPassword("waldo", secret)}).String()
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("waldo:"+secret))
	dial := func(ctx context.Context, _, addr string) (net.Conn, error) {
		host, ps, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(ps)
		if err != nil {
			return nil, err
		}
		client, server := net.Pipe()
		go policy.HandleConnect(server, host, port)
		stop := context.AfterFunc(ctx, func() { client.Close() })
		defer stop()
		var reply proto.Reply
		if err := proto.ReadMsg(client, &reply); err != nil {
			client.Close()
			return nil, err
		}
		if !reply.OK {
			client.Close()
			return nil, fmt.Errorf("%s", reply.Msg)
		}
		return client, nil
	}
	p.transport = &http.Transport{Proxy: nil, DialContext: dial, IdleConnTimeout: 30 * time.Second}
	p.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(auth)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="waldo"`)
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		if r.Method == http.MethodConnect {
			upstream, err := dial(r.Context(), "tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				conn.Close()
				upstream.Close()
				return
			}
			p.active.Store(conn, upstream)
			p.mu.Unlock()
			defer func() { p.active.Delete(conn); conn.Close(); upstream.Close() }()
			io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
			// Preserve bytes already buffered by net/http.
			go func() { io.Copy(upstream, rw.Reader); upstream.Close() }()
			io.Copy(conn, upstream)
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "expected proxy request", 400)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		for _, h := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
			out.Header.Del(h)
		}
		resp, err := p.transport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		defer resp.Body.Close()
		for k, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})}
	go p.server.Serve(ln)
	return p, nil
}

func (p *HTTPProxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.server.Close()
		p.active.Range(func(k, v any) bool { k.(net.Conn).Close(); v.(net.Conn).Close(); return true })
		p.transport.CloseIdleConnections()
	})
}
