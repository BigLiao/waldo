package netpolicy

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"waldo/internal/config"
)

func TestNativeProxyRequiresAuthAndStripsItForHTTPAndConnect(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("authentication leaked upstream")
		}
		io.WriteString(w, "upstream")
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	tlsServer := httptest.NewTLSServer(handler)
	defer tlsServer.Close()
	p, err := StartHTTPProxy(New(&config.Policy{Allow: []string{strings.TrimPrefix(httpServer.URL, "http://"), strings.TrimPrefix(tlsServer.URL, "https://")}, AllowPrivate: true}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	u, _ := url.Parse(p.URL)
	for _, auth := range []string{"", "wrong", "correct"} {
		proxyURL := *u
		switch auth {
		case "":
			proxyURL.User = nil
		case "wrong":
			proxyURL.User = url.UserPassword("waldo", "wrong")
		}
		transport := &http.Transport{Proxy: http.ProxyURL(&proxyURL), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		for _, target := range []string{httpServer.URL, tlsServer.URL} {
			resp, err := client.Get(target)
			if auth != "correct" {
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode != 407 {
						t.Errorf("unauthenticated status %d", resp.StatusCode)
					}
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(body) != "upstream" {
				t.Fatal(resp.StatusCode, string(body))
			}
		}
		transport.CloseIdleConnections()
	}
	p.Close()
	client := http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: time.Second}
	if resp, err := client.Get(httpServer.URL); err == nil {
		resp.Body.Close()
		t.Fatal("closed session proxy still available")
	}
}
