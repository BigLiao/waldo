package projection

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"waldo/internal/fileversion"
)

func connectVersion(t *testing.T, v *View) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan error, 1)
	go func() { done <- v.Serve(b) }()
	t.Cleanup(func() {
		a.Close()
		b.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("version server did not stop")
		}
	})
	if err := writePacket(a, binary.BigEndian.AppendUint32([]byte{1}, 3)); err != nil {
		t.Fatal(err)
	}
	if p, err := readPacket(a); err != nil || p[0] != 2 {
		t.Fatalf("INIT: %x %v", p, err)
	}
	return a
}
func versionExchange(t *testing.T, c net.Conn, p []byte) []byte {
	t.Helper()
	if err := writePacket(c, p); err != nil {
		t.Fatal(err)
	}
	r, err := readPacket(c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func checkedOpen(t *testing.T, c net.Conn, path string) (string, fileversion.Info) {
	t.Helper()
	body := replaceString(nil, fileversion.Open, nil)
	body = replaceString(body, path, nil)
	r := versionExchange(t, c, versionReply(200, 1, body))
	if r[0] != fileversion.Reply {
		t.Fatalf("checked OPEN: %x", r)
	}
	h, rest, err := packetString(r[5:])
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := packetString(rest)
	if err != nil {
		t.Fatal(err)
	}
	info, err := fileversion.Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	return h, info
}

func TestVersionExtensionUsesDescriptorAndCannotWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source")
	os.WriteFile(path, []byte("original"), 0600)
	stamp := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var v *View
	c := connectVersion(t, v)
	h, info := checkedOpen(t, c, path)
	if info.ModifiedNS != stamp.UnixNano() || info.Size != 8 {
		t.Fatalf("lost precision: %+v", info)
	}
	for _, kind := range []byte{6, 10} {
		body := replaceString(nil, h, nil)
		body = binary.BigEndian.AppendUint64(body, 0)
		body = replaceString(body, "bad", nil)
		r := versionExchange(t, c, versionReply(kind, 2, body))
		if r[0] != 101 || binary.BigEndian.Uint32(r[5:9]) != 3 {
			t.Fatalf("read-only handle accepted mutation: %x", r)
		}
	}
	// Renaming the path must not switch an already opened descriptor to a new inode.
	replacement := filepath.Join(root, "new")
	os.WriteFile(replacement, []byte("replacement"), 0600)
	os.Rename(replacement, path)
	body := replaceString(nil, h, nil)
	body = binary.BigEndian.AppendUint64(body, 0)
	body = binary.BigEndian.AppendUint32(body, 32)
	r := versionExchange(t, c, versionReply(5, 3, body))
	text, _, err := packetString(r[5:])
	if err != nil || r[0] != 103 || text != "original" {
		t.Fatalf("descriptor changed: %x %v", r, err)
	}
	body = replaceString(nil, fileversion.Close, nil)
	body = replaceString(body, h, nil)
	r = versionExchange(t, c, versionReply(200, 4, body))
	if r[0] != fileversion.Reply {
		t.Fatalf("checked close: %x", r)
	}
	text, _, err = packetString(r[5:])
	after, decodeErr := fileversion.Decode(text)
	if err != nil || decodeErr != nil || after.Version != info.Version {
		t.Fatalf("CLOSE used replacement path: %+v %v %v", after, err, decodeErr)
	}
}

func TestVersionExtensionPreservesVirtualInstructions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "CLAUDE.md")
	os.WriteFile(path, []byte("project rules\n"), 0600)
	v, err := New(root, []string{"CLAUDE.md"})
	if err != nil {
		t.Fatal(err)
	}
	c := connectVersion(t, v)
	h, info := checkedOpen(t, c, path)
	body := replaceString(nil, h, nil)
	body = binary.BigEndian.AppendUint64(body, 0)
	body = binary.BigEndian.AppendUint32(body, 4096)
	r := versionExchange(t, c, versionReply(5, 2, body))
	text, _, err := packetString(r[5:])
	if err != nil || !strings.Contains(text, Reference) || info.Mode&0777 != 0444 || info.Size != int64(len(text)) {
		t.Fatalf("lost instruction view: %q %+v %v", text, info, err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, []byte("project rules\n")) {
		t.Fatal("modified project instructions")
	}
}
