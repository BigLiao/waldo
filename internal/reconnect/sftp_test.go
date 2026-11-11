package reconnect_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/pkg/sftp"

	"waldo/internal/reconnect"
	"waldo/internal/util"
)

func TestOpenSFTPHandleSurvivesReconnect(t *testing.T) {
	a, b := reconnect.New(3*time.Second), reconnect.New(3*time.Second)
	defer a.Close()
	defer b.Close()
	attach := func() net.Conn {
		x, y := net.Pipe()
		go a.Serve(x)
		go b.Serve(y)
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
	fs, err := sftp.NewServer(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	go fs.Serve()
	sc, err := sftp.NewClientPipe(st, st)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { fs.Close(); client.Close(); sc.Close() }()
	path := filepath.Join(t.TempDir(), "append-once")
	f, err := sc.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}
	physical.Close()
	done := make(chan error, 1)
	go func() { _, err := f.Write([]byte("after\n")); done <- err }()
	attach()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SFTP write did not resume")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "before\nafter\n" {
		t.Fatalf("SFTP handle lost or append replayed: %q %v", got, err)
	}
}
