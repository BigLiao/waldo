package fscache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"waldo/internal/fileversion"
)

type handle struct {
	real, path, alias                    string
	checked, cached, statServed, closing bool
	info                                 fileversion.Info
	body                                 *blob
	spans                                []span
}
type request struct {
	id               uint32
	kind             byte
	path             string
	h                *handle
	packet           []byte // original OPEN retained only until an unsupported-extension reply
	checked, closing bool
	offset           uint64
}
type proxy struct {
	cache               *Cache
	front, upstream     io.ReadWriteCloser
	frontMu, upstreamMu sync.Mutex
	pending             map[uint32]*request
	handles             map[string]*handle
	nextID              uint32
	nextHandle          uint64
	opening             int
}

func (c *Cache) Serve(front, upstream io.ReadWriteCloser) error {
	c.mu.Lock()
	if c.serving {
		c.mu.Unlock()
		return errors.New("fscache: already serving")
	}
	c.serving = true
	c.mu.Unlock()
	p := &proxy{cache: c, front: front, upstream: upstream, pending: map[uint32]*request{}, handles: map[string]*handle{}}
	results := make(chan error, 2)
	go func() { results <- p.frontLoop() }()
	go func() { results <- p.upstreamLoop() }()
	err := <-results
	front.Close()
	upstream.Close()
	<-results
	c.mu.Lock()
	for path := range c.files {
		c.drop(path)
	}
	for _, h := range p.handles {
		c.release(h.body)
		h.body = nil
	}
	c.serving = false
	c.mu.Unlock()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func (p *proxy) writeFront(b []byte) error {
	p.frontMu.Lock()
	defer p.frontMu.Unlock()
	return writeWirePacket(p.front, b)
}
func (p *proxy) writeUpstream(b []byte) error {
	p.upstreamMu.Lock()
	defer p.upstreamMu.Unlock()
	return writeWirePacket(p.upstream, b)
}
func (p *proxy) track(b []byte, r *request) ([]byte, error) {
	if len(p.pending) >= 65536 {
		return nil, fmt.Errorf("too many SFTP requests")
	}
	for {
		p.nextID++
		if p.pending[p.nextID] == nil {
			break
		}
	}
	p.pending[p.nextID] = r
	b = append([]byte(nil), b...)
	binary.BigEndian.PutUint32(b[1:5], p.nextID)
	return b, nil
}
func (p *proxy) alias(h *handle) string {
	p.nextHandle++
	s := fmt.Sprintf("\x00waldo-cache:%d", p.nextHandle)
	h.alias = s
	p.handles[s] = h
	return s
}
func (p *proxy) frontLoop() error {
	for {
		b, err := readWirePacket(p.front)
		if err != nil {
			return err
		}
		if b[0] == initPacket {
			if err = p.writeUpstream(b); err != nil {
				return err
			}
			continue
		}
		if err = p.forward(b); err != nil {
			return err
		}
	}
}

func (p *proxy) forward(b []byte) error {
	if len(b) < 5 {
		return io.ErrUnexpectedEOF
	}
	r := &request{id: binary.BigEndian.Uint32(b[1:5]), kind: b[0]}
	c := p.cache
	c.mu.Lock()
	c.sweep(time.Now())
	var local []byte
	var err error
	if r.kind == openPacket || r.kind == opendirPacket {
		if len(p.handles)+p.opening >= maxHandles {
			c.mu.Unlock()
			return p.writeFront(status(r.id, 4))
		}
		p.opening++
	}
	switch r.kind {
	case openPacket:
		var rest []byte
		var ok bool
		r.path, rest, ok = packetString(b[5:])
		if !ok || len(rest) < 4 {
			err = io.ErrUnexpectedEOF
			break
		}
		flags := binary.BigEndian.Uint32(rest)
		if flags == 1 && c.cachePath(r.path) {
			r.checked = true
			r.packet = append([]byte(nil), b...)
			b = reply(extendedPacket, r.id, appendString(appendString(nil, fileversion.Open), r.path))
		} else if flags&0x3e != 0 {
			c.drop(r.path)
		}
	case readPacket, writePacket, closePacket, fstatPacket, fsetstatPacket, readdirPacket:
		alias, rest, ok := packetString(b[5:])
		if !ok {
			err = io.ErrUnexpectedEOF
			break
		}
		h := p.handles[alias]
		if h == nil || h.closing {
			local = status(r.id, 4)
			break
		}
		r.h = h
		r.path = h.path
		if r.kind == readPacket {
			if len(rest) != 12 {
				err = io.ErrUnexpectedEOF
				break
			}
			r.offset = binary.BigEndian.Uint64(rest)
			length := uint64(binary.BigEndian.Uint32(rest[8:]))
			if h.cached && h.body != nil {
				if length == 0 {
					local = reply(dataPacket, r.id, appendString(nil, ""))
				} else if r.offset >= uint64(len(h.body.data)) {
					local = status(r.id, 1)
				} else {
					end := min(r.offset+length, uint64(len(h.body.data)))
					local = reply(dataPacket, r.id, appendString(nil, string(h.body.data[int(r.offset):int(end)])))
				}
			}
		}
		if r.kind == fstatPacket && h.checked && !h.statServed {
			h.statServed = true
			local = reply(attrsPacket, r.id, h.info.Attributes())
		}
		if r.kind == closePacket {
			h.closing = true
			r.closing = true
			if h.checked {
				r.checked = true
				b = reply(extendedPacket, r.id, appendString(appendString(nil, fileversion.Close), h.real))
			}
		}
		if r.kind == writePacket || r.kind == fsetstatPacket {
			c.drop(h.path)
		}
		if local == nil && !(r.closing && r.checked) {
			b, _ = replaceHandle(b, h.real)
		}
	case renamePacket, removePacket, setstatPacket:
		path, rest, ok := packetString(b[5:])
		if ok {
			c.drop(path)
			if r.kind == renamePacket {
				target, _, ok := packetString(rest)
				if ok {
					c.drop(target)
				}
			}
		}
	case extendedPacket:
		ext, rest, ok := packetString(b[5:])
		if ok && (ext == "fsync@openssh.com" || ext == "fstatvfs@openssh.com") {
			alias, tail, ok := packetString(rest)
			if !ok {
				err = io.ErrUnexpectedEOF
				break
			}
			h := p.handles[alias]
			if h == nil {
				local = status(r.id, 4)
				break
			}
			if h.checked {
				local = status(r.id, 8)
			} else {
				b = reply(extendedPacket, r.id, appendString(appendString(nil, ext), h.real))
				b = append(b, tail...)
			}
		}
	}
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if local != nil {
		c.mu.Unlock()
		return p.writeFront(local)
	}
	b, err = p.track(b, r)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return p.writeUpstream(b)
}

func (p *proxy) upstreamLoop() error {
	for {
		b, err := readWirePacket(p.upstream)
		if err != nil {
			return err
		}
		if b[0] == versionPacket {
			if err = p.writeFront(b); err != nil {
				return err
			}
			continue
		}
		if len(b) < 5 {
			return io.ErrUnexpectedEOF
		}
		c := p.cache
		c.mu.Lock()
		id := binary.BigEndian.Uint32(b[1:5])
		r := p.pending[id]
		if r == nil {
			c.mu.Unlock()
			return fmt.Errorf("unknown SFTP response %d", id)
		}
		delete(p.pending, id)
		if r.kind == openPacket && r.checked && b[0] == statusPacket {
			if code, ok := statusCode(b); ok && code == 8 {
				r.checked = false
				next, e := p.track(r.packet, r)
				c.mu.Unlock()
				if e != nil {
					return e
				}
				if e = p.writeUpstream(next); e != nil {
					return e
				}
				continue
			}
		}
		b, err = p.response(b, r)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		binary.BigEndian.PutUint32(b[1:5], r.id)
		if err = p.writeFront(b); err != nil {
			return err
		}
	}
}

func (p *proxy) response(b []byte, r *request) ([]byte, error) {
	c := p.cache
	if r.kind == openPacket || r.kind == opendirPacket {
		p.opening--
		var real string
		var info fileversion.Info
		checked := false
		if r.checked && b[0] == fileversion.Reply {
			h, rest, ok := packetString(b[5:])
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			s, tail, ok := packetString(rest)
			if !ok || len(tail) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			var err error
			info, err = fileversion.Decode(s)
			if err != nil {
				return nil, err
			}
			real, checked = h, true
		} else if b[0] == handlePacket {
			var ok bool
			real, _, ok = packetString(b[5:])
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
		} else {
			if r.kind == openPacket {
				c.drop(r.path)
			}
			return b, nil
		}
		h := &handle{real: real, path: r.path, checked: checked, info: info}
		if checked && info.Regular() && info.Size <= maxFileSize {
			e := c.files[r.path]
			if e != nil && e.version == info.Version {
				h.body = e.body
				h.body.refs++
				h.cached = true
				c.touch(e)
			} else {
				c.drop(r.path)
				h.body = &blob{refs: 1}
			}
		}
		return reply(handlePacket, r.id, appendString(nil, p.alias(h))), nil
	}
	if r.kind == readPacket && r.h != nil && !r.h.cached && r.h.body != nil && b[0] == dataPacket {
		data, _, ok := packetString(b[5:])
		if !ok {
			return nil, io.ErrUnexpectedEOF
		}
		h := r.h
		if r.offset > maxFileSize || uint64(len(data)) > maxFileSize-r.offset || !c.grow(h.body, int(r.offset)+len(data)) {
			c.release(h.body)
			h.body = nil
			h.spans = nil
		} else {
			copy(h.body.data[int(r.offset):], data)
			h.spans = addSpan(h.spans, span{int(r.offset), int(r.offset) + len(data)})
			if len(h.spans) > 256 {
				c.release(h.body)
				h.body = nil
				h.spans = nil
			}
		}
	}
	if r.closing {
		h := r.h
		if r.checked && b[0] == fileversion.Reply {
			s, rest, ok := packetString(b[5:])
			if !ok || len(rest) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			info, err := fileversion.Decode(s)
			if err != nil {
				return nil, err
			}
			if !h.cached && h.body != nil && info.Version == h.info.Version && info.Size == int64(len(h.body.data)) && (info.Size == 0 || len(h.spans) == 1 && h.spans[0] == (span{0, int(info.Size)})) {
				c.store(h.path, info.Version, h.body)
			}
			b = status(r.id, 0)
		}
		delete(p.handles, h.alias)
		c.release(h.body)
		h.body = nil
		h.spans = nil
	}
	return b, nil
}
