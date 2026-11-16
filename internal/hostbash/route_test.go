package hostbash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteMountOwnership(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	project := filepath.Join(root, "project")
	os.Mkdir(project, 0700)
	mounts := "1 0 0:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / " + project + " rw - fuse.sshfs waldo:/project rw\n3 2 0:1 /state " + project + "/.claude rw - ext4 /dev/root rw\n"
	r, e := parseMounts(strings.NewReader(mounts), project)
	if e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		path, location, want string
		search, fail         bool
	}{
		{project + "/not-created", "auto", "host", false, false},
		{project + "/.claude/memory", "auto", "agent", false, false},
		{project + "-other/x", "auto", "agent", false, false},
		{root + "/tmp.txt", "auto", "agent", false, false},
		{root + "/tmp.txt", "host", "host", false, false},
		{project + "/x", "agent", "", false, true},
		{root, "auto", "", true, true},
		{root, "host", "host", true, false},
		{"subdir", "auto", "host", true, false},
	} {
		_, side, e := r.route(tt.path, tt.location, tt.search)
		if (e != nil) != tt.fail || side != tt.want {
			t.Errorf("%+v: side=%s err=%v", tt, side, e)
		}
	}
	link := filepath.Join(root, "link")
	os.Symlink(project, link)
	path, side, e := r.route(link+"/new.txt", "auto", false)
	if e != nil || side != "host" || path != project+"/new.txt" {
		t.Fatal(path, side, e)
	}
	if _, _, e = r.route(link+"/new.txt", "agent", false); e == nil {
		t.Fatal("agent symlink crossed host mount")
	}
}

func TestMountEscapesAndSource(t *testing.T) {
	r, e := parseMounts(strings.NewReader("1 0 0:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / /with\\040space rw - fuse.sshfs waldo:/x rw\n3 1 0:3 / /other rw - fuse.sshfs other:/x rw\n"), "/")
	if e != nil {
		t.Fatal(e)
	}
	if r.owner("/with space/a") != "host" || r.owner("/other/a") != "agent" {
		t.Fatal(r)
	}
}
