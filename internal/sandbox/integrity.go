package sandbox

import (
	"os"
	"path/filepath"
	"syscall"
)

// Snapshot records the state of protected paths before a session, so that
// paths which did not exist (and therefore could not be bind-mounted
// read-only) can be checked afterwards.
type Snapshot map[string]fileID

type fileID struct {
	exists bool
	ino    uint64
	dev    uint64
}

// extraWatched are paths that, when they appear, change what host-side tools
// execute (git reads commondir/config.worktree; a new .git turns a plain
// directory into a repository with hooks).
var extraWatched = []string{".git", ".git/commondir", ".git/config.worktree", ".git/hooks"}

func watched(project string) []string {
	var out []string
	for _, n := range append(append([]string{}, ProtectedNames...), extraWatched...) {
		out = append(out, filepath.Join(project, n))
	}
	return out
}

func stat(p string) fileID {
	st, err := os.Lstat(p)
	if err != nil {
		return fileID{}
	}
	sys, _ := st.Sys().(*syscall.Stat_t)
	if sys == nil {
		return fileID{exists: true}
	}
	return fileID{exists: true, ino: sys.Ino, dev: uint64(sys.Dev)}
}

// TakeSnapshot records the current state.
func TakeSnapshot(project string) Snapshot {
	s := Snapshot{}
	for _, p := range watched(project) {
		s[p] = stat(p)
	}
	return s
}

// Changed returns protected paths that appeared, vanished or were replaced
// during the session.
func (s Snapshot) Changed() []string {
	var out []string
	seen := map[string]bool{}
	for p, before := range s {
		after := stat(p)
		if before != after && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
