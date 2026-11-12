package transport

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"waldo/internal/proto"
	"waldo/internal/reconnect"
	"waldo/internal/termfilter"
	"waldo/internal/util"
)

// ReconnectMessage is the user-facing notice for a link state change.
func ReconnectMessage(up bool) string {
	if up {
		return "连接已恢复，继续原会话"
	}
	return "连接中断，正在尝试重连（最多 90 秒，Ctrl+C 取消）"
}

func (c *Conn) ReconnectExpired() bool {
	link, ok := c.RW.(*reconnect.Stream)
	return ok && errors.Is(link.Err(), reconnect.ErrTimeout)
}

// OpenResumable starts one independent host and only reattaches to that host
// on retry. The same yamux session remains above the reliable byte stream.
func (s *SSH) OpenResumable() (*Conn, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(token)
	first, err := s.Open([]string{"--reconnect-attach", id, "--reconnect-start"})
	if err != nil {
		return nil, err
	}
	link := reconnect.New(reconnect.Window)
	ready := make(chan struct{})
	var initial sync.Once
	var attached bool // State callbacks are serialized by Stream.Serve.
	ctx, cancel := context.WithCancel(context.Background())
	conn := &Conn{RW: link, closeFunc: func() { link.Close(); cancel() }, reconnecting: func() bool { return !link.Connected() }}
	status := func(up bool) {
		if f := conn.notice.Load(); f != nil {
			(*f)(up)
			return
		}
		fmt.Fprintf(os.Stderr, "\r\nwaldo: %s\r\n", ReconnectMessage(up))
	}
	link.State = func(up bool) {
		if up {
			if attached {
				status(true)
			}
			attached = true
			initial.Do(func() { close(ready) })
		} else if link.Err() == nil {
			status(false)
		}
	}
	go func() { <-link.Done(); cancel() }()
	go func() {
		physical := first
		delay := time.Second
		for {
			link.Serve(&physicalStream{Conn: physical})
			physical.Close()
			if link.Err() != nil {
				return
			}
			for {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				physical, err = s.reattach(ctx, id)
				if err == nil {
					delay = time.Second
					break
				}
				delay = min(5*time.Second, delay*2)
			}
		}
	}()
	select {
	case <-ready:
		return conn, nil
	case <-link.Done():
		link.Close()
		return nil, link.Err()
	}
}

// physicalStream closes both SSH pipes by terminating/reaping the process.
type physicalStream struct{ *Conn }

func (p *physicalStream) Read(b []byte) (int, error)  { return p.RW.Read(b) }
func (p *physicalStream) Write(b []byte) (int, error) { return p.RW.Write(b) }
func (p *physicalStream) Close() error                { p.Conn.Close(); return nil }

// Retry authentication is noninteractive and each attempt has a deadline.
// A missing/expired session is never replaced with a fresh agent.
func (s *SSH) reattach(parent context.Context, token string) (*Conn, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	copySSH := *s
	copySSH.ExtraArgs = append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1"}, s.ExtraArgs...)
	cmd := copySSH.command(copySSH.bootstrap("--reconnect-attach " + util.ShellQuote(token)))
	// A failed attempt may be noisy; the main session reports reconnect status.
	cmd.Stderr = termfilter.New(io.Discard)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	conn := &Conn{RW: util.RWC{R: stdout, W: stdin, C: []io.Closer{stdin}}, cmd: cmd}
	read := make(chan struct {
		line string
		err  error
	}, 1)
	br := bufio.NewReader(stdout)
	go func() {
		line, err := readLine(br)
		read <- struct {
			line string
			err  error
		}{line, err}
	}()
	select {
	case result := <-read:
		if result.err != nil || result.line != proto.BannerPrefix+proto.FullVersion() {
			conn.Close()
			return nil, errors.New("original remote session unavailable")
		}
		conn.RW = util.RWC{R: br, W: stdin, C: []io.Closer{stdin}}
		return conn, nil
	case <-ctx.Done():
		conn.Close()
		return nil, ctx.Err()
	}
}
