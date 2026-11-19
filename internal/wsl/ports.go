package wsl

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

const (
	bridgeStartupTimeout  = 10 * time.Second
	bridgeHalfCloseIdle   = 5 * time.Second
	bridgeCloseGrace      = bridgeHalfCloseIdle + time.Second
	bridgeConnectionLimit = 8
)

const interopUnavailable = "WSL 互操作不可用（确认已启用 interop，并完整重启 WSL）"

// The script runs on Windows, where Loopback is the Windows host. The pipe
// content is raw TCP payload, never evaluated as a PowerShell command.
const windowsPortScript = `
$ErrorActionPreference = 'Stop'
$halfCloseIdle = %d
function Wait-BridgeIO([Threading.Tasks.Task] $operation) {
    if (!$drain.Active) {
        $first = [Threading.Tasks.Task]::WhenAny($sending, $operation).GetAwaiter().GetResult()
        if ([object]::ReferenceEquals($first, $sending)) {
            [void]$sending.GetAwaiter().GetResult()
            $tcp.Client.Shutdown([Net.Sockets.SocketShutdown]::Send)
            $drain.Active = $true
        }
    }
    return (!$drain.Active -or $operation.Wait($halfCloseIdle))
}

$tcp = [Net.Sockets.TcpClient]::new()
try {
    $connect = $tcp.ConnectAsync([Net.IPAddress]::Loopback, %d)
    if (!$connect.Wait(5000)) { throw 'Connection timed out' }
    $inputPipe = [Console]::OpenStandardInput()
    $outputPipe = [Console]::OpenStandardOutput()
    $network = $tcp.GetStream()
    $outputPipe.WriteByte(1)
    $outputPipe.Flush()
    $sending = $inputPipe.CopyToAsync($network)
    $drain = @{ Active = $false }
    $buffer = [byte[]]::new(65536)
    while ($true) {
        $reading = $network.ReadAsync($buffer, 0, $buffer.Length)
        if (!(Wait-BridgeIO $reading)) { break }
        $count = $reading.GetAwaiter().GetResult()
        if ($count -eq 0) { break }
        $writing = $outputPipe.WriteAsync($buffer, 0, $count)
        if (!(Wait-BridgeIO $writing)) { break }
        [void]$writing.GetAwaiter().GetResult()
    }
} finally {
    $tcp.Dispose()
}
`

// PortBridge opens Windows TCP loopback sockets via WSL process interop.
type PortBridge struct {
	ctx        context.Context
	cancel     context.CancelFunc
	powerShell string

	mu      sync.Mutex
	active  int
	pending sync.WaitGroup
}

func NewPortBridge() (*PortBridge, error) {
	if !IsWSL2() {
		return nil, errors.New("net.windows_ports 需要 WSL2")
	}
	registration, err := os.ReadFile("/proc/sys/fs/binfmt_misc/WSLInterop")
	if err != nil || !strings.HasPrefix(string(registration), "enabled\n") {
		return nil, errors.New(interopUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Resolve the Windows system executable independently of the project's PATH.
	out, err := exec.CommandContext(ctx, "/usr/bin/wslpath", "-u", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`).Output()
	if err != nil {
		return nil, fmt.Errorf("定位 Windows PowerShell: %w", err)
	}
	path := strings.TrimSpace(string(out))
	st, err := os.Stat(path)
	if err != nil || !filepath.IsAbs(path) || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("找不到可执行的 Windows PowerShell: %q", path)
	}
	lifetime, stop := context.WithCancel(context.Background())
	return &PortBridge{ctx: lifetime, cancel: stop, powerShell: path}, nil
}

func (p *PortBridge) Close() {
	// Stop accepting new workers before waiting for the existing ones.
	p.mu.Lock()
	p.cancel()
	p.mu.Unlock()
	p.pending.Wait()
}

func (p *PortBridge) reserve() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.active >= bridgeConnectionLimit {
		return errors.New("Windows loopback: too many connections")
	}
	p.active++
	p.pending.Add(1)
	return nil
}

func (p *PortBridge) release() {
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	p.pending.Done()
}

func (p *PortBridge) Dial(port int) (io.ReadWriteCloser, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid Windows loopback port %d", port)
	}
	if err := p.reserve(); err != nil {
		return nil, err
	}
	script := fmt.Sprintf(windowsPortScript, bridgeHalfCloseIdle.Milliseconds(), port)
	cmd := exec.Command(p.powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(script))
	cmd.Dir = filepath.Dir(p.powerShell)
	conn, err := startBridge(p.ctx, cmd, p.release)
	if err != nil {
		return nil, fmt.Errorf("Windows 127.0.0.1:%d: %w", port, err)
	}
	return conn, nil
}

func encodedCommand(script string) string {
	var encoded []byte
	for _, code := range utf16.Encode([]rune(script)) {
		encoded = binary.LittleEndian.AppendUint16(encoded, code)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

type bridgeConn struct {
	input  *os.File
	output *os.File
	close  func()
}

func (c *bridgeConn) Read(b []byte) (int, error)  { return c.output.Read(b) }
func (c *bridgeConn) Write(b []byte) (int, error) { return c.input.Write(b) }
func (c *bridgeConn) CloseWrite() error           { return c.input.Close() }
func (c *bridgeConn) Close() error {
	c.close()
	return nil
}

func startBridge(ctx context.Context, cmd *exec.Cmd, release func()) (*bridgeConn, error) {
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	input, inputWriter, err := os.Pipe()
	if err != nil {
		release()
		return nil, err
	}
	defer input.Close()
	outputReader, output, err := os.Pipe()
	if err != nil {
		inputWriter.Close()
		release()
		return nil, err
	}
	defer output.Close()
	cmd.Stdin, cmd.Stdout = input, output
	if err := cmd.Start(); err != nil {
		inputWriter.Close()
		outputReader.Close()
		release()
		if errors.Is(err, syscall.ENOEXEC) {
			return nil, fmt.Errorf("%s: %w", interopUnavailable, err)
		}
		return nil, fmt.Errorf("启动 Windows 桥接（检查 WSL interop）: %w", err)
	}
	// Keep ownership of the parent-side pipes so Wait cannot discard the
	// final output before the caller has drained it.
	input.Close()
	output.Close()
	done := make(chan struct{})
	conn := &bridgeConn{output: outputReader, input: inputWriter, close: sync.OnceFunc(func() {
		inputWriter.Close()
		outputReader.Close()
		select {
		case <-done:
		case <-time.After(bridgeCloseGrace):
			cmd.Process.Kill()
			<-done
		}
	})}
	stopWatching := context.AfterFunc(ctx, func() { conn.Close() })
	go func() {
		cmd.Wait()
		release()
		close(done)
		stopWatching()
	}()
	if err := outputReader.SetReadDeadline(time.Now().Add(bridgeStartupTimeout)); err != nil {
		conn.Close()
		return nil, err
	}
	var ready [1]byte
	if _, err := io.ReadFull(outputReader, ready[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("Windows 桥接未就绪（检查服务监听和 WSL interop）: %w", err)
	}
	if ready[0] != 1 {
		conn.Close()
		return nil, fmt.Errorf("Windows 桥接返回了无效的响应")
	}
	if err := outputReader.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
