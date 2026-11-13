package audit

import (
	"encoding/binary"
	"io"
	"sync"
	"time"
)

const (
	maxFSStatsPacket  = 1 << 24
	maxFSStatsPending = 8192
)

// FSCounts describes completed SFTP requests observed on A. Durations are
// cumulative request latencies, so overlapping requests can exceed wall time.
// Cached operations answered on B and file access by commands on A are absent.
type FSCounts struct {
	ReadCount     uint64
	WriteCount    uint64
	ReadDuration  time.Duration
	WriteDuration time.Duration
	Dropped       uint64
}

type fsClass byte

const (
	fsRead fsClass = iota + 1
	fsWrite
)

type fsPending struct {
	class fsClass
	at    time.Time
}

// FSStats observes the two halves of one SFTP stream without keeping paths or
// file data. Each tap retains at most a fixed prefix and four OPEN flag bytes.
// Invalid framing disables only the observer, never the underlying stream.
type FSStats struct {
	mu        sync.Mutex
	counts    FSCounts
	pending   map[uint32]fsPending
	requests  fsStatsTap
	responses fsStatsTap
	disabled  bool
	closed    bool
}

func NewFSStats() *FSStats {
	s := &FSStats{pending: make(map[uint32]fsPending)}
	s.requests.stats, s.requests.request = s, true
	s.responses.stats = s
	return s
}

// Requests observes bytes sent from B to A; Responses observes A to B.
// Writers always report success, including after malformed data or Close.
func (s *FSStats) Requests() io.Writer  { return &s.requests }
func (s *FSStats) Responses() io.Writer { return &s.responses }

func (s *FSStats) Snapshot() FSCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts
}

// Close discards unfinished requests and freezes the counters.
func (s *FSStats) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.counts.Dropped += uint64(len(s.pending))
		s.pending = nil
		s.closed = true
	}
}

func (s *FSStats) disableLocked() {
	s.counts.Dropped += uint64(len(s.pending)) + 1
	s.pending = nil
	s.disabled = true
}

type fsStatsTap struct {
	stats   *FSStats
	request bool
	header  [5]byte
	headerN int
	total   uint32 // body length excluding packet type
	offset  uint32
	prefix  [128]byte
	prefixN int
	flags   [4]byte // OPEN flags follow a variable-length path; never keep it
	flagN   int
	started time.Time
}

func (t *fsStatsTap) Write(p []byte) (int, error) {
	n := len(p)
	s := t.stats
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(p) > 0 && !s.disabled && !s.closed {
		if t.headerN < len(t.header) {
			if t.headerN == 0 {
				t.started = time.Now()
			}
			used := copy(t.header[t.headerN:], p)
			t.headerN += used
			p = p[used:]
			if t.headerN < len(t.header) {
				break
			}
			size := binary.BigEndian.Uint32(t.header[:4])
			if size < 1 || size > maxFSStatsPacket {
				s.disableLocked()
				break
			}
			t.total, t.offset = size-1, 0
			t.prefixN, t.flagN = 0, 0
			if t.total == 0 {
				t.finishLocked()
			}
			continue
		}
		used := len(p)
		if uint32(used) > t.total-t.offset {
			used = int(t.total - t.offset)
		}
		chunk := p[:used]
		capture := chunk
		for len(capture) > 0 {
			limit := t.prefixLimit()
			if t.prefixN >= limit {
				break
			}
			copied := copy(t.prefix[t.prefixN:limit], capture)
			t.prefixN += copied
			capture = capture[copied:]
		}
		if t.request && t.header[4] == fxpOpen && t.prefixN >= 8 {
			// Even a long pathname can be classified without retaining its
			// bytes. Capture just the four bytes following that pathname.
			flagAt := uint64(8) + uint64(binary.BigEndian.Uint32(t.prefix[4:8]))
			begin := max(uint64(t.offset), flagAt)
			end := min(uint64(t.offset)+uint64(used), flagAt+4)
			if begin < end && flagAt+4 <= uint64(t.total) {
				copy(t.flags[begin-flagAt:end-flagAt], chunk[begin-uint64(t.offset):end-uint64(t.offset)])
				t.flagN += int(end - begin)
			}
		}
		t.offset += uint32(used)
		p = p[used:]
		if t.offset == t.total {
			t.finishLocked()
		}
	}
	return n, nil
}

// Keep only request IDs, OPEN path lengths, and recognized-length extension
// names. Paths and READ/WRITE data are skipped rather than copied.
func (t *fsStatsTap) prefixLimit() int {
	if t.request && t.header[4] == fxpOpen {
		return 8
	}
	if t.request && t.header[4] == fxpExtended {
		if t.prefixN < 8 {
			return 8
		}
		length := binary.BigEndian.Uint32(t.prefix[4:8])
		if length <= uint32(len(t.prefix)-8) {
			return 8 + int(length)
		}
		return 8
	}
	return 4
}

func (t *fsStatsTap) finishLocked() {
	t.headerN = 0
	typ := t.header[4]
	if typ == 1 || typ == 2 { // INIT and VERSION have no request ID
		return
	}
	if t.prefixN < 4 {
		t.stats.disableLocked()
		return
	}
	id := binary.BigEndian.Uint32(t.prefix[:4])
	s := t.stats
	if t.request {
		class := t.classify()
		if class == 0 {
			return
		}
		if _, duplicate := s.pending[id]; duplicate {
			// Reused in-flight IDs cannot be correlated safely.
			s.disableLocked()
			return
		}
		if len(s.pending) >= maxFSStatsPending {
			s.counts.Dropped++
			return
		}
		s.pending[id] = fsPending{class: class, at: t.started}
		return
	}
	switch typ {
	case 101, 102, 103, 104, 105, 201: // STATUS, HANDLE, DATA, NAME, ATTRS, EXTENDED_REPLY
	default:
		return
	}
	if pending, found := s.pending[id]; found {
		delete(s.pending, id)
		duration := time.Since(pending.at)
		if pending.class == fsRead {
			s.counts.ReadCount++
			s.counts.ReadDuration += duration
		} else {
			s.counts.WriteCount++
			s.counts.WriteDuration += duration
		}
	}
}

func (t *fsStatsTap) classify() fsClass {
	switch t.header[4] {
	case fxpOpen:
		if t.flagN != len(t.flags) {
			t.stats.counts.Dropped++
			return 0
		}
		if binary.BigEndian.Uint32(t.flags[:])&0x3e != 0 {
			return fsWrite
		}
		return fsRead
	case 5, 7, 8, 11, 12, 16, 17, 19: // READ, LSTAT, FSTAT, OPENDIR, READDIR, REALPATH, STAT, READLINK
		return fsRead
	case 6, fxpSetstat, 10, fxpRemove, fxpMkdir, fxpRmdir, fxpRename, fxpSymlink:
		return fsWrite
	case fxpExtended:
		if t.prefixN < 8 {
			return 0
		}
		length := binary.BigEndian.Uint32(t.prefix[4:8])
		if length > uint32(t.prefixN-8) {
			return 0
		}
		switch string(t.prefix[8 : 8+length]) {
		case "posix-rename@openssh.com", "hardlink@openssh.com", "fsync@openssh.com", "lsetstat@openssh.com":
			return fsWrite
		case "statvfs@openssh.com", "fstatvfs@openssh.com":
			return fsRead
		}
	}
	return 0 // CLOSE and unknown extensions are neither reads nor writes
}
