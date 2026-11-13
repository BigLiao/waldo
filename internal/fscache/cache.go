// Package fscache is a session-scoped, write-through SFTP read cache.
// The upstream OS server remains responsible for all mutations and writable
// handles. A Cache must only serve one connection at a time.
package fscache

import (
	"path"
	"strings"
	"sync"
	"time"
)

const (
	leaseDuration    = 3 * time.Second
	maxFileSize      = 1 << 20
	maxBytes         = 32 << 20
	maxFiles         = 256
	maxMetadata      = 8192
	maxMetadataBytes = 2 << 20
	sweepInterval    = 250 * time.Millisecond
)

// Cache starts disabled. Its owner must renew the lease with SetEnabled(true)
// only while upstream change notifications are working.
type Cache struct {
	root                    string
	metadataTTL, contentTTL time.Duration
	mu                      sync.Mutex
	enabled                 bool
	leaseUntil              time.Time
	generation              uint64
	busy                    int // pending mutations and open writable handles
	used                    int
	metadataBytes           int
	files                   map[string]*entry
	metadata                map[metaKey]*metadata
	proxy                   *proxy
	nextSweep               time.Time
}

type metaKey struct {
	kind byte
	path string
}
type metadata struct {
	kind    byte
	body    []byte
	expires time.Time
}
type entry struct {
	path    string
	data    []byte
	expires time.Time
	remote  *remoteFile
}
type span struct{ start, end int }
type capture struct {
	generation uint64
	expires    time.Time
	data       []byte
	spans      []span
	size       uint64
	knownSize  bool
	regular    bool
	verified   bool // a real EOF or a descriptor FSTAT after the captured reads
	revision   uint64
}

// New constructs an in-memory cache. Zero TTL disables the respective cache.
func New(root string, metadataTTL, contentTTL time.Duration) *Cache {
	return &Cache{root: path.Clean(root), metadataTTL: metadataTTL, contentTTL: contentTTL,
		files: make(map[string]*entry), metadata: make(map[metaKey]*metadata)}
}

// SetEnabled renews a short lease, or disables and discards the cache. A missed
// heartbeat therefore cannot leave cached reads enabled indefinitely.
func (c *Cache) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !enabled || !c.enabled || !now.Before(c.leaseUntil) {
		c.invalidateLocked()
	}
	c.enabled = enabled
	if enabled {
		c.leaseUntil = now.Add(leaseDuration)
	}
}

// Invalidate discards metadata and file contents, including in-flight fills.
// Already open cached handles retain their upstream file identity and fall
// back to that handle after invalidation.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateLocked()
}

func (c *Cache) invalidateLocked() {
	c.generation++
	clear(c.files)
	clear(c.metadata)
	c.used = 0
	c.metadataBytes = 0
	if c.proxy != nil {
		for rf := range c.proxy.remotes {
			rf.capture, rf.entry = nil, nil
			rf.attrs = nil
			rf.retained = false
			c.proxy.maybeCloseLocked(rf)
		}
	}
}

func (c *Cache) activeLocked(now time.Time) bool {
	if c.enabled && !now.Before(c.leaseUntil) {
		c.enabled = false
		c.invalidateLocked()
	}
	return c.enabled && c.busy == 0
}

func (c *Cache) cachePath(p string) (string, bool) {
	// Cleaning symlink/../name can change the inode named by a path.
	if !path.IsAbs(p) || len(p) > 4096 || p != path.Clean(p) {
		return "", false
	}
	return p, p == c.root || strings.HasPrefix(p, strings.TrimSuffix(c.root, "/")+"/")
}

func (c *Cache) dropEntryLocked(e *entry) {
	if c.files[e.path] != e {
		return
	}
	delete(c.files, e.path)
	c.used -= cap(e.data)
	e.remote.entry = nil
	e.remote.retained = false
	if c.proxy != nil {
		c.proxy.maybeCloseLocked(e.remote)
	}
}

func (c *Cache) sweepLocked(now time.Time) {
	c.activeLocked(now)
	if now.Before(c.nextSweep) {
		return
	}
	c.nextSweep = now.Add(sweepInterval)
	for _, e := range c.files {
		if !now.Before(e.expires) {
			c.dropEntryLocked(e)
		}
	}
	for k, m := range c.metadata {
		if !now.Before(m.expires) {
			c.metadataBytes -= len(m.body) + len(k.path)
			delete(c.metadata, k)
		}
	}
	if c.proxy != nil {
		for rf := range c.proxy.remotes {
			if rf.attrs != nil && !now.Before(rf.attrs.expires) {
				c.metadataBytes -= len(rf.attrs.body)
				rf.attrs = nil
			}
			if cp := rf.capture; cp != nil && !now.Before(cp.expires) {
				c.used -= cap(cp.data)
				rf.capture = nil
			}
		}
	}
}

func (c *Cache) storeMetaLocked(kind byte, p string, response byte, body []byte, at time.Time) {
	ttl := c.metadataTTL
	// Keep bounded size hints when metadata responses are disabled. A hint can
	// select an empty-file candidate, but never proves capture completeness.
	if ttl <= 0 && kind == statPacket && response == attrsPacket {
		ttl = c.contentTTL
	}
	if ttl <= 0 || len(body) > 64<<10 {
		return
	}
	key := metaKey{kind, p}
	if len(c.metadata) >= maxMetadata && c.metadata[key] == nil {
		return
	}
	oldSize := 0
	if old := c.metadata[key]; old != nil {
		oldSize = len(old.body) + len(key.path)
	}
	newSize := len(body) + len(p)
	if c.metadataBytes-oldSize+newSize > maxMetadataBytes {
		return
	}
	c.metadataBytes += newSize - oldSize
	c.metadata[key] = &metadata{kind: response, body: append([]byte(nil), body...), expires: at.Add(ttl)}
}

func (c *Cache) storeFileAttrsLocked(rf *remoteFile, body []byte, at time.Time) {
	if c.metadataTTL <= 0 || len(body) > 64<<10 {
		return
	}
	oldSize := 0
	if rf.attrs != nil {
		oldSize = len(rf.attrs.body)
	}
	if c.metadataBytes-oldSize+len(body) > maxMetadataBytes {
		return
	}
	c.metadataBytes += len(body) - oldSize
	rf.attrs = &metadata{kind: attrsPacket, body: append([]byte(nil), body...), expires: at.Add(c.metadataTTL)}
}

func (c *Cache) learnLocked(rf *remoteFile, offset uint64, data []byte, eof bool, generation uint64) {
	cp := rf.capture
	now := time.Now()
	if cp == nil || rf.closed || rf.closing || cp.generation != c.generation || generation != c.generation || !c.activeLocked(now) || !now.Before(cp.expires) {
		return
	}
	if offset > maxFileSize || uint64(len(data)) > maxFileSize-offset {
		c.used -= cap(cp.data)
		rf.capture = nil
		return
	}
	end := int(offset) + len(data)
	if len(data) > 0 {
		cp.revision++
		cp.verified = false
		if cp.knownSize && uint64(end) > cp.size {
			cp.knownSize = false
		}
		if end > len(cp.data) {
			if end > cap(cp.data) {
				capacity := cap(cp.data) * 2
				if capacity < 4096 {
					capacity = 4096
				}
				for capacity < end {
					capacity *= 2
				}
				if capacity > maxFileSize {
					capacity = maxFileSize
				}
				delta := capacity - cap(cp.data)
				if c.used+delta > maxBytes-maxMetadataBytes {
					c.used -= cap(cp.data)
					rf.capture = nil
					return
				}
				buf := make([]byte, end, capacity)
				copy(buf, cp.data)
				cp.data = buf
				c.used += delta
			} else {
				cp.data = cp.data[:end]
			}
		}
		copy(cp.data[int(offset):end], data)
		addSpan(cp, span{int(offset), end})
		if len(cp.spans) > 256 {
			c.used -= cap(cp.data)
			rf.capture = nil
			return
		}
	}
	if eof && len(cp.spans) == 1 && cp.spans[0].start == 0 && uint64(cp.spans[0].end) == offset {
		cp.size, cp.knownSize = offset, true
		cp.verified = true
	} else if eof && offset == 0 && len(cp.data) == 0 {
		cp.size, cp.knownSize = 0, true
		cp.verified = true
	}
	c.completeLocked(rf)
}

func addSpan(cp *capture, s span) {
	merged := make([]span, 0, len(cp.spans)+1)
	for _, old := range cp.spans {
		if old.end < s.start {
			merged = append(merged, old)
			continue
		}
		if s.end < old.start {
			merged = append(merged, s)
			s = old
			continue
		}
		if old.start < s.start {
			s.start = old.start
		}
		if old.end > s.end {
			s.end = old.end
		}
	}
	cp.spans = append(merged, s)
}

func (c *Cache) completeLocked(rf *remoteFile) {
	cp := rf.capture
	if cp == nil || rf.closed || rf.closing || !cp.regular || !cp.verified || !cp.knownSize || cp.size > maxFileSize || cp.generation != c.generation || !time.Now().Before(cp.expires) {
		return
	}
	if cp.size > 0 && (len(cp.spans) != 1 || cp.spans[0].start != 0 || uint64(cp.spans[0].end) != cp.size) {
		return
	}
	if len(c.files) >= maxFiles && c.files[rf.path] == nil {
		c.used -= cap(cp.data)
		rf.capture = nil
		return
	}
	if old := c.files[rf.path]; old != nil {
		c.dropEntryLocked(old)
	}
	e := &entry{path: rf.path, data: cp.data, expires: cp.expires, remote: rf}
	rf.capture, rf.entry, rf.retained = nil, e, true
	c.files[rf.path] = e
}

func (cp *capture) covered() bool {
	return len(cp.data) == 0 || len(cp.spans) == 1 && cp.spans[0].start == 0 && cp.spans[0].end == len(cp.data)
}
