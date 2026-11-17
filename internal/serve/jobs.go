package serve

import (
	"sync"
	"syscall"
	"time"
)

// A process group remains registered after its leader exits if background jobs
// still use it. Detached descendants have the native lifecycle limits described
// in SECURITY.md; they do not escape the inherited Seatbelt policy.
type processGroups struct {
	mu      sync.Mutex
	groups  map[int]bool
	closing bool
}

var nativeJobs = processGroups{groups: make(map[int]bool)}

func (j *processGroups) add(pid int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closing {
		syscall.Kill(-pid, syscall.SIGKILL)
		return
	}
	j.groups[pid] = true
}

func (j *processGroups) prune() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for pid := range j.groups {
		if syscall.Kill(-pid, 0) == syscall.ESRCH {
			delete(j.groups, pid)
		}
	}
}

func (j *processGroups) close() {
	j.mu.Lock()
	j.closing = true
	for pid := range j.groups {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
	j.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j.prune()
		j.mu.Lock()
		empty := len(j.groups) == 0
		j.mu.Unlock()
		if empty {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for pid := range j.groups {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}
