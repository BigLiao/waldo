package fscache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

type remoteFile struct {
	real, path                                     string
	refs, inflight                                 int
	writable, directory, retained, closing, closed bool
	openedGeneration                               uint64
	capture                                        *capture
	entry                                          *entry
	attrs                                          *metadata
}
type handle struct {
	remote  *remoteFile
	closing bool
}
type request struct {
	originalID         uint32
	kind               byte
	internal, mutation bool
	path               string
	at                 time.Time
	generation         uint64
	readonly, writable bool
	offset             uint64
	handle             *handle
	remote             *remoteFile
	capture            *capture
	revision           uint64
	covered            bool
	confirmClose       *request
}
type proxy struct {
	cache               *Cache
	front, upstream     io.ReadWriteCloser
	frontMu, upstreamMu sync.Mutex
	nextID              uint32
	nextHandle          uint64
	pending             map[uint32]*request
	handles             map[string]*handle
	remotes             map[*remoteFile]struct{}
	closeQueue          []*remoteFile
	requestQueue        []*request
	wake                chan struct{}
	done                chan struct{}
}

// Serve transparently proxies SFTP. Complete small files are learned from their
// normal responses. If the client closes after an exact-sized read without EOF,
// one descriptor FSTAT confirms completeness before retaining the cold handle.
// Cached read-only opens retain an upstream descriptor, so invalidation can
// fall back to the same inode rather than reopening a potentially renamed path.
// Directory listings and unknown extensions are always forwarded.
func (c *Cache) Serve(front, upstream io.ReadWriteCloser) error {
	p := &proxy{cache: c, front: front, upstream: upstream, pending: make(map[uint32]*request),
		handles: make(map[string]*handle), remotes: make(map[*remoteFile]struct{}), wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.mu.Lock()
	if c.proxy != nil {
		c.mu.Unlock()
		return errors.New("fscache: already serving a connection")
	}
	c.invalidateLocked()
	c.proxy = p
	c.busy = 0
	c.mu.Unlock()
	results := make(chan error, 3)
	go func() { results <- p.frontLoop() }()
	go func() { results <- p.upstreamLoop() }()
	go func() { results <- p.maintenance() }()
	err := <-results
	close(p.done)
	front.Close()
	upstream.Close()
	<-results
	<-results
	c.mu.Lock()
	c.enabled = false
	c.invalidateLocked()
	c.proxy = nil
	c.busy = 0
	c.mu.Unlock()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (p *proxy) writeFront(packet []byte) error {
	p.frontMu.Lock()
	defer p.frontMu.Unlock()
	return writeWirePacket(p.front, packet)
}
func (p *proxy) writeUpstream(packet []byte) error {
	p.upstreamMu.Lock()
	defer p.upstreamMu.Unlock()
	return writeWirePacket(p.upstream, packet)
}
func (p *proxy) frontLoop() error {
	for {
		packet, err := readWirePacket(p.front)
		if err != nil {
			return err
		}
		if packet[0] == initPacket {
			if err := p.writeUpstream(packet); err != nil {
				return err
			}
			continue
		}
		if err := p.forwardRequest(packet); err != nil {
			return err
		}
	}
}

func (p *proxy) forwardRequest(packet []byte) error {
	if len(packet) < 5 {
		return io.ErrUnexpectedEOF
	}
	c := p.cache
	r := &request{kind: packet[0], originalID: binary.BigEndian.Uint32(packet[1:5]), at: time.Now()}
	var local []byte
	c.mu.Lock()
	c.sweepLocked(r.at)
	r.generation = c.generation
	var parseErr error
	switch r.kind {
	case openPacket:
		path, rest, ok := packetString(packet[5:])
		if !ok || len(rest) < 4 {
			parseErr = io.ErrUnexpectedEOF
			break
		}
		r.path = path
		flags := binary.BigEndian.Uint32(rest)
		r.writable = flags&0x3e != 0
		r.readonly = flags == 1
		r.mutation = r.writable
		if key, ok := c.cachePath(path); ok {
			r.path = key
			if e := c.files[key]; e != nil && r.readonly && c.activeLocked(r.at) && r.at.Before(e.expires) {
				alias := p.aliasLocked(e.remote)
				local = reply(handlePacket, r.originalID, appendString(nil, alias))
			}
		}
	case closePacket, readPacket, writePacket, fstatPacket, fsetstatPacket, readdirPacket:
		alias, rest, ok := packetString(packet[5:])
		if !ok {
			parseErr = io.ErrUnexpectedEOF
			break
		}
		h := p.handles[alias]
		if h == nil || h.closing {
			local = status(r.originalID, 4)
			break
		}
		r.handle, r.remote = h, h.remote
		r.path = h.remote.path
		if r.kind == readPacket {
			if len(rest) < 12 {
				parseErr = io.ErrUnexpectedEOF
				break
			}
			r.offset = binary.BigEndian.Uint64(rest)
			length := binary.BigEndian.Uint32(rest[8:])
			if e := h.remote.entry; e != nil && c.activeLocked(r.at) && r.at.Before(e.expires) {
				if length == 0 {
					local = reply(dataPacket, r.originalID, appendString(nil, ""))
				} else if r.offset >= uint64(len(e.data)) {
					local = status(r.originalID, 1)
				} else {
					end := r.offset + uint64(length)
					if end > uint64(len(e.data)) {
						end = uint64(len(e.data))
					}
					body := appendString(nil, string(e.data[int(r.offset):int(end)]))
					local = reply(dataPacket, r.originalID, body)
				}
			}
		} else if r.kind == closePacket {
			h.closing = true
			delete(p.handles, alias)
			if !h.remote.writable && (h.remote.retained || h.remote.refs > 1) {
				h.remote.refs--
				p.maybeCloseLocked(h.remote)
				local = status(r.originalID, 0)
			} else if cp := h.remote.capture; !h.remote.writable && !h.remote.directory && cp != nil && cp.covered() && (len(cp.data) > 0 || cp.knownSize && cp.size == 0) && c.activeLocked(r.at) && r.at.Before(cp.expires) {
				// Exact-sized readers (including SSHFS) may never request EOF.
				// Confirm against this descriptor, never a potentially stale path.
				p.requestQueue = append(p.requestQueue, &request{kind: fstatPacket, internal: true, remote: h.remote, path: r.path, at: r.at, generation: c.generation, capture: cp, revision: cp.revision, covered: true, confirmClose: r})
				select {
				case p.wake <- struct{}{}:
				default:
				}
				c.mu.Unlock()
				return nil
			} else {
				// A pipelined READ response must not retain a descriptor after
				// its final CLOSE has already been forwarded upstream.
				h.remote.closing = true
			}
		} else if r.kind == fstatPacket && c.metadataTTL > 0 && c.activeLocked(r.at) {
			if m := h.remote.attrs; m != nil && r.at.Before(m.expires) {
				local = reply(m.kind, r.originalID, m.body)
			}
		}
		r.mutation = r.kind == writePacket || r.kind == fsetstatPacket || (r.kind == closePacket && h.remote.writable)
		if local == nil {
			packet, _ = replaceHandle(packet, h.remote.real)
			if r.kind == fstatPacket && h.remote.capture != nil {
				r.capture = h.remote.capture
				r.revision = r.capture.revision
				r.covered = r.capture.covered()
			}
		}
	case statPacket, lstatPacket, realpathPacket, readlinkPacket:
		path, _, ok := packetString(packet[5:])
		if !ok {
			parseErr = io.ErrUnexpectedEOF
			break
		}
		r.path = path
		if key, ok := c.cachePath(path); ok {
			r.path = key
			if m := c.metadata[metaKey{r.kind, key}]; m != nil && c.metadataTTL > 0 && c.activeLocked(r.at) && r.at.Before(m.expires) {
				local = reply(m.kind, r.originalID, m.body)
			}
		}
	case opendirPacket:
		r.path, _, _ = packetString(packet[5:])
	case extendedPacket:
		ext, rest, ok := packetString(packet[5:])
		if !ok {
			parseErr = io.ErrUnexpectedEOF
			break
		}
		// Forward extensions verbatim apart from namespaced descriptor values.
		if ext == "fsync@openssh.com" || ext == "fstatvfs@openssh.com" {
			alias, tail, ok := packetString(rest)
			if !ok {
				parseErr = io.ErrUnexpectedEOF
				break
			}
			h := p.handles[alias]
			if h == nil || h.closing {
				local = status(r.originalID, 4)
				break
			}
			r.handle, r.remote = h, h.remote
			packet = appendString(appendString(append([]byte(nil), packet[:5]...), ext), h.remote.real)
			packet = append(packet, tail...)
		}
		r.mutation = ext != "statvfs@openssh.com" && ext != "fstatvfs@openssh.com"
	default:
		// Unknown operations must not bypass cache invalidation.
		r.mutation = true
	}
	if parseErr != nil {
		c.mu.Unlock()
		return parseErr
	}
	if local != nil {
		c.mu.Unlock()
		return p.writeFront(local)
	}
	if r.mutation {
		c.invalidateLocked()
		c.busy++
		r.generation = c.generation
	}
	if r.remote != nil {
		r.remote.inflight++
	}
	packet, err := p.trackLocked(packet, r)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return p.writeUpstream(packet)
}

func (p *proxy) aliasLocked(rf *remoteFile) string {
	p.nextHandle++
	buf := binary.BigEndian.AppendUint64([]byte("\x00waldo-cache:"), p.nextHandle)
	alias := string(buf)
	p.handles[alias] = &handle{remote: rf}
	rf.refs++
	return alias
}

func (p *proxy) trackLocked(packet []byte, r *request) ([]byte, error) {
	if len(p.pending) >= 65536 {
		return nil, errors.New("fscache: too many pending requests")
	}
	for {
		p.nextID++
		if p.pending[p.nextID] == nil {
			break
		}
	}
	p.pending[p.nextID] = r
	packet = append([]byte(nil), packet...)
	binary.BigEndian.PutUint32(packet[1:5], p.nextID)
	return packet, nil
}

func (p *proxy) upstreamLoop() error {
	for {
		packet, err := readWirePacket(p.upstream)
		if err != nil {
			return err
		}
		if packet[0] == versionPacket {
			if err := p.writeFront(packet); err != nil {
				return err
			}
			continue
		}
		if len(packet) < 5 {
			return io.ErrUnexpectedEOF
		}
		id := binary.BigEndian.Uint32(packet[1:5])
		c := p.cache
		c.mu.Lock()
		r := p.pending[id]
		if r == nil {
			c.mu.Unlock()
			return fmt.Errorf("fscache: unknown response id %d", id)
		}
		delete(p.pending, id)
		packet, err = p.responseLocked(packet, r)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if !r.internal {
			binary.BigEndian.PutUint32(packet[1:5], r.originalID)
			if err := p.writeFront(packet); err != nil {
				return err
			}
		}
	}
}

func (p *proxy) responseLocked(packet []byte, r *request) ([]byte, error) {
	c := p.cache
	now := time.Now()
	c.sweepLocked(now)
	if r.remote != nil {
		r.remote.inflight--
	}
	if r.kind == openPacket || r.kind == opendirPacket {
		if packet[0] == handlePacket {
			real, _, ok := packetString(packet[5:])
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			rf := &remoteFile{real: real, path: r.path, writable: r.writable, directory: r.kind == opendirPacket, openedGeneration: r.generation}
			p.remotes[rf] = struct{}{}
			if rf.writable {
				c.busy++
			}
			if r.readonly && r.generation == c.generation && c.activeLocked(now) && c.contentTTL > 0 && now.Before(r.at.Add(c.contentTTL)) {
				if _, ok := c.cachePath(r.path); ok {
					rf.capture = &capture{generation: c.generation, expires: r.at.Add(c.contentTTL)}
					if m := c.metadata[metaKey{statPacket, r.path}]; m != nil && now.Before(m.expires) {
						rf.capture.size, rf.capture.knownSize, _ = attributeInfo(m.body)
						if rf.capture.size > maxFileSize {
							rf.capture = nil
						}
					}
				}
			}
			packet = reply(handlePacket, r.originalID, appendString(nil, p.aliasLocked(rf)))
		}
	} else if r.kind == closePacket && r.remote != nil {
		rf := r.remote
		if rf.attrs != nil {
			c.metadataBytes -= len(rf.attrs.body)
			rf.attrs = nil
		}
		if rf.entry != nil {
			c.dropEntryLocked(rf.entry)
		}
		if !r.internal {
			rf.refs--
		}
		if rf.writable {
			c.busy--
		}
		if rf.capture != nil {
			c.used -= cap(rf.capture.data)
			rf.capture = nil
		}
		rf.closed = true
		delete(p.remotes, rf)
	}
	if r.mutation {
		// Discard every concurrent fill before acknowledging the mutation.
		c.invalidateLocked()
		c.busy--
	}
	if !r.mutation && r.generation == c.generation && c.activeLocked(now) {
		switch r.kind {
		case statPacket, lstatPacket:
			if packet[0] == attrsPacket {
				if _, ok := c.cachePath(r.path); ok {
					c.storeMetaLocked(r.kind, r.path, attrsPacket, packet[5:], r.at)
				}
			}
		case realpathPacket, readlinkPacket:
			if packet[0] == namePacket {
				if _, ok := c.cachePath(r.path); ok {
					c.storeMetaLocked(r.kind, r.path, namePacket, packet[5:], r.at)
				}
			}
		case fstatPacket:
			if packet[0] == attrsPacket && r.remote != nil && !r.remote.closed && !r.remote.closing && r.remote.openedGeneration == c.generation {
				c.storeFileAttrsLocked(r.remote, packet[5:], r.at)
				if _, ok := c.cachePath(r.path); ok {
					c.storeMetaLocked(statPacket, r.path, attrsPacket, packet[5:], r.at)
				}
				if cp := r.remote.capture; cp != nil {
					cp.size, cp.knownSize, cp.regular = attributeInfo(packet[5:])
					cp.verified = cp == r.capture && cp.revision == r.revision && r.covered && cp.size == uint64(len(cp.data))
					if cp.size > maxFileSize {
						c.used -= cap(cp.data)
						r.remote.capture = nil
					} else {
						c.completeLocked(r.remote)
					}
				}
			}
		case readPacket:
			if r.remote != nil {
				if packet[0] == dataPacket {
					data, _, ok := packetString(packet[5:])
					if !ok {
						return nil, io.ErrUnexpectedEOF
					}
					c.learnLocked(r.remote, r.offset, []byte(data), false, r.generation)
				} else if code, ok := statusCode(packet); ok && code == 1 {
					c.learnLocked(r.remote, r.offset, nil, true, r.generation)
				}
			}
		}
	}
	if r.confirmClose != nil {
		closeRequest := r.confirmClose
		rf := r.remote
		if rf.entry != nil && c.activeLocked(now) && now.Before(rf.entry.expires) {
			rf.refs--
			r.internal = false
			r.originalID = closeRequest.originalID
			packet = status(r.originalID, 0)
		} else {
			rf.closing = true
			p.requestQueue = append(p.requestQueue, closeRequest)
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
	}
	if r.remote != nil {
		p.maybeCloseLocked(r.remote)
	}
	return packet, nil
}

func (p *proxy) maybeCloseLocked(rf *remoteFile) {
	if rf.closed || rf.closing || rf.retained || rf.refs > 0 || rf.inflight > 0 {
		return
	}
	rf.closing = true
	p.closeQueue = append(p.closeQueue, rf)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *proxy) maintenance() error {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return nil
		case <-p.wake:
		case <-ticker.C:
		}
		for {
			c := p.cache
			c.mu.Lock()
			c.sweepLocked(time.Now())
			if len(p.requestQueue) == 0 && len(p.closeQueue) == 0 {
				c.mu.Unlock()
				break
			}
			var r *request
			if len(p.requestQueue) > 0 {
				r = p.requestQueue[0]
				p.requestQueue = p.requestQueue[1:]
			} else {
				rf := p.closeQueue[0]
				p.closeQueue = p.closeQueue[1:]
				r = &request{kind: closePacket, internal: true, remote: rf, at: time.Now(), generation: c.generation}
			}
			rf := r.remote
			rf.inflight++
			packet, err := p.trackLocked(reply(r.kind, 0, appendString(nil, rf.real)), r)
			c.mu.Unlock()
			if err != nil {
				return err
			}
			if err := p.writeUpstream(packet); err != nil {
				return err
			}
		}
	}
}
