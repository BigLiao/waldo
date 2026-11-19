package wsl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBridgeTransfersBinaryAndTrailingResponse(t *testing.T) {
	done := make(chan struct{})
	conn, err := startBridge(context.Background(), exec.Command("sh", "-c", "printf '\\001'; cat; printf tail"), func() { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := []byte{0x00, 0xff, '\n', 'x'}
	if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(out, append(payload, "tail"...)) {
		t.Fatalf("read from bridge: %q, %v", out, err)
	}
	conn.Close()
	<-done
}

func TestBridgeRejectsMissingAcknowledgement(t *testing.T) {
	done := make(chan struct{})
	conn, err := startBridge(context.Background(), exec.Command("sh", "-c", "exit 1"), func() { close(done) })
	if err == nil {
		conn.Close()
		t.Fatal("bridge started without a successful connection")
	}
	<-done
}

func TestBridgeCancellationClosesPipesAndReleasesSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	conn, err := startBridge(ctx, exec.Command("sh", "-c", "printf '\\001'; exec cat"), func() { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled bridge kept its process or connection slot")
	}
}

// Accept connections without reading, responding or closing them. This
// reproduces a Windows service that leaves a half-closed socket open.
const holdWindowsConnections = `
$ErrorActionPreference = 'Stop'
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$connections = [Collections.Generic.List[Net.Sockets.TcpClient]]::new()
try {
    $listener.Start()
    $output = [Console]::OpenStandardOutput()
    $output.WriteByte(1)
    $port = [Text.Encoding]::ASCII.GetBytes(([string]$listener.LocalEndpoint.Port) + [char]10)
    $output.Write($port, 0, $port.Length)
    $output.Flush()
    $stopping = [Console]::OpenStandardInput().ReadAsync([byte[]]::new(1), 0, 1)
    while (!$stopping.IsCompleted) {
        $accepting = $listener.AcceptTcpClientAsync()
        $first = [Threading.Tasks.Task]::WhenAny($stopping, $accepting).GetAwaiter().GetResult()
        if ([object]::ReferenceEquals($first, $stopping)) { break }
        $connections.Add($accepting.GetAwaiter().GetResult())
    }
} finally {
    $listener.Stop()
    foreach ($connection in $connections) { $connection.Dispose() }
}
`

func TestWindowsBridgeReleasesHalfClosedSlots(t *testing.T) {
	bridge, err := NewPortBridge()
	if err != nil {
		t.Skipf("Windows interop not available: %v", err)
	}
	defer bridge.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.Command(bridge.powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(holdWindowsConnections))
	fixture, err := startBridge(ctx, cmd, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	if err := fixture.output.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(fixture).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}

	var connections []*bridgeConn
	for range bridgeConnectionLimit {
		c, err := bridge.Dial(port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		connections = append(connections, c.(*bridgeConn))
	}
	if c, err := bridge.Dial(port); err == nil {
		c.Close()
		t.Fatal("bridge accepted a connection beyond the session limit")
	}
	for _, c := range connections {
		if err := c.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(bridgeHalfCloseIdle + 4*time.Second)
	for _, c := range connections {
		if err := c.output.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		var buffer [1]byte
		if n, err := c.Read(buffer[:]); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("half-closed bridge kept waiting: %d bytes, %v", n, err)
		}
		c.Close()
	}
	c, err := bridge.Dial(port)
	if err != nil {
		t.Fatalf("no slot available after half-close timeout: %v", err)
	}
	defer c.Close()
	bridge.Close()
	if bridge.active != 0 {
		t.Fatal("bridge shutdown left a worker active")
	}
}
