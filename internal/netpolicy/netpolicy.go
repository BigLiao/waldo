// Package netpolicy enforces the network policy on A's host: which domains
// the sandbox may reach, which host services it may use, and which sandbox
// ports appear on the host's 127.0.0.1. It runs in the trusted waldo process.
package netpolicy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// Policy decides outbound access. The dynamic part (`waldo allow`) is
// re-read whenever allowed.json changes, so it takes effect immediately.
type Policy struct {
	P     *config.Policy
	Audit *audit.Logger

	mu       sync.Mutex
	dynMtime time.Time
	dyn      []string

	// Resolver and Dial are overridable for tests.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, addr string) (net.Conn, error)
}

func New(p *config.Policy, a *audit.Logger) *Policy {
	return &Policy{
		P: p, Audit: a,
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
}

func (p *Policy) dynamic() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := os.Stat(config.AllowedPath())
	if err != nil {
		p.dyn = nil
		return nil
	}
	if !st.ModTime().Equal(p.dynMtime) {
		p.dynMtime = st.ModTime()
		a, err := config.LoadAllowed()
		if err == nil {
			p.dyn = append(append([]string{}, a["*"]...), a[p.P.Project]...)
		}
	}
	return p.dyn
}

// Match reports whether an allow-list entry matches host:port.
// Entries: "example.com" (ports 80/443), "example.com:8443", "*.example.com".
func Match(entry, host string, port int) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	ep := 0
	if h, ps, err := net.SplitHostPort(entry); err == nil {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return false
		}
		entry, ep = h, n
	}
	if ep == 0 {
		if port != 80 && port != 443 {
			return false
		}
	} else if ep != port {
		return false
	}
	if strings.HasPrefix(entry, "*.") {
		suf := entry[1:]
		return strings.HasSuffix(host, suf) && len(host) > len(suf)
	}
	return host == entry
}

// Allowed reports whether host:port may be reached.
func (p *Policy) Allowed(host string, port int) bool {
	for _, e := range p.P.Allow {
		if Match(e, host, port) {
			return true
		}
	}
	for _, e := range p.dynamic() {
		if Match(e, host, port) {
			return true
		}
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// private reports addresses the sandbox must never reach through the proxy
// (the host itself, the LAN, link-local metadata services).
func private(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a)
}

func hint(host string) string {
	return fmt.Sprintf("waldo: 出网被拒绝：%s 不在白名单中。如需放行，请在宿主机上运行：waldo allow %s", host, host)
}

// HandleConnect serves a KindConnect stream from the sandbox.
func (p *Policy) HandleConnect(st io.ReadWriteCloser, host string, port int) {
	reply := func(ok bool, msg string) error { return proto.WriteMsg(st, proto.Reply{OK: ok, Msg: msg}) }
	if host == "" || port <= 0 || port > 65535 {
		reply(false, "waldo: 无效的目标")
		st.Close()
		return
	}
	if !p.Allowed(host, port) {
		p.Audit.Log("net-deny", map[string]any{"host": host, "port": port})
		reply(false, hint(host))
		st.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		var err error
		addrs, err = p.Resolve(ctx, host)
		if err != nil || len(addrs) == 0 {
			reply(false, fmt.Sprintf("waldo: 无法解析 %s: %v", host, err))
			st.Close()
			return
		}
	}
	var conn net.Conn
	var lastErr error
	for _, a := range addrs {
		if private(a) && !p.P.AllowPrivate {
			lastErr = fmt.Errorf("%s 解析到内网/本机地址 %s，已拒绝", host, a)
			continue
		}
		c, err := p.Dial(ctx, net.JoinHostPort(a.Unmap().String(), strconv.Itoa(port)))
		if err == nil {
			conn = c
			break
		}
		lastErr = err
	}
	if conn == nil {
		p.Audit.Log("net-deny", map[string]any{"host": host, "port": port, "err": fmt.Sprint(lastErr)})
		reply(false, fmt.Sprintf("waldo: 连接 %s:%d 失败：%v", host, port, lastErr))
		st.Close()
		return
	}
	p.Audit.Log("net", map[string]any{"host": host, "port": port, "ip": conn.RemoteAddr().String()})
	if err := reply(true, ""); err != nil {
		conn.Close()
		st.Close()
		return
	}
	util.Splice(st, conn.(io.ReadWriteCloser))
}

// HandleHostPort serves a KindHostPort stream: the sandbox wants a service
// that runs on the host (only ports listed in host_ports).
func (p *Policy) HandleHostPort(st io.ReadWriteCloser, port int) {
	ok := false
	for _, hp := range p.P.HostPorts {
		if hp == port {
			ok = true
		}
	}
	if !ok {
		proto.WriteMsg(st, proto.Reply{OK: false, Msg: "port not allowed"})
		st.Close()
		return
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		proto.WriteMsg(st, proto.Reply{OK: false, Msg: err.Error()})
		st.Close()
		return
	}
	p.Audit.Log("hostport", map[string]any{"port": port})
	if proto.WriteMsg(st, proto.Reply{OK: true}) != nil {
		c.Close()
		st.Close()
		return
	}
	util.Splice(st, c)
}
