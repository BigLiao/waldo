// Package fscache caches file data learned from ordinary SFTP reads. Every
// read-only open validates that file on A; no watchers or notifications exist.
package fscache

import (
	"path"
	"strings"
	"sync"
	"time"

	"waldo/internal/fileversion"
)

const (
	maxFileSize = 1 << 20
	maxBytes    = 32 << 20
	maxFiles    = 256
	maxHandles  = 1024
)

type blob struct {
	data []byte
	refs int
}
type span struct{ start, end int }
type entry struct {
	version fileversion.Version
	body    *blob
	expires time.Time
	used    uint64
}
type Cache struct {
	mu         sync.Mutex
	root       string
	contentTTL time.Duration
	files      map[string]*entry
	used       int // capacities of all live blobs, including opened/evicted snapshots
	clock      uint64
	serving    bool
}

// New creates a byte cache. ContentTTL is its idle retention limit, never a
// freshness lease. Path metadata is always forwarded to A.
func New(root string, contentTTL time.Duration) *Cache {
	return &Cache{root: path.Clean(root), contentTTL: contentTTL, files: map[string]*entry{}}
}

func (c *Cache) cachePath(p string) bool {
	return c.contentTTL > 0 && path.IsAbs(p) && len(p) <= 4096 && p == path.Clean(p) && (p == c.root || strings.HasPrefix(p, strings.TrimSuffix(c.root, "/")+"/"))
}

func (c *Cache) release(b *blob) {
	if b == nil {
		return
	}
	b.refs--
	if b.refs == 0 {
		c.used -= cap(b.data)
	}
}
func (c *Cache) drop(p string) {
	if e := c.files[p]; e != nil {
		delete(c.files, p)
		c.release(e.body)
	}
}
func (c *Cache) sweep(now time.Time) {
	for p, e := range c.files {
		if !now.Before(e.expires) {
			c.drop(p)
		}
	}
}
func (c *Cache) evictOne() bool {
	var oldest string
	var used uint64
	for p, e := range c.files {
		if oldest == "" || e.used < used {
			oldest, used = p, e.used
		}
	}
	if oldest == "" {
		return false
	}
	c.drop(oldest)
	return true
}
func (c *Cache) touch(e *entry) {
	c.clock++
	e.used = c.clock
	e.expires = time.Now().Add(c.contentTTL)
}
func (c *Cache) grow(b *blob, n int) bool {
	if n <= len(b.data) {
		return true
	}
	if n > maxFileSize {
		return false
	}
	if n <= cap(b.data) {
		b.data = b.data[:n]
		return true
	}
	size := max(4096, cap(b.data)*2)
	for size < n {
		size *= 2
	}
	size = min(size, maxFileSize)
	delta := size - cap(b.data)
	for c.used+delta > maxBytes {
		if !c.evictOne() {
			return false
		}
	}
	buf := make([]byte, n, size)
	copy(buf, b.data)
	b.data = buf
	c.used += delta
	return true
}
func (c *Cache) store(p string, v fileversion.Version, b *blob) {
	c.drop(p)
	for len(c.files) >= maxFiles {
		c.evictOne()
	}
	b.refs++
	e := &entry{version: v, body: b}
	c.touch(e)
	c.files[p] = e
}
func addSpan(spans []span, s span) []span {
	out := make([]span, 0, len(spans)+1)
	for _, old := range spans {
		if old.end < s.start {
			out = append(out, old)
			continue
		}
		if s.end < old.start {
			out = append(out, s)
			s = old
			continue
		}
		s.start = min(s.start, old.start)
		s.end = max(s.end, old.end)
	}
	return append(out, s)
}
