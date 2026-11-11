package host

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWriteIDMaps(t *testing.T) {
	dir := t.TempDir()
	maps, err := writeIDMaps(dir, 51368, 100, "ubuntu", "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ path, line string }{{maps.uid, "ubuntu:51368\n"}, {maps.gid, "ubuntu:100\n"}} {
		b, err := os.ReadFile(want.path)
		if err != nil || string(b) != want.line {
			t.Fatalf("mapping %s = %q, %v; want %q", want.path, b, err, want.line)
		}
		st, err := os.Stat(want.path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("mapping permissions = %v, %v; want 0600", st, err)
		}
	}
	// Never overwrite a map that another process could already be reading.
	if _, err := writeIDMaps(dir, 123, 456, "different", "different"); err == nil {
		t.Fatal("existing mappings were overwritten")
	}
	b, _ := os.ReadFile(maps.uid)
	if string(b) != "ubuntu:51368\n" {
		t.Fatalf("existing mapping changed: %q", b)
	}
}

func TestRemoveRunDirRemovesIDMaps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	for _, sub := range []string{"m/0", "stubs/0", "s"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writeIDMaps(dir, 51368, 100, "ubuntu", "ubuntu"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeRunDir(dir)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("session ID mappings were not cleaned up: %v", err)
	}
}

func TestIDMapRejectsInvalidNamesAndIDs(t *testing.T) {
	for _, name := range []string{"", "ubuntu:0", "ubuntu\nroot:0", "ubuntu\rroot:0", "ubuntu\x00", " leading", "two words", "user\tname"} {
		if _, err := idMapLine(name, 1000); err == nil {
			t.Errorf("accepted invalid mapping name %q", name)
		}
	}
	for _, id := range []int{-1, -1000} {
		if _, err := idMapLine("ubuntu", id); err == nil {
			t.Errorf("accepted invalid ID %d", id)
		}
	}
	for _, name := range []string{"ubuntu", "domain\\user", "group-with-dashes", "name.with.dots", "user$"} {
		if _, err := idMapLine(name, 0); err != nil {
			t.Errorf("rejected valid mapping name %q: %v", name, err)
		}
	}
	dir := t.TempDir()
	if _, err := writeIDMaps(dir, 1000, -1, "ubuntu", "ubuntu"); err == nil {
		t.Fatal("accepted invalid GID")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid input created partial mappings: %v %v", entries, err)
	}
}

func TestIDMapFilesDoNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "gidmap")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeIDMaps(dir, 51368, 100, "ubuntu", "ubuntu"); err == nil {
		t.Fatal("followed an existing mapping symlink")
	}
	if _, err := os.Lstat(filepath.Join(dir, "uidmap")); !os.IsNotExist(err) {
		t.Fatalf("left a partial UID map: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "untouched" {
		t.Fatalf("symlink target changed: %q %v", b, err)
	}
}

func TestIDMapOptionsEscapeFuseSeparators(t *testing.T) {
	m := idMapFiles{uid: `/run/waldo/user,dir\with spaces/uidmap`, gid: `/run/waldo/other,dir\with spaces/gidmap`}
	want := []string{"idmap=file", "nomap=ignore", `uidfile=/run/waldo/user\,dir\\with spaces/uidmap`, `gidfile=/run/waldo/other\,dir\\with spaces/gidmap`}
	got := m.options()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("options = %q; want %q", got, want)
	}
	for _, option := range got {
		if strings.Contains(option, "uid=") || strings.Contains(option, "gid=") {
			t.Fatalf("mapping must not force all file ownership: %q", option)
		}
	}
}

func TestLocalIDNameNSSFallback(t *testing.T) {
	// Emulate pure-Go os/user failing to find an NSS account. The fallback
	// command receives separate argv and never evaluates the output as shell.
	dir := t.TempDir()
	idPath := filepath.Join(dir, "id")
	if err := os.WriteFile(idPath, []byte("#!/bin/sh\ncase \"$1\" in\n-un) printf 'domain\\\\user\\n';;\n-gn) printf 'users\\n';;\n*) exit 1;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	missing := func(string) (string, error) { return "", errors.New("missing NSS record") }
	name, err := localIDName("用户", "-un", 12345, missing)
	if err != nil || name != `domain\user` {
		t.Fatalf("NSS user fallback = %q, %v", name, err)
	}
	name, err = localIDName("主组", "-gn", 54321, missing)
	if err != nil || name != "users" {
		t.Fatalf("NSS group fallback = %q, %v", name, err)
	}
	if err := os.WriteFile(idPath, []byte("#!/bin/sh\nprintf '12345\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := localIDName("用户", "-un", 12345, missing); err == nil {
		t.Fatal("accepted an unresolved numeric ID as a username")
	}
	if err := os.Remove(idPath); err != nil {
		t.Fatal(err)
	}
	if _, err := localIDName("用户", "-un", 12345, missing); err == nil {
		t.Fatal("silently ignored a failed NSS fallback")
	}
}

func TestLocalIDNameUsesNumericProcessID(t *testing.T) {
	name, err := localIDName("用户", "-un", 51368, func(id string) (string, error) {
		if id != strconv.Itoa(51368) {
			t.Fatalf("looked up %q instead of the process UID", id)
		}
		return "jesonliao", nil
	})
	if err != nil || name != "jesonliao" {
		t.Fatalf("local name = %q, %v", name, err)
	}
}
