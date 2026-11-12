package session

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/term"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// Use a real tty in a subprocess: a plain writer test cannot catch a
// missing cleanup on one of the relay's return paths or unrestored termios.
func TestRelayRestoresTerminal(t *testing.T) {
	for _, ending := range []string{"disconnect", "exit", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalRelayChild$")
			cmd.Env = append(os.Environ(), "WALDO_TEST_TERMINAL_END="+ending)
			master, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			var out []byte
			var sent bool
			for {
				var buf [4096]byte
				n, err := master.Read(buf[:])
				out = append(out, buf[:n]...)
				if ending == "cancel" && !sent && bytes.Contains(out, []byte("ready")) {
					master.Write([]byte{3})
					sent = true
				}
				if err != nil {
					break
				} // Linux pty masters return EIO at slave close.
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("relay child: %v\n%s", err, out)
			}
			s := string(out)
			enabled := strings.Index(s, "\x1b[?1003h")
			for _, reset := range []string{"\x1b[?1003l", "\x1b[?1006l", "\x1b[?2004l", "\x1b[?25h"} {
				at := strings.Index(s, reset)
				if enabled < 0 || at <= enabled {
					t.Errorf("terminal mode %q not restored after agent output: %q", reset, s)
				}
				if ending == "disconnect" && at > strings.Index(s, "与 B 的连接中断") {
					t.Errorf("terminal restored after disconnect warning: %q", s)
				}
			}
		})
	}
}

func TestTerminalRelayChild(t *testing.T) {
	ending := os.Getenv("WALDO_TEST_TERMINAL_END")
	if ending == "" {
		return
	}
	before, err := term.GetState(0)
	if err != nil {
		t.Fatal(err)
	}
	var frames bytes.Buffer
	fw := proto.NewFrameWriter(&frames)
	fw.Write(proto.FStdout, []byte("\x1b[?1003h\x1b[?1006h\x1b[?2004h\x1b[?25lready"))
	want := 255
	if ending == "exit" {
		fw.WriteJSON(proto.FExit, proto.Exit{Code: 7})
		want = 7
	}
	var input io.Reader = &frames
	var rc *relayReconnect
	if ending == "cancel" {
		r, w := io.Pipe()
		defer r.Close()
		defer w.Close()
		go io.Copy(w, &frames)
		input = r
		rc = &relayReconnect{cancel: func() bool { r.Close(); return true }}
	}
	if got := relayAgentIOCancelable(util.RWC{R: input, W: io.Discard}, true, rc); got != want {
		t.Errorf("exit code %d, want %d", got, want)
	}
	after, err := term.GetState(0)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("tty attributes not restored: %v", err)
	}
}

func TestReconnectNoticeStaysOnBottomRow(t *testing.T) {
	down := reconnectNotice(24, 40, false)
	if !strings.HasPrefix(down, "\x1b7\x1b[24;1H\x1b[2K") || !strings.HasSuffix(down, "\x1b8") {
		t.Fatalf("notice must save cursor, draw on the last row and restore: %q", down)
	}
	if strings.ContainsAny(down, "\r\n") {
		t.Fatalf("notice must not scroll the agent screen: %q", down)
	}
	if got := reconnectNotice(24, 40, true); got != "\x1b7\x1b[24;1H\x1b[2K\x1b8" {
		t.Fatalf("recovery should only clear the row: %q", got)
	}
	if got := truncateCells("ab连接", 5); got != "ab连" {
		t.Fatalf("truncateCells = %q", got)
	}
}
