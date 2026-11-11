package serve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// startProxy listens on 127.0.0.1:ProxyPort inside the sandbox. Every
// connection is handed to the host waldo process, which applies the domain
// allow-list and makes the real connection.
func (s *server) startProxy() error {
	if s.cfg.ProxyPort == 0 {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.cfg.ProxyPort))
	if err != nil {
		return err
	}
	s.markOwn(s.cfg.ProxyPort)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			p, _ := strconv.Atoi(port)
			st, err := s.open(proto.Header{Kind: proto.KindConnect, Host: host, Port: p})
			if err != nil {
				return nil, err
			}
			return st, nil
		},
		Proxy:               nil,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	srv := &http.Server{Handler: &proxyHandler{s: s, tr: tr}, ErrorLog: log.New(io.Discard, "", 0)}
	go srv.Serve(ln)
	return nil
}

type proxyHandler struct {
	s  *server
	tr *http.Transport
}

func deny(w http.ResponseWriter, err error) {
	msg := err.Error()
	var d *deniedError
	code := http.StatusBadGateway
	if errors.As(err, &d) {
		code = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Waldo-Denied", "1")
	w.WriteHeader(code)
	io.WriteString(w, msg+"\n")
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, "bad CONNECT target", http.StatusBadRequest)
			return
		}
		pn, _ := strconv.Atoi(port)
		st, err := p.s.open(proto.Header{Kind: proto.KindConnect, Host: host, Port: pn})
		if err != nil {
			deny(w, err)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			st.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		c, brw, err := hj.Hijack()
		if err != nil {
			st.Close()
			return
		}
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if n := brw.Reader.Buffered(); n > 0 {
			b, _ := brw.Reader.Peek(n)
			st.Write(b)
		}
		util.Splice(st, c.(*net.TCPConn))
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "waldo 沙箱代理只接受代理请求", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		deny(w, unwrapDenied(err))
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func unwrapDenied(err error) error {
	var d *deniedError
	if errors.As(err, &d) {
		return d
	}
	return err
}

// scanPorts reports the set of TCP ports listening on loopback/any inside the
// sandbox network namespace whenever it changes.
func (s *server) scanPorts(ctl io.Writer) {
	var last string
	for {
		ports := listeningPorts()
		var keep []int
		for _, p := range ports {
			if !s.isOwn(p) {
				keep = append(keep, p)
			}
		}
		key := fmt.Sprint(keep)
		if key != last {
			last = key
			if err := proto.WriteMsg(ctl, proto.PortEvent{Ports: keep}); err != nil {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func listeningPorts() []int {
	set := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != "0A" { // LISTEN
				continue
			}
			addr, port, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if !loopbackOrAny(addr) {
				continue
			}
			if n, err := strconv.ParseUint(port, 16, 16); err == nil {
				set[int(n)] = true
			}
		}
		fh.Close()
	}
	var out []int
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func loopbackOrAny(hexaddr string) bool {
	switch hexaddr {
	case "00000000", "0100007F", // 0.0.0.0, 127.0.0.1
		"00000000000000000000000000000000", // ::
		"00000000000000000000000001000000", // ::1
		"0000000000000000FFFF00000100007F", // ::ffff:127.0.0.1
		"0000000000000000FFFF000000000000":
		return true
	}
	return false
}
