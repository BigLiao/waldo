package audit

import "encoding/binary"

// SFTP packet types we care about (draft-ietf-secsh-filexfer-02, v3).
const (
	fxpOpen     = 3
	fxpSetstat  = 9
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRename   = 18
	fxpSymlink  = 20
	fxpExtended = 200

	pflagWrite  = 0x02
	pflagAppend = 0x04
	pflagCreat  = 0x08
	pflagTrunc  = 0x10

	maxInteresting = 64 << 10
)

// SFTPSniffer observes the client->server half of an SFTP stream and reports
// mutating operations. It never fails the stream: on anything unexpected it
// stops sniffing.
type SFTPSniffer struct {
	report func(op, path, to string)

	hdr      [5]byte
	hdrN     int
	need     int // bytes remaining in the current packet body (after type)
	keep     bool
	body     []byte
	typ      byte
	disabled bool
}

func NewSFTPSniffer(report func(op, path, to string)) *SFTPSniffer {
	return &SFTPSniffer{report: report}
}

func (s *SFTPSniffer) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !s.disabled {
		if s.need == 0 && s.hdrN < 5 {
			c := copy(s.hdr[s.hdrN:], p)
			s.hdrN += c
			p = p[c:]
			if s.hdrN < 5 {
				break
			}
			l := int(binary.BigEndian.Uint32(s.hdr[:4]))
			if l < 1 || l > 1<<24 {
				s.disabled = true
				break
			}
			s.typ = s.hdr[4]
			s.need = l - 1
			s.keep = interesting(s.typ) && s.need <= maxInteresting
			s.body = s.body[:0]
			if s.need == 0 {
				s.finish()
			}
			continue
		}
		c := s.need
		if c > len(p) {
			c = len(p)
		}
		if s.keep {
			s.body = append(s.body, p[:c]...)
		}
		s.need -= c
		p = p[c:]
		if s.need == 0 {
			s.finish()
		}
	}
	return n, nil
}

func interesting(t byte) bool {
	switch t {
	case fxpOpen, fxpSetstat, fxpRemove, fxpMkdir, fxpRmdir, fxpRename, fxpSymlink, fxpExtended:
		return true
	}
	return false
}

func (s *SFTPSniffer) finish() {
	s.hdrN = 0
	if !s.keep {
		return
	}
	b := s.body
	if len(b) < 4 {
		return
	}
	b = b[4:] // request id
	str := func() (string, bool) {
		if len(b) < 4 {
			return "", false
		}
		l := int(binary.BigEndian.Uint32(b))
		if l > len(b)-4 {
			return "", false
		}
		v := string(b[4 : 4+l])
		b = b[4+l:]
		return v, true
	}
	switch s.typ {
	case fxpOpen:
		path, ok := str()
		if !ok || len(b) < 4 {
			return
		}
		fl := binary.BigEndian.Uint32(b)
		if fl&(pflagWrite|pflagAppend|pflagCreat|pflagTrunc) != 0 {
			s.report("write", path, "")
		}
	case fxpSetstat:
		if path, ok := str(); ok {
			s.report("setstat", path, "")
		}
	case fxpRemove:
		if path, ok := str(); ok {
			s.report("remove", path, "")
		}
	case fxpMkdir:
		if path, ok := str(); ok {
			s.report("mkdir", path, "")
		}
	case fxpRmdir:
		if path, ok := str(); ok {
			s.report("rmdir", path, "")
		}
	case fxpRename:
		from, ok1 := str()
		to, ok2 := str()
		if ok1 && ok2 {
			s.report("rename", from, to)
		}
	case fxpSymlink:
		a, ok1 := str()
		c, ok2 := str()
		if ok1 && ok2 {
			s.report("symlink", a, c)
		}
	case fxpExtended:
		name, ok := str()
		if !ok {
			return
		}
		switch name {
		case "posix-rename@openssh.com":
			from, ok1 := str()
			to, ok2 := str()
			if ok1 && ok2 {
				s.report("rename", from, to)
			}
		case "hardlink@openssh.com":
			from, ok1 := str()
			to, ok2 := str()
			if ok1 && ok2 {
				s.report("link", from, to)
			}
		}
	}
}
