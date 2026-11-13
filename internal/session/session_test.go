package session

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/audit"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// B cannot ask A to make an arbitrary outbound connection or dial a local
// port. Optional streams also stay closed unless enabled for the session.
func TestRejectUnknownStreamsFromB(t *testing.T) {
	a, b := net.Pipe()
	ca, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	sb, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	go acceptFromB(ca, &Sandbox{}, nil)
	for _, kind := range []string{proto.KindConnect, proto.KindDial, proto.KindHostPort, proto.KindSandbox, proto.KindPaseo, proto.KindFSWatch, "bogus"} {
		st, err := sb.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		proto.WriteMsg(st, proto.Header{Kind: kind, Host: "example.com", Port: 22})
		st.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 1)
		n, err := st.Read(buf)
		if n != 0 || err == nil || os.IsTimeout(err) {
			t.Errorf("%s: stream not closed by A (n=%d err=%v)", kind, n, err)
		}
	}
}

func TestCheckProject(t *testing.T) {
	if CheckProject("/home/u", "/home/u") == nil || CheckProject("/", "/home/u") == nil || CheckProject("/home", "/home/u") == nil {
		t.Error("too-wide project dirs must be refused")
	}
	if CheckProject("/home/u/work/p", "/home/u") != nil {
		t.Error("normal project refused")
	}
}

func TestRejectCacheWatchOutsideProject(t *testing.T) {
	a, b := net.Pipe()
	trusted, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	remote, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	go acceptFromB(trusted, &Sandbox{Project: "/project", ContentCacheTTL: time.Second}, nil)
	for _, header := range []proto.Header{
		{Kind: proto.KindFSWatch, Path: "/"},
		{Kind: proto.KindFSWatch, Path: "/project/../other"},
		{Kind: proto.KindFSWatch, Path: "/project", Host: "example.com"},
		{Kind: proto.KindFSWatch, Path: "/project", Exec: &proto.ExecReq{Path: "/bin/sh"}},
	} {
		st, err := remote.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.WriteMsg(st, header); err != nil {
			t.Fatal(err)
		}
		st.SetReadDeadline(time.Now().Add(time.Second))
		var one [1]byte
		if n, err := st.Read(one[:]); n != 0 || err == nil || os.IsTimeout(err) {
			t.Fatalf("unauthorized cache watch accepted: %+v: %d %v", header, n, err)
		}
		st.Close()
	}
}

func TestOnlyOneCacheWatchMaySubscribePerSession(t *testing.T) {
	pair := func() (*yamux.Session, *yamux.Session) {
		a, b := net.Pipe()
		client, err := yamux.Client(a, util.YamuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		server, err := yamux.Server(b, util.YamuxConfig())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close(); server.Close() })
		return client, server
	}
	trusted, remote := pair()
	sandboxLink, sandboxPeer := pair()
	dataDir := t.TempDir()
	logger, err := audit.Open(dataDir, "watch-test")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	go acceptFromB(trusted, &Sandbox{Project: "/project", ContentCacheTTL: time.Second, sess: sandboxLink}, logger)
	headers := make(chan proto.Header, 1)
	go func() {
		st, err := sandboxPeer.AcceptStream()
		if err != nil {
			return
		}
		var header proto.Header
		if proto.ReadMsg(st, &header) == nil {
			headers <- header
			st.Write([]byte{1})
		}
	}()
	first, err := remote.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := proto.WriteMsg(first, proto.Header{Kind: proto.KindFSWatch, Path: "/project"}); err != nil {
		t.Fatal(err)
	}
	first.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := first.Read(one[:]); err != nil {
		t.Fatalf("first watch failed: %v", err)
	}
	if got := <-headers; got != (proto.Header{Kind: proto.KindFSWatch, Path: "/project"}) {
		t.Fatalf("watch header not reconstructed from session policy: %+v", got)
	}
	duplicate, err := remote.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	if err := proto.WriteMsg(duplicate, proto.Header{Kind: proto.KindFSWatch, Path: "/project"}); err != nil {
		t.Fatal(err)
	}
	duplicate.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := duplicate.Read(one[:]); n != 0 || err == nil || os.IsTimeout(err) {
		t.Fatalf("duplicate cache watch was not rejected: %d %v", n, err)
	}
	log, err := os.ReadFile(audit.Path(dataDir))
	if err != nil || !strings.Contains(string(log), `"kind":"fs-watch"`) {
		t.Fatalf("accepted watch missing audit event: %s %v", log, err)
	}
}
