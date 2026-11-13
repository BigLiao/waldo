package session

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hashicorp/yamux"
	"waldo/internal/audit"
	"waldo/internal/paseo"
	"waldo/internal/proto"
	"waldo/internal/util"
)

func TestPaseoUsesExistingMultiplexedLink(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-only" || r.URL.RawQuery != "callerAgentId=owned-agent" {
			t.Error("caller identity changed")
		}
		io.WriteString(w, `{"tools":[]}`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL + paseo.Path + "?callerAgentId=owned-agent")
	broker := paseo.NewBroker(&paseo.Endpoint{URL: u, Headers: http.Header{"Authorization": []string{"Bearer local-only"}}})
	defer broker.Close()
	log, err := audit.Open(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	a, b := net.Pipe()
	client, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	go acceptFromB(client, &Sandbox{}, log, broker)
	gateway, err := paseo.StartGateway(func() (io.ReadWriteCloser, error) {
		st, err := remote.OpenStream()
		if err != nil {
			return nil, err
		}
		if err := proto.WriteMsg(st, proto.Header{Kind: proto.KindPaseo}); err != nil {
			st.Close()
			return nil, err
		}
		return st, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	r, _ := http.NewRequest("POST", gateway.URL+paseo.Path, strings.NewReader(`{"method":"tools/list"}`))
	r.Header.Set("Authorization", "Bearer "+gateway.Token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(data) != `{"tools":[]}` {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}
