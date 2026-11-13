//go:build linux

package fswatch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const watchMask = unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE |
	unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW

// Watcher watches a project recursively. New installs the initial watches before
// returning, so a caller can safely begin filling its cache afterwards. Changes
// are coalesced; Events is an invalidation signal, not a filesystem event log.
type Watcher struct {
	root      string
	rootDev   uint64
	rootIno   uint64
	rootWD    int
	fd        *os.File
	wakeR     *os.File
	wakeW     *os.File
	dirs      map[int]string
	events    chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New observes root and its existing subdirectories without following symlinks.
// It fails if it cannot establish complete coverage of the initial tree.
func New(root string) (*Watcher, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("watch project: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("watch project: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return nil, fmt.Errorf("watch project: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("watch project: %s is not a directory", root)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("watch project: inotify: %w", err)
	}
	w := &Watcher{
		root: root, rootDev: st.Dev, rootIno: st.Ino, rootWD: -1,
		fd: os.NewFile(uintptr(fd), "project-inotify"), dirs: make(map[int]string),
		events: make(chan Event, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		w.fd.Close()
		return nil, fmt.Errorf("watch project: wake pipe: %w", err)
	}
	w.wakeR = os.NewFile(uintptr(pipe[0]), "project-watch-wake-read")
	w.wakeW = os.NewFile(uintptr(pipe[1]), "project-watch-wake-write")
	if err := w.scan(false); err != nil {
		w.closeFiles()
		return nil, err
	}
	go w.run()
	return w, nil
}

func (w *Watcher) Events() <-chan Event { return w.events }

// Close stops observation, closes Events, and waits for the reader to finish.
// It is safe to call repeatedly or concurrently, including after an error.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.stop)
		// os.File protects against descriptor reuse if the reader already failed
		// and closed this pipe. A raw unix.Write on a saved fd would not.
		w.wakeW.Write([]byte{1})
	})
	<-w.done
	return nil
}

func (w *Watcher) closeFiles() {
	w.fd.Close()
	w.wakeR.Close()
	w.wakeW.Close()
}

// scan reconciles the full tree at startup or after an ambiguous watch event.
// Ordinary new directories only install watches on the new subtree.
func (w *Watcher) scan(allowVanished bool) error {
	var rootStat unix.Stat_t
	if err := unix.Stat(w.root, &rootStat); err != nil {
		return fmt.Errorf("watch project root: %w", err)
	}
	if rootStat.Dev != w.rootDev || rootStat.Ino != w.rootIno {
		return fmt.Errorf("watch project: root was replaced")
	}
	seen := make(map[int]string)
	if err := w.addTree(w.root, seen, allowVanished); err != nil {
		return err
	}
	if err := unix.Stat(w.root, &rootStat); err != nil {
		return fmt.Errorf("watch project root: %w", err)
	}
	if rootStat.Dev != w.rootDev || rootStat.Ino != w.rootIno {
		return fmt.Errorf("watch project: root was replaced during scan")
	}
	for wd := range w.dirs {
		if _, ok := seen[wd]; ok {
			continue
		}
		if err := w.removeWatch(wd); err != nil {
			return err
		}
	}
	w.dirs = seen
	return nil
}

// addTree watches each directory before enumerating its children. Callers
// invalidate both before and after adding a new subtree so writes before its
// watches were installed cannot leave a freshly populated cache valid.
func (w *Watcher) addTree(root string, into map[int]string, allowVanished bool) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			// Runtime notifications describe an earlier instant: a temporary
			// directory may already be gone by the time we install its watch.
			// Skip only that vanished entry and keep covering its siblings.
			if allowVanished && vanishedDir(err) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		wd, err := unix.InotifyAddWatch(int(w.fd.Fd()), path, watchMask)
		if err != nil {
			if allowVanished && vanishedDir(err) {
				return filepath.SkipDir
			}
			return fmt.Errorf("add watch %s: %w", path, err)
		}
		into[wd] = path
		if path == w.root {
			w.rootWD = wd
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("watch project tree: %w", err)
	}
	return nil
}

func vanishedDir(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR)
}

func (w *Watcher) removeWatch(wd int) error {
	// An automatically removed watch reports EINVAL here. Queued events for
	// removed descriptors are ignored after their map entries are removed.
	if _, err := unix.InotifyRmWatch(int(w.fd.Fd()), uint32(wd)); err != nil && err != unix.EINVAL {
		return fmt.Errorf("remove obsolete project watch: %w", err)
	}
	delete(w.dirs, wd)
	return nil
}

func (w *Watcher) removeTree(root string) error {
	for wd, path := range w.dirs {
		if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			continue
		}
		if err := w.removeWatch(wd); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) changed() {
	select {
	case w.events <- Event{}:
	default:
	}
}

func (w *Watcher) failed(err error) {
	// A fatal event must replace a pending ordinary invalidation, not disappear
	// because the coalescing channel is full. run is the only sender.
	select {
	case <-w.events:
	default:
	}
	w.events <- Event{Err: err}
}

func (w *Watcher) run() {
	defer close(w.done)
	defer close(w.events)
	defer w.closeFiles()
	fd := int(w.fd.Fd())
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(w.wakeR.Fd()), Events: unix.POLLIN}}
	buf := make([]byte, 64<<10)
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		if _, err := unix.Poll(poll, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			w.failed(fmt.Errorf("watch project poll: %w", err))
			return
		}
		select {
		case <-w.stop:
			return
		default:
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			w.failed(fmt.Errorf("watch project: inotify descriptor failed"))
			return
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			if err == nil {
				err = fmt.Errorf("unexpected end of inotify stream")
			}
			w.failed(fmt.Errorf("watch project read: %w", err))
			return
		}
		rescan, err := w.process(buf[:n])
		if err != nil {
			w.failed(err)
			return
		}
		w.changed()
		if rescan {
			if err := w.scan(true); err != nil {
				w.failed(err)
				return
			}
			w.changed()
		}
	}
}

func (w *Watcher) process(buf []byte) (bool, error) {
	rescan := false
	for len(buf) > 0 {
		if len(buf) < unix.SizeofInotifyEvent {
			return false, fmt.Errorf("watch project: truncated inotify event")
		}
		wd := int(int32(binary.NativeEndian.Uint32(buf[0:4])))
		mask := binary.NativeEndian.Uint32(buf[4:8])
		nameLen := int(binary.NativeEndian.Uint32(buf[12:16]))
		if nameLen > len(buf)-unix.SizeofInotifyEvent {
			return false, fmt.Errorf("watch project: truncated inotify name")
		}
		name := strings.TrimRight(string(buf[unix.SizeofInotifyEvent:unix.SizeofInotifyEvent+nameLen]), "\x00")
		buf = buf[unix.SizeofInotifyEvent+nameLen:]
		if mask&unix.IN_Q_OVERFLOW != 0 {
			return false, fmt.Errorf("watch project: inotify queue overflow")
		}
		if mask&unix.IN_UNMOUNT != 0 {
			return false, fmt.Errorf("watch project: watched filesystem unmounted")
		}
		if wd == w.rootWD && mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_IGNORED) != 0 {
			return false, fmt.Errorf("watch project: root watch was removed or moved")
		}
		parent, ok := w.dirs[wd]
		if !ok {
			continue // a queued event from a watch removed during reconciliation
		}
		if mask&unix.IN_DELETE_SELF != 0 {
			w.changed()
			if err := w.removeTree(parent); err != nil {
				return false, err
			}
			continue
		}
		if mask&(unix.IN_MOVE_SELF|unix.IN_IGNORED) != 0 {
			rescan = true
		}
		if mask&unix.IN_ISDIR == 0 || name == "" {
			continue
		}
		if filepath.Base(name) != name || name == "." || name == ".." {
			return false, fmt.Errorf("watch project: invalid directory event name")
		}
		path := filepath.Join(parent, name)
		if mask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0 {
			w.changed()
			if err := w.removeTree(path); err != nil {
				return false, err
			}
		}
		if mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
			w.changed()
			if err := w.addTree(path, w.dirs, true); err != nil {
				return false, err
			}
		}
	}
	return rescan, nil
}
