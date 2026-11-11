package transport

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// Exercise the real OpenResumable retry loop and real detached host. A local
// ssh facade executes the remote command, avoiding FUSE and SSH credentials.
func TestSSHReconnectKeepsControlStream(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real waldo helper")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "waldo")
	buildID := strings.TrimPrefix(proto.FullVersion(), proto.Version+"+")
	build := exec.Command("go", "build", "-ldflags", "-X waldo/internal/proto.Build="+buildID+" -X waldo/internal/proto.Version="+proto.Version, "-o", bin, "../../cmd/waldo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	pids := filepath.Join(dir, "ssh-pids")
	t.Setenv("WALDO_TEST_SSH_PIDS", pids)
	fakeSSH(t, `for remote do :; done
printf '%s\n' "$$" >> "$WALDO_TEST_SSH_PIDS"
exec /bin/sh -c "$remote"
`)
	tr := &SSH{Host: "local-test", RemoteDir: dir, Stderr: io.Discard}
	conn, err := tr.OpenResumable()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	session, err := yamux.Client(conn.RW, util.ResumableYamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Ping(); err != nil {
		t.Fatal(err)
	}
	ctl, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	ctl.SetReadDeadline(time.Now().Add(15 * time.Second))
	if err := proto.WriteMsg(ctl, proto.Header{Kind: proto.KindCtl}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(pids)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("ssh pid %q: %v", data, err)
	}
	process, _ := os.FindProcess(pid)
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	// The already-open control stream must continue on the same host after
	// the retry loop reconnects. A new host cannot interpret this mid-stream.
	if err := proto.WriteMsg(ctl, proto.Hello{Version: "incompatible"}); err != nil {
		t.Fatal(err)
	}
	var ack proto.HelloAck
	if err := proto.ReadMsg(ctl, &ack); err != nil || ack.Error == "" {
		t.Fatalf("original stream did not resume: %+v %v", ack, err)
	}
	data, _ = os.ReadFile(pids)
	if len(strings.Fields(string(data))) < 2 {
		t.Fatalf("no replacement SSH connection: %q", data)
	}
}
