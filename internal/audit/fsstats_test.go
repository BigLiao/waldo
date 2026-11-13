package audit

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func BenchmarkFSStatsReadRoundTrip(b *testing.B) {
	stats := NewFSStats()
	request := fsStatsPacket(5, 1, "handle", uint64bytes(0), uint32(4096))
	response := fsStatsPacket(103, 1, strings.Repeat("x", 4096))
	b.ReportAllocs()
	b.SetBytes(int64(len(request) + len(response)))
	for b.Loop() {
		stats.Requests().Write(request)
		stats.Responses().Write(response)
	}
}

func fsStatsPacket(typ byte, id uint32, parts ...any) []byte {
	b := pkt(typ, parts...)
	binary.BigEndian.PutUint32(b[5:9], id)
	return b
}

func fsStatsWrite(t *testing.T, w io.Writer, b []byte, fragment int) {
	t.Helper()
	for len(b) > 0 {
		n := min(fragment, len(b))
		if got, err := w.Write(b[:n]); got != n || err != nil {
			t.Fatalf("tap altered stream write: n=%d, err=%v", got, err)
		}
		b = b[n:]
	}
}

func TestFSStatsFragmentedRequestsAndOutOfOrderResponses(t *testing.T) {
	s := NewFSStats()
	// Include a pathname well beyond the fixed prefix, fragmented so the
	// OPEN flags straddle incoming writes. Both forms must classify correctly.
	requests := append(fsStatsPacket(fxpOpen, 11, "/read", uint32(1), uint32(0)),
		fsStatsPacket(fxpOpen, 12, "/"+strings.Repeat("long", 1000), uint32(0x1a), uint32(0))...)
	requests = append(requests, fsStatsPacket(5, 13, "handle", uint64bytes(0), uint32(20))...)
	requests = append(requests, fsStatsPacket(12, 14, "directory")...)
	fsStatsWrite(t, s.Requests(), requests, 3)
	if got := s.Snapshot(); got.ReadCount != 0 || got.WriteCount != 0 {
		t.Fatalf("counted requests before completion: %+v", got)
	}
	// Add a known delay to demonstrate request-ID correlation, without a sleep.
	pending := s.pending[12]
	pending.at = time.Now().Add(-time.Second)
	s.pending[12] = pending
	responses := append(fsStatsPacket(101, 14, uint32(1), "EOF", ""), fsStatsPacket(102, 12, "h")...)
	responses = append(responses, fsStatsPacket(103, 13, "contents")...)
	responses = append(responses, fsStatsPacket(102, 11, "h")...)
	fsStatsWrite(t, s.Responses(), responses, 1)
	got := s.Snapshot()
	if got.ReadCount != 3 || got.WriteCount != 1 || got.Dropped != 0 || got.WriteDuration < time.Second || got.ReadDuration <= 0 {
		t.Fatalf("counts = %+v", got)
	}
	if len(s.pending) != 0 {
		t.Fatalf("completed IDs retained: %v", s.pending)
	}
	// EOF and errors are completed RPCs, not missing observations. Repeated or
	// unknown response IDs must never count twice.
	fsStatsWrite(t, s.Responses(), fsStatsPacket(101, 14, uint32(1)), 100)
	if s.Snapshot() != got {
		t.Fatal("unknown response changed counters")
	}
}

func TestFSStatsClassifiesMetadataAndExtensions(t *testing.T) {
	s := NewFSStats()
	types := []byte{7, 8, 11, 12, 16, 17, 19, 6, 9, 10, 13, 14, 15, 18, 20}
	for i, typ := range types {
		id := uint32(i + 1)
		fsStatsWrite(t, s.Requests(), fsStatsPacket(typ, id, "ignored"), 100)
		fsStatsWrite(t, s.Responses(), fsStatsPacket(101, id, uint32(0)), 100)
	}
	for i, name := range []string{"posix-rename@openssh.com", "hardlink@openssh.com", "fsync@openssh.com", "statvfs@openssh.com", "fstatvfs@openssh.com", "unknown"} {
		id := uint32(100 + i)
		fsStatsWrite(t, s.Requests(), fsStatsPacket(fxpExtended, id, name, "/secret-path"), 1)
		fsStatsWrite(t, s.Responses(), fsStatsPacket(201, id, []byte{0, 0}), 100)
	}
	// CLOSE and protocol version negotiation are outside these categories.
	fsStatsWrite(t, s.Requests(), fsStatsPacket(4, 300, "handle"), 100)
	fsStatsWrite(t, s.Responses(), fsStatsPacket(101, 300, uint32(0)), 100)
	fsStatsWrite(t, s.Requests(), fsStatsPacket(1, 3), 100)
	fsStatsWrite(t, s.Responses(), fsStatsPacket(2, 3), 100)
	got := s.Snapshot()
	if got.ReadCount != 9 || got.WriteCount != 11 || got.Dropped != 0 {
		t.Fatalf("counts = %+v", got)
	}
}

func TestFSStatsSkipsDataAndPathContents(t *testing.T) {
	s := NewFSStats()
	secret := bytes.Repeat([]byte("private-file-contents"), 200000)
	fsStatsWrite(t, s.Requests(), fsStatsPacket(6, 1, "handle", uint64bytes(0), secret), 32768)
	fsStatsWrite(t, s.Responses(), fsStatsPacket(101, 1, uint32(0)), 32768)
	fsStatsWrite(t, s.Requests(), fsStatsPacket(5, 2, "handle", uint64bytes(0), uint32(len(secret))), 32768)
	fsStatsWrite(t, s.Responses(), fsStatsPacket(103, 2, secret), 32768)
	if s.requests.prefixN != 4 || s.responses.prefixN != 4 {
		t.Fatal("copied more than request IDs for file data")
	}
	fsStatsWrite(t, s.Requests(), fsStatsPacket(fxpOpen, 3, "/private-path", uint32(1), uint32(0)), 32768)
	if s.requests.prefixN != 8 || bytes.Contains(s.requests.prefix[:], []byte("private")) {
		t.Fatal("retained pathname")
	}
	fsStatsWrite(t, s.Requests(), fsStatsPacket(fxpExtended, 4, "fsync@openssh.com", "/private-path"), 32768)
	if bytes.Contains(s.requests.prefix[:], []byte("private")) {
		t.Fatal("retained extension arguments")
	}
}

func TestFSStatsLimitsAndMalformedFrames(t *testing.T) {
	s := NewFSStats()
	for id := uint32(0); id < maxFSStatsPending+5; id++ {
		fsStatsWrite(t, s.Requests(), fsStatsPacket(17, id, "/p"), 100)
	}
	if len(s.pending) != maxFSStatsPending || s.Snapshot().Dropped != 5 {
		t.Fatalf("unbounded pending map: %d, %+v", len(s.pending), s.Snapshot())
	}
	s.Close()
	s.Close()
	got := s.Snapshot()
	if got.Dropped != maxFSStatsPending+5 || len(s.pending) != 0 {
		t.Fatalf("close counts = %+v", got)
	}
	fsStatsWrite(t, s.Responses(), fsStatsPacket(105, 1), 100)
	if s.Snapshot() != got {
		t.Fatal("closed observer resumed counting")
	}
	for _, malformed := range [][]byte{
		{0, 0, 0, 0, 17},
		{1, 0, 0, 1, 17}, // more than 16 MiB
		{0, 0, 0, 1, 17}, // missing request ID
	} {
		s := NewFSStats()
		fsStatsWrite(t, s.Requests(), fsStatsPacket(17, 1, "/p"), 100)
		fsStatsWrite(t, s.Requests(), malformed, 1)
		fsStatsWrite(t, s.Requests(), fsStatsPacket(17, 2, "/p"), 100)
		fsStatsWrite(t, s.Responses(), fsStatsPacket(105, 1), 100)
		if got := s.Snapshot(); !s.disabled || len(s.pending) != 0 || got.ReadCount != 0 || got.Dropped != 2 {
			t.Fatalf("malformed packet was not isolated: %+v", got)
		}
	}
}

func TestFSStatsConcurrentDirectionsAndSnapshots(t *testing.T) {
	s := NewFSStats()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for id := uint32(0); id < 1000; id++ {
			s.Requests().Write(fsStatsPacket(17, id, "/p"))
		}
	}()
	go func() {
		defer wg.Done()
		for id := uint32(0); id < 1000; id++ {
			s.Responses().Write(fsStatsPacket(105, id))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			s.Snapshot()
		}
	}()
	wg.Wait()
	s.Close()
}
