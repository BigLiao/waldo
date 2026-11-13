package session

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"waldo/internal/audit"
)

func fileObservationPacket(typ byte, id uint32, parts ...any) []byte {
	body := binary.BigEndian.AppendUint32([]byte{typ}, id)
	for _, part := range parts {
		switch v := part.(type) {
		case string:
			body = binary.BigEndian.AppendUint32(body, uint32(len(v)))
			body = append(body, v...)
		case uint32:
			body = binary.BigEndian.AppendUint32(body, v)
		case uint64:
			body = binary.BigEndian.AppendUint64(body, v)
		}
	}
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func readFileObservationLog(t *testing.T, dir string) ([]map[string]any, []byte) {
	t.Helper()
	raw, err := os.ReadFile(audit.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events, raw
}

func TestSpliceObservedFSPreservesBothDirectionsAndFlushesCounts(t *testing.T) {
	stub, remote := net.Pipe()
	sandbox, process := net.Pipe()
	for _, conn := range []net.Conn{stub, remote, sandbox, process} {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	dir := t.TempDir()
	logger, err := audit.Open(dir, "file-observe")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	readSecret := strings.Repeat("private-read-contents", 40000)
	writeSecret := strings.Repeat("private-write-contents", 40000)
	requests := fileObservationPacket(3, 1, "/private-read-path", uint32(1), uint32(0))
	requests = append(requests, fileObservationPacket(5, 2, "read-handle", uint64(0), uint32(len(readSecret)))...)
	requests = append(requests, fileObservationPacket(3, 3, "/private-write-path", uint32(0x1a), uint32(0))...)
	requests = append(requests, fileObservationPacket(6, 4, "write-handle", uint64(0), writeSecret)...)
	requests = append(requests, fileObservationPacket(12, 5, "directory")...)
	responses := fileObservationPacket(102, 1, "read-handle")
	responses = append(responses, fileObservationPacket(103, 2, readSecret)...)
	responses = append(responses, fileObservationPacket(102, 3, "write-handle")...)
	responses = append(responses, fileObservationPacket(101, 4, uint32(0), "", "")...)
	responses = append(responses, fileObservationPacket(101, 5, uint32(1), "EOF", "")...)

	var observedRequests bytes.Buffer
	var mutations []string
	mutationTap := audit.NewSFTPSniffer(func(op, path, to string) {
		mutations = append(mutations, op+":"+path)
	})
	done := make(chan struct{})
	go func() {
		spliceObservedFS(remote, sandbox, io.MultiWriter(&observedRequests, mutationTap), logger)
		close(done)
	}()
	processResult := make(chan error, 1)
	go func() {
		got := make([]byte, len(requests))
		if _, err := io.ReadFull(process, got); err != nil {
			processResult <- err
			return
		}
		if !bytes.Equal(got, requests) {
			processResult <- io.ErrUnexpectedEOF
			return
		}
		_, err := process.Write(responses)
		process.Close()
		processResult <- err
	}()
	if _, err := stub.Write(requests); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stub)
	if err != nil || !bytes.Equal(got, responses) {
		t.Fatalf("response changed: equal=%t, err=%v", bytes.Equal(got, responses), err)
	}
	if err := <-processResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("file splice did not finish")
	}
	if !bytes.Equal(observedRequests.Bytes(), requests) {
		t.Fatal("existing mutation tap did not receive original request bytes")
	}
	if len(mutations) != 1 || mutations[0] != "write:/private-write-path" {
		t.Fatalf("mutation audit changed: %v", mutations)
	}

	events, raw := readFileObservationLog(t, dir)
	if len(events) != 2 {
		t.Fatalf("expected one final read/write aggregate, got %s", raw)
	}
	for _, event := range events {
		wantCount := float64(2)
		if event["kind"] == "file-read" {
			wantCount = 3 // read-only OPEN, READ, and the normal READDIR EOF
		} else if event["kind"] != "file-write" {
			t.Fatalf("unexpected event: %v", event)
		}
		if event["count"] != wantCount || event["scope"] != "sftp_rpc" || event["s"] != "file-observe" {
			t.Fatalf("unexpected aggregate: %v", event)
		}
		if duration, ok := event["duration_ms"].(float64); !ok || duration <= 0 {
			t.Fatalf("missing duration: %v", event)
		}
	}
	for _, secret := range []string{"private-read-contents", "private-write-contents", "/private-read-path", "/private-write-path"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("statistics retained sensitive content: %q", secret)
		}
	}
}

func TestSpliceObservedFSReportsPendingRequestAtDisconnect(t *testing.T) {
	stub, remote := net.Pipe()
	sandbox, process := net.Pipe()
	for _, conn := range []net.Conn{stub, remote, sandbox, process} {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	dir := t.TempDir()
	logger, err := audit.Open(dir, "file-disconnect")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	done := make(chan struct{})
	go func() {
		spliceObservedFS(remote, sandbox, io.Discard, logger)
		close(done)
	}()
	request := fileObservationPacket(17, 7, "/private-unfinished-path")
	processResult := make(chan error, 1)
	go func() {
		got := make([]byte, len(request))
		_, err := io.ReadFull(process, got)
		process.Close() // the source disappears without answering STAT
		processResult <- err
	}()
	if _, err := stub.Write(request); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(stub); err != nil {
		t.Fatal(err)
	}
	if err := <-processResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected file splice did not finish")
	}
	events, raw := readFileObservationLog(t, dir)
	if len(events) != 1 || events[0]["kind"] != "file-stats-dropped" || events[0]["count"] != float64(1) {
		t.Fatalf("unfinished request was lost or counted as completed: %s", raw)
	}
	if bytes.Contains(raw, []byte("private-unfinished")) {
		t.Fatal("statistics retained unfinished path")
	}
}

func TestLogFileCountsDoesNotRepeatPreviousIntervals(t *testing.T) {
	dir := t.TempDir()
	logger, err := audit.Open(dir, "file-delta")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	first := audit.FSCounts{ReadCount: 2, ReadDuration: 3 * time.Millisecond}
	prev := logFileCounts(logger, audit.FSCounts{}, first)
	prev = logFileCounts(logger, prev, first) // an idle interval emits nothing
	last := audit.FSCounts{ReadCount: 3, ReadDuration: 7 * time.Millisecond, WriteCount: 1, WriteDuration: 2 * time.Millisecond}
	logFileCounts(logger, prev, last)
	events, raw := readFileObservationLog(t, dir)
	if len(events) != 3 || events[0]["count"] != float64(2) || events[1]["count"] != float64(1) || events[1]["duration_ms"] != float64(4) || events[2]["kind"] != "file-write" {
		t.Fatalf("intervals repeated cumulative totals: %s", raw)
	}
}
