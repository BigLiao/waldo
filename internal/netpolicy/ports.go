package netpolicy

import (
	"fmt"
	"io"
	"net"
	"sort"
	"sync"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/util"
)

// WellKnown lists ports above 1024 that are never auto-forwarded: a sandbox
// service must not be able to impersonate a database or daemon the user
// expects on localhost.
var WellKnown = map[int]bool{
	1080: true, 1433: true, 1521: true, 2049: true, 2375: true, 2376: true, 2379: true, 2380: true,
	3128: true, 3306: true, 3389: true, 4369: true, 5000: false, 5432: true, 5433: true, 5672: true,
	5900: true, 5984: true, 6379: true, 6380: true, 6443: true, 7474: true, 7687: true, 8086: true,
	8200: true, 8500: true, 9042: true, 9092: true, 9200: true, 9300: true, 11211: true, 15672: true,
	26257: true, 27017: true, 27018: true, 27019: true, 28015: true, 50051: false,
}

// Forwarder mirrors listening sandbox ports onto the host's loopback.
type Forwarder struct {
	Mode    config.PortsMode
	Dial    func(port int) (io.ReadWriteCloser, error) // opens a stream to the sandbox port
	Audit   *audit.Logger
	OnState func(active []int)

	mu     sync.Mutex
	active map[int][]net.Listener
	busy   map[int]bool
}

func (f *Forwarder) wanted(p int) bool {
	switch f.Mode.Mode {
	case "off":
		return false
	case "list":
		for _, x := range f.Mode.List {
			if x == p {
				return true
			}
		}
		return false
	}
	return p >= 1024 && !WellKnown[p]
}

// Update applies the current set of sandbox listening ports.
func (f *Forwarder) Update(ports []int) {
	f.mu.Lock()
	if f.active == nil {
		f.active = map[int][]net.Listener{}
		f.busy = map[int]bool{}
	}
	now := map[int]bool{}
	for _, p := range ports {
		now[p] = true
	}
	changed := false
	for p, lns := range f.active {
		if !now[p] {
			for _, ln := range lns {
				ln.Close()
			}
			delete(f.active, p)
			f.Audit.Log("port-close", map[string]any{"port": p})
			changed = true
		}
	}
	for p := range f.busy {
		if !now[p] {
			delete(f.busy, p)
		}
	}
	for _, p := range ports {
		if _, ok := f.active[p]; ok || f.busy[p] || !f.wanted(p) {
			continue
		}
		var lns []net.Listener
		ln4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			f.busy[p] = true
			f.Audit.Log("port-busy", map[string]any{"port": p, "err": err.Error()})
			continue
		}
		lns = append(lns, ln4)
		if ln6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", p)); err == nil {
			lns = append(lns, ln6)
		}
		f.active[p] = lns
		f.Audit.Log("port-open", map[string]any{"port": p})
		changed = true
		for _, ln := range lns {
			go f.serve(p, ln)
		}
	}
	act := f.activeLocked()
	f.mu.Unlock()
	if changed && f.OnState != nil {
		f.OnState(act)
	}
}

func (f *Forwarder) activeLocked() []int {
	var out []int
	for p := range f.active {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// Active returns the forwarded ports.
func (f *Forwarder) Active() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeLocked()
}

func (f *Forwarder) serve(port int, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			st, err := f.Dial(port)
			if err != nil {
				c.Close()
				return
			}
			util.Splice(st, c)
		}()
	}
}

// Close stops all forwards.
func (f *Forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, lns := range f.active {
		for _, ln := range lns {
			ln.Close()
		}
	}
	f.active = nil
}
