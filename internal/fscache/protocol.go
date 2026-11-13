package fscache

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	initPacket     byte = 1
	versionPacket  byte = 2
	openPacket     byte = 3
	closePacket    byte = 4
	readPacket     byte = 5
	writePacket    byte = 6
	lstatPacket    byte = 7
	fstatPacket    byte = 8
	setstatPacket  byte = 9
	fsetstatPacket byte = 10
	opendirPacket  byte = 11
	readdirPacket  byte = 12
	removePacket   byte = 13
	mkdirPacket    byte = 14
	rmdirPacket    byte = 15
	realpathPacket byte = 16
	statPacket     byte = 17
	renamePacket   byte = 18
	readlinkPacket byte = 19
	symlinkPacket  byte = 20
	statusPacket   byte = 101
	handlePacket   byte = 102
	dataPacket     byte = 103
	namePacket     byte = 104
	attrsPacket    byte = 105
	extendedPacket byte = 200
)

func readWirePacket(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < 1 || n > 16<<20 {
		return nil, fmt.Errorf("invalid SFTP packet size %d", n)
	}
	p := make([]byte, int(n))
	_, err := io.ReadFull(r, p)
	return p, err
}

func writeWirePacket(w io.Writer, p []byte) error {
	buf := binary.BigEndian.AppendUint32(nil, uint32(len(p)))
	buf = append(buf, p...)
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

func packetString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return "", nil, false
	}
	return string(b[4 : 4+int(n)]), b[4+int(n):], true
}

func appendString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func reply(kind byte, id uint32, body []byte) []byte {
	p := binary.BigEndian.AppendUint32([]byte{kind}, id)
	return append(p, body...)
}

func status(id, code uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, code)
	b = appendString(b, "")
	b = appendString(b, "")
	return reply(statusPacket, id, b)
}

func statusCode(p []byte) (uint32, bool) {
	if len(p) < 9 || p[0] != statusPacket {
		return 0, false
	}
	return binary.BigEndian.Uint32(p[5:9]), true
}

func attributeInfo(body []byte) (size uint64, knownSize, regular bool) {
	if len(body) < 4 {
		return
	}
	flags, offset := binary.BigEndian.Uint32(body), 4
	if flags&1 != 0 {
		if len(body) < offset+8 {
			return
		}
		size, knownSize = binary.BigEndian.Uint64(body[offset:]), true
		offset += 8
	}
	if flags&2 != 0 {
		offset += 8
	}
	if flags&4 != 0 && len(body) >= offset+4 {
		regular = binary.BigEndian.Uint32(body[offset:])&0170000 == 0100000
	}
	return
}

func replaceHandle(p []byte, h string) ([]byte, bool) {
	_, rest, ok := packetString(p[5:])
	if !ok {
		return nil, false
	}
	out := appendString(append([]byte(nil), p[:5]...), h)
	return append(out, rest...), true
}
