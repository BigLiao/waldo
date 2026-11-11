package netpolicy

import (
	"net/netip"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		entry, host string
		port        int
		want        bool
	}{
		{"github.com", "github.com", 443, true},
		{"github.com", "GitHub.com.", 443, true},
		{"github.com", "github.com", 22, false},
		{"github.com", "api.github.com", 443, false},
		{"*.github.com", "api.github.com", 443, true},
		{"*.github.com", "github.com", 443, false},
		{"*.github.com", "evilgithub.com", 443, false},
		{"example.com:8443", "example.com", 8443, true},
		{"example.com:8443", "example.com", 443, false},
		{"registry.npmjs.org", "registry.npmjs.org.evil.com", 443, false},
	}
	for _, c := range cases {
		if got := Match(c.entry, c.host, c.port); got != c.want {
			t.Errorf("Match(%q, %q, %d) = %v", c.entry, c.host, c.port, got)
		}
	}
}

func TestPrivate(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.64.1.1", "::ffff:127.0.0.1", "fe80::1", "0.0.0.0"} {
		if !private(netip.MustParseAddr(a)) {
			t.Errorf("%s should be private", a)
		}
	}
	for _, a := range []string{"140.82.112.3", "2606:4700::1"} {
		if private(netip.MustParseAddr(a)) {
			t.Errorf("%s should be public", a)
		}
	}
}
