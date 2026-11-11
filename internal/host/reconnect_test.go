package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/proto"
	"waldo/internal/reconnect"
	"waldo/internal/util"
)

func TestMain(m *testing.M) {
	// Let the detached owner/attach helpers execute this test binary using
	// their real production entry point, without compiling an extra waldo.
	if len(os.Args) > 1 && os.Args[1] == "host" {
		os.Exit(Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func waitReconnect(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("reconnect condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAgentProcessSurvivesReconnect(t *testing.T) {
	a, b := reconnect.New(3*time.Second), reconnect.New(3*time.Second)
	defer a.Close()
	defer b.Close()
	attach := func() net.Conn {
		x, y := net.Pipe()
		go a.Serve(x)
		go b.Serve(y)
		waitReconnect(t, func() bool { return a.Connected() && b.Connected() })
		return x
	}
	physical := attach()
	client, _ := yamux.Client(a, util.ResumableYamuxConfig())
	server, _ := yamux.Server(b, util.ResumableYamuxConfig())
	defer client.Close()
	defer server.Close()
	st, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `printf 'ready %s\n' "$$"; IFS= read -r reply; printf 'resumed %s %s\n' "$$" "$reply"; exit 7`)
	h := &host{}
	finished := make(chan int, 1)
	go func() { finished <- h.runAgent(cmd, remote) }()
	var out strings.Builder
	for !strings.Contains(out.String(), "ready ") {
		typ, p, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		if typ == proto.FStdout {
			out.Write(p)
		}
	}
	pid := cmd.Process.Pid
	physical.Close()
	waitReconnect(t, func() bool { return !a.Connected() && !b.Connected() })
	proto.NewFrameWriter(st).Write(proto.FStdin, []byte("continued\n"))
	attach()
	for {
		typ, p, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		if typ == proto.FStdout {
			out.Write(p)
		}
		if typ == proto.FExit {
			var ex proto.Exit
			proto.DecodeJSON(p, &ex)
			if ex.Code != 7 {
				t.Fatalf("exit %d", ex.Code)
			}
			break
		}
	}
	want := "ready " + strconv.Itoa(pid) + "\nresumed " + strconv.Itoa(pid) + " continued\n"
	if out.String() != want {
		t.Fatalf("agent restarted or output replayed: %q, want %q", out.String(), want)
	}
	if code := <-finished; code != 7 {
		t.Fatalf("host exit %d", code)
	}
}

func TestAgentExitsAfterReconnectWindow(t *testing.T) {
	a, b := reconnect.New(300*time.Millisecond), reconnect.New(300*time.Millisecond)
	defer a.Close()
	defer b.Close()
	x, y := net.Pipe()
	go a.Serve(x)
	go b.Serve(y)
	client, _ := yamux.Client(a, util.ResumableYamuxConfig())
	server, _ := yamux.Server(b, util.ResumableYamuxConfig())
	defer client.Close()
	defer server.Close()
	st, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "echo ready; read reply")
	h := &host{}
	finished := make(chan int, 1)
	go func() { finished <- h.runAgent(cmd, remote) }()
	if _, _, err := proto.ReadFrame(st); err != nil {
		t.Fatal(err)
	}
	x.Close()
	select {
	case code := <-finished:
		if code != 137 {
			t.Fatalf("agent exit %d, want killed process (137)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent survived expired reconnect window")
	}
}

type attachProcess struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.Reader
}

func (p *attachProcess) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *attachProcess) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *attachProcess) Close() error                { p.in.Close(); p.cmd.Process.Kill(); return nil }

func TestDetachedOwnerSurvivesAttachDeath(t *testing.T) {
	var random [32]byte
	rand.Read(random[:])
	token := hex.EncodeToString(random[:])
	dir := t.TempDir()
	link := reconnect.New(3 * time.Second)
	defer link.Close()
	start := func(initial bool) *attachProcess {
		args := []string{"host", "--data-dir", dir, "--reconnect-attach", token, "--reconnect-window", "3s"}
		if initial {
			args = append(args, "--reconnect-start")
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.Stderr = os.Stderr
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(out)
		banner, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(banner, proto.BannerPrefix) {
			t.Fatalf("attach: %q %v", banner, err)
		}
		p := &attachProcess{cmd: cmd, in: in, out: br}
		t.Cleanup(func() { p.Close(); cmd.Wait() })
		go link.Serve(p)
		waitReconnect(t, link.Connected)
		return p
	}
	first := start(true)
	client, err := yamux.Client(link, util.ResumableYamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	first.Close()
	waitReconnect(t, func() bool { return !link.Connected() })
	start(false)
	if _, err := client.Ping(); err != nil {
		t.Fatalf("same yamux session did not survive attach death: %v", err)
	}
	// Reject a version before preparing mounts; this exercises graceful exit
	// delivery and owner/socket cleanup without depending on /dev/fuse.
	ctl, _ := client.OpenStream()
	proto.WriteMsg(ctl, proto.Header{Kind: proto.KindCtl})
	proto.WriteMsg(ctl, proto.Hello{Version: "incompatible"})
	var ack proto.HelloAck
	if err := proto.ReadMsg(ctl, &ack); err != nil || ack.Error == "" {
		t.Fatalf("missing final reply: %+v %v", ack, err)
	}
	path, _ := reconnectSocket(token)
	waitReconnect(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
}

func TestOwnerExpiresWithoutAttachment(t *testing.T) {
	var random [32]byte
	rand.Read(random[:])
	token := hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "host", "--data-dir", t.TempDir(), "--reconnect-serve", token, "--reconnect-window", "150ms")
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err == nil || ctx.Err() != nil || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("owner did not exit at window expiry: %v, context=%v, %s", err, ctx.Err(), out)
	}
	path, _ := reconnectSocket(token)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired owner left a socket: %v", err)
	}
}
