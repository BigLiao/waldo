package host

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"waldo/internal/proto"
)

// Hold the first output frame until the child has exited and been reaped.
// Its final write must remain readable after exec.Cmd.Wait returns.
type gatedAgentOutput struct {
	net.Conn
	started, release chan struct{}
	once             sync.Once
}

func (c *gatedAgentOutput) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return c.Conn.Write(p)
}

func TestAgentDrainsOutputAfterWait(t *testing.T) {
	client, remote := net.Pipe()
	defer client.Close()
	defer remote.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	stream := &gatedAgentOutput{Conn: remote, started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(stream.release) })
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `printf 'ready\n'; IFS= read -r reply; printf 'final %s\n' "$reply"; printf 'final stderr\n' >&2; exit 7`)
	h := &host{}
	finished := make(chan int, 1)
	go func() { finished <- h.runAgent(cmd, stream) }()
	select {
	case <-stream.started:
	case <-ctx.Done():
		t.Fatal("agent did not produce its initial output")
	}
	if err := proto.NewFrameWriter(client).Write(proto.FStdin, []byte("continued\n")); err != nil {
		t.Fatal(err)
	}
	waitReconnect(t, func() bool { return syscall.Kill(cmd.Process.Pid, 0) == syscall.ESRCH })
	release()
	var stdout, stderr strings.Builder
	for {
		typ, p, err := proto.ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		switch typ {
		case proto.FStdout:
			stdout.Write(p)
		case proto.FStderr:
			stderr.Write(p)
		case proto.FExit:
			if stdout.String() != "ready\nfinal continued\n" || stderr.String() != "final stderr\n" {
				t.Fatalf("lost output after Wait: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if code := <-finished; code != 7 {
				t.Fatalf("exit %d, want 7", code)
			}
			return
		}
	}
}
