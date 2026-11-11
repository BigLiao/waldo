package audit

import (
	"encoding/binary"
	"testing"
)

func pkt(typ byte, parts ...any) []byte {
	var body []byte
	body = append(body, typ)
	body = binary.BigEndian.AppendUint32(body, 7) // id
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			body = binary.BigEndian.AppendUint32(body, uint32(len(v)))
			body = append(body, v...)
		case uint32:
			body = binary.BigEndian.AppendUint32(body, v)
		case []byte:
			body = append(body, v...)
		}
	}
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func TestSniffer(t *testing.T) {
	var got []string
	s := NewSFTPSniffer(func(op, path, to string) { got = append(got, op+":"+path+":"+to) })
	var stream []byte
	stream = append(stream, pkt(3, "/p/read.txt", uint32(0x01), uint32(0))...)
	stream = append(stream, pkt(3, "/p/new.txt", uint32(0x1a), uint32(0))...)
	stream = append(stream, pkt(6, "handle", uint64bytes(0), "lots of data that must be skipped")...)
	stream = append(stream, pkt(200, "posix-rename@openssh.com", "/p/a", "/p/b")...)
	stream = append(stream, pkt(13, "/p/gone")...)
	// Feed one byte at a time to exercise reassembly.
	for i := range stream {
		s.Write(stream[i : i+1])
	}
	want := []string{"write:/p/new.txt:", "rename:/p/a:/p/b", "remove:/p/gone:"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func uint64bytes(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
