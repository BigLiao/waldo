//go:build linux

package fswatch

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func watch(t *testing.T, root string) *Watcher {
	t.Helper()
	w, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func changed(t *testing.T, w *Watcher) {
	t.Helper()
	select {
	case event, ok := <-w.Events():
		if !ok {
			t.Fatal("watcher closed without a change")
		}
		if event.Err != nil {
			t.Fatalf("watcher failed: %v", event.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no invalidation after project modification")
	}
}

// settle drains the coalesced notifications for one mutation and gives the
// recursive scan time to finish, so the next assertion tests a later mutation.
func settle(t *testing.T, w *Watcher) {
	t.Helper()
	timer := time.NewTimer(80 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-w.Events():
			if !ok || event.Err != nil {
				t.Fatalf("watcher stopped: %v", event.Err)
			}
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(80 * time.Millisecond)
		case <-timer.C:
			return
		}
	}
}

func put(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExternalWriteAndAtomicReplace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "existing", "nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file")
	put(t, path, "old")
	w := watch(t, root)
	put(t, path, "changed")
	changed(t, w)
	settle(t, w)
	// Prepare outside the tree: the only observed operation is the atomic
	// replacement, rather than an earlier write to the temporary file.
	tmp := filepath.Join(t.TempDir(), "replacement")
	put(t, tmp, "replacement")
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	// A directory watch must also observe subsequent writes to the new inode.
	put(t, path, "new inode modified")
	changed(t, w)
}

func TestNewAndMovedSubtreesRemainObserved(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "created"
		if moved {
			name = "moved-in"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			w := watch(t, root)
			dir := filepath.Join(root, "sub", "nested")
			if moved {
				source := filepath.Join(t.TempDir(), "source")
				if err := os.MkdirAll(filepath.Join(source, "nested"), 0700); err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Join(source, "nested", "file"), "initial")
				if err := os.Rename(source, filepath.Join(root, "sub")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			changed(t, w)
			settle(t, w)
			put(t, filepath.Join(dir, "file"), "later write")
			changed(t, w)
			settle(t, w)
			if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "renamed")); err != nil {
				t.Fatal(err)
			}
			changed(t, w)
			settle(t, w)
			put(t, filepath.Join(root, "renamed", "nested", "file"), "after directory rename")
			changed(t, w)
		})
	}
}

func TestDeleteAndMetadataChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	put(t, path, "content")
	w := watch(t, root)
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	put(t, path, "still watching root")
	changed(t, w)
}

func TestMovedOutSubtreeStopsBeingObserved(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	w := watch(t, root)
	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(sub, outside); err != nil {
		t.Fatal(err)
	}
	changed(t, w)
	settle(t, w)
	put(t, filepath.Join(outside, "nested", "file"), "outside project now")
	select {
	case event := <-w.Events():
		t.Fatalf("moved-out subtree remained observed: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
	put(t, filepath.Join(root, "file"), "root still watched")
	changed(t, w)
}

func TestReadsAndSymlinkTargetsDoNotInvalidate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	put(t, path, "content")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	w := watch(t, root)
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(root); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(outside, "file"), "outside project")
	select {
	case event := <-w.Events():
		t.Fatalf("read or symlink target caused invalidation: %+v", event)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRootMoveFailsClosed(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	w := watch(t, root)
	if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	select {
	case event, ok := <-w.Events():
		if !ok || event.Err == nil {
			t.Fatalf("root move must report unreliable observation: %+v, open=%v", event, ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("root move did not fail watcher")
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("watcher kept running after fatal event")
	}
}

func TestCloseIsConcurrentAndUnblocksEvents(t *testing.T) {
	w := watch(t, t.TempDir())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); w.Close() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock idle watcher")
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("Events is not closed")
	}
}

func TestOverflowCannotBeMistakenForOrdinaryChange(t *testing.T) {
	w := &Watcher{events: make(chan Event, 1)}
	w.changed()
	buf := make([]byte, unix.SizeofInotifyEvent)
	binary.NativeEndian.PutUint32(buf[4:8], unix.IN_Q_OVERFLOW)
	_, err := w.process(buf)
	if err == nil {
		t.Fatal("overflow was not rejected")
	}
	w.failed(err)
	if event := <-w.Events(); event.Err == nil {
		t.Fatal("pending invalidation hid fatal observation error")
	}
}

func TestNewRejectsMissingAndNonDirectoryRoots(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	put(t, file, "content")
	for _, path := range []string{file, filepath.Join(root, "missing")} {
		if w, err := New(path); err == nil {
			w.Close()
			t.Fatalf("New(%q) unexpectedly succeeded", path)
		}
	}
}

func TestRuntimeDirectoryChurnKeepsRemainingCoverage(t *testing.T) {
	for _, mask := range []uint32{unix.IN_CREATE, unix.IN_MOVED_TO} {
		t.Run(map[uint32]string{unix.IN_CREATE: "created", unix.IN_MOVED_TO: "moved-in"}[mask], func(t *testing.T) {
			root := t.TempDir()
			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
			if err != nil {
				t.Fatal(err)
			}
			file := os.NewFile(uintptr(fd), "test-inotify")
			t.Cleanup(func() { file.Close() })
			wd, err := unix.InotifyAddWatch(fd, root, watchMask)
			if err != nil {
				t.Fatal(err)
			}
			// Process a saved notification after its directory has disappeared.
			// Keeping the reader stopped makes this race deterministic.
			w := &Watcher{root: root, rootWD: wd, fd: file, dirs: map[int]string{wd: root}, events: make(chan Event, 1)}
			transient := filepath.Join(root, "transient")
			if err := os.Mkdir(transient, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(transient); err != nil {
				t.Fatal(err)
			}
			surviving := filepath.Join(root, "surviving", "nested")
			if err := os.MkdirAll(surviving, 0700); err != nil {
				t.Fatal(err)
			}
			packet := func(mask uint32, name string) []byte {
				nameLen := (len(name) + 1 + 3) &^ 3
				p := make([]byte, unix.SizeofInotifyEvent+nameLen)
				binary.NativeEndian.PutUint32(p[0:4], uint32(wd))
				binary.NativeEndian.PutUint32(p[4:8], mask|unix.IN_ISDIR)
				binary.NativeEndian.PutUint32(p[12:16], uint32(nameLen))
				copy(p[unix.SizeofInotifyEvent:], name)
				return p
			}
			batch := append(packet(mask, "transient"), packet(unix.IN_DELETE, "transient")...)
			batch = append(batch, packet(unix.IN_CREATE, "surviving")...)
			if _, err := w.process(batch); err != nil {
				t.Fatalf("short-lived runtime directory disabled observation: %v", err)
			}
			select {
			case event := <-w.Events():
				if event.Err != nil {
					t.Fatalf("churn was reported as a fatal error: %v", event.Err)
				}
			default:
				t.Fatal("short-lived directory did not invalidate cached paths")
			}
			found := false
			for _, path := range w.dirs {
				found = found || path == surviving
			}
			if !found {
				t.Fatal("vanished entry prevented monitoring the remaining subtree")
			}
		})
	}
}

func TestRuntimeMissingDirectoriesDoNotHidePermissionOrResourceErrors(t *testing.T) {
	for _, err := range []error{unix.EACCES, unix.EPERM, unix.ENOSPC, unix.EMFILE} {
		if vanishedDir(&os.PathError{Op: "watch", Path: "project/sub", Err: err}) {
			t.Fatalf("coverage failure %v was mistaken for a vanished directory", err)
		}
	}
	if !vanishedDir(&os.PathError{Op: "watch", Path: "project/sub", Err: unix.ENOENT}) {
		t.Fatal("wrapped ENOENT was not recognized")
	}
	if !errors.Is(unix.EACCES, os.ErrPermission) {
		t.Fatal("permission error test does not represent a permission failure")
	}
}
