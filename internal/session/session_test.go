package session

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// A compromised B may only open fs and exec streams; anything else (e.g.
// asking A to make an outbound connection or dial a local port) is refused.
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
	for _, kind := range []string{proto.KindConnect, proto.KindDial, proto.KindHostPort, proto.KindSandbox, "bogus"} {
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
