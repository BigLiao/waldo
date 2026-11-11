package projection

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func connectView(t *testing.T, v *View) (*sftp.Client, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	a.SetDeadline(time.Now().Add(10 * time.Second))
	b.SetDeadline(time.Now().Add(10 * time.Second))
	done := make(chan struct{})
	go func() { defer close(done); v.Serve(b) }()
	c, err := sftp.NewClientPipe(a, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		a.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SFTP view did not stop")
		}
	})
	return c, a
}

func readFile(t *testing.T, c *sftp.Client, path string) string {
	t.Helper()
	f, err := c.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != int64(len(b)) {
		t.Fatalf("FSTAT: %v %v, data=%d", st, err, len(b))
	}
	return string(b)
}

func TestInstructionsAreSessionOnly(t *testing.T) {
	project := t.TempDir()
	original := "# Project rules\nKeep existing instructions."
	p := filepath.Join(project, "AGENTS.md")
	if err := os.WriteFile(p, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		v, err := New(project, []string{"AGENTS.md", "CLAUDE.md"})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := connectView(t, v)
		got := readFile(t, c, p)
		if !strings.HasPrefix(got, original) || strings.Count(got, Reference) != 1 {
			t.Fatalf("instructions %q", got)
		}
		if got := readFile(t, c, filepath.Join(project, "CLAUDE.md")); got != Reference+"\n" {
			t.Fatalf("new instruction file: %q", got)
		}
		entries, err := c.ReadDir(project)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, e := range entries {
			counts[e.Name()]++
			if e.Mode().Perm() != 0444 {
				t.Errorf("virtual file permissions: %v", e.Mode())
			}
		}
		if counts["AGENTS.md"] != 1 || counts["CLAUDE.md"] != 1 {
			t.Fatalf("directory entries: %v", counts)
		}
		st, err := c.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("LSTAT: %v %v", st, err)
		}
	}
	actual, err := os.ReadFile(p)
	if err != nil || string(actual) != original {
		t.Fatalf("project instructions changed: %q %v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(project, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("virtual file was written to project")
	}
}

func TestVirtualFilesRejectMutations(t *testing.T) {
	project := t.TempDir()
	p := filepath.Join(project, "AGENTS.md")
	os.WriteFile(p, []byte("original"), 0600)
	other := filepath.Join(project, "temporary")
	os.WriteFile(other, []byte("new contents"), 0600)
	v, err := New(project, []string{"AGENTS.md", "CLAUDE.md"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := connectView(t, v)
	for name, op := range map[string]func() error{
		"write": func() error {
			f, e := c.OpenFile(p, os.O_WRONLY|os.O_TRUNC)
			if f != nil {
				f.Close()
			}
			return e
		},
		"read-write": func() error {
			f, e := c.OpenFile(p, os.O_RDWR)
			if f != nil {
				f.Close()
			}
			return e
		},
		"remove":     func() error { return c.Remove(p) },
		"chmod":      func() error { return c.Chmod(p, 0777) },
		"rename out": func() error { return c.Rename(p, other) },
		"rename in":  func() error { return c.PosixRename(other, p) },
		"link":       func() error { return c.Link(p, other+"-link") },
		"symlink":    func() error { return c.Symlink(other, filepath.Join(project, "CLAUDE.md")) },
	} {
		if err := op(); err == nil {
			t.Errorf("%s unexpectedly succeeded", name)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "original" {
		t.Fatal("original was modified")
	}
}

func TestOrdinaryFilesKeepOSServerSemantics(t *testing.T) {
	project := t.TempDir()
	v, err := New(project, []string{"AGENTS.md"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := connectView(t, v)
	p := filepath.Join(project, "normal")
	f, err := c.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(3); err != nil {
		t.Fatal(err)
	}
	if err := f.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	if err := c.PosixRename(p, p+"-renamed"); err != nil {
		t.Fatal(err)
	}
	// FSTAT must use the open file descriptor, including after rename/unlink.
	if err := c.Remove(p + "-renamed"); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != 3 || st.Mode().Perm() != 0600 {
		t.Fatalf("open descriptor stat: %v %v", st, err)
	}
	f.Close()
	if _, err := c.StatVFS(project); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir(filepath.Join(project, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory(filepath.Join(project, "dir")); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePermissionsPassThrough(t *testing.T) {
	project := t.TempDir()
	v, err := New(project, []string{"AGENTS.md"})
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	a.SetDeadline(time.Now().Add(5 * time.Second))
	b.SetDeadline(time.Now().Add(5 * time.Second))
	defer a.Close()
	go v.Serve(b)
	// OPEN's attribute flags differ from its pflags. Preserve the real server's
	// handling of the requested creation mode (RequestServer omits those flags).
	writePacket(a, []byte{1, 0, 0, 0, 3})
	if _, err := readPacket(a); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(project, "private-file")
	packet := replaceString([]byte{3, 0, 0, 0, 1}, p, nil)
	packet = binary.BigEndian.AppendUint32(packet, 0x0a) // WRITE | CREAT
	packet = binary.BigEndian.AppendUint32(packet, 0x04) // ATTR_PERMISSIONS
	packet = binary.BigEndian.AppendUint32(packet, 0100600)
	if err := writePacket(a, packet); err != nil {
		t.Fatal(err)
	}
	reply, err := readPacket(a)
	if err != nil || reply[0] != 102 {
		t.Fatalf("OPEN reply: %x %v", reply, err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("create mode: %v %v", st, err)
	}
}

func TestExistingReferenceAndCustomFile(t *testing.T) {
	project := t.TempDir()
	p := filepath.Join(project, "GEMINI.md")
	original := "existing rules\n\n" + Reference + "\n"
	os.WriteFile(p, []byte(original), 0600)
	v, err := New(project, []string{"GEMINI.md"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := connectView(t, v)
	if got := readFile(t, c, p); got != original {
		t.Fatalf("duplicate reference: %q", got)
	}
	if _, err := c.Stat(filepath.Join(project, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("unconfigured instruction file: %v", err)
	}
}
