package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"waldo/internal/proto"
)

const startupBufferLimit = 8 << 20

// startupStream adapts only Paseo's Claude SDK JSON-lines transport. Notices
// are UI-only assistant text, clearly attributed to Waldo; they are never sent
// to Claude or represented as native init/result/model-usage events.
// Input control messages bypass the prompt queue to avoid an initialize deadlock.
type startupStream struct {
	mu                         sync.Mutex
	output                     io.Writer
	send                       func(byte, []byte) error
	started                    time.Time
	initID                     string
	ready, failed, closed, eof bool
	input, outputLine          []byte
	pending                    [][]byte
	pendingBytes               int
	rawOutput                  bool
}

func startupEnabled(o Options) bool {
	if !o.Paseo || o.Profile.Telemetry != "claude" {
		return false
	}
	args := append(append([]string(nil), o.Profile.Args...), o.Args...)
	value := func(flag string) string {
		v := ""
		for i, a := range args {
			if a == "--" {
				break
			}
			if a == flag && i+1 < len(args) {
				v = args[i+1]
			} else if s, ok := strings.CutPrefix(a, flag+"="); ok {
				v = s
			}
		}
		return v
	}
	return value("--input-format") == "stream-json" && value("--output-format") == "stream-json" && value("--permission-prompt-tool") == "stdio"
}
func newStartupStream(out io.Writer) *startupStream {
	s := &startupStream{output: out, started: time.Now()}
	s.notice("Waldo：正在启动，连接远端并初始化 Claude… 首条消息会在初始化完成后发送。")
	return s
}
func (s *startupStream) notice(text string) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	uid := fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(id[:4]), hex.EncodeToString(id[4:6]), hex.EncodeToString(id[6:8]), hex.EncodeToString(id[8:10]), hex.EncodeToString(id[10:]))
	data, err := json.Marshal(map[string]any{"type": "assistant", "uuid": uid, "session_id": "", "parent_tool_use_id": nil, "message": map[string]any{"id": "waldo-startup-" + uid, "type": "message", "role": "assistant", "model": "waldo", "content": []any{map[string]string{"type": "text", "text": text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}})
	if err != nil {
		return err
	}
	_, err = s.output.Write(append(data, '\n'))
	return err
}
func (s *startupStream) attach(send func(byte, []byte) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.send = send
}
func (s *startupStream) fail(err error) error {
	s.failed = true
	s.pending = nil
	s.pendingBytes = 0
	return err
}
func (s *startupStream) Input(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return io.ErrClosedPipe
	}
	if s.ready {
		return s.send(proto.FStdin, p)
	}
	s.input = append(s.input, p...)
	for {
		n := bytes.IndexByte(s.input, '\n')
		if n < 0 {
			break
		}
		line := s.input[:n+1]
		if len(line) > startupBufferLimit {
			return s.fail(fmt.Errorf("Claude input line exceeds 8 MiB"))
		}
		if err := s.inputLine(line); err != nil {
			return s.fail(err)
		}
		s.input = s.input[n+1:]
	}
	if len(s.input) > startupBufferLimit {
		return s.fail(fmt.Errorf("Claude input line exceeds 8 MiB"))
	}
	// Do not retain a large backing buffer containing prompt text.
	if len(s.input) == 0 {
		s.input = nil
	}
	return nil
}
func (s *startupStream) inputLine(line []byte) error {
	var msg struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(line, &msg) == nil {
		if msg.Type == "control_request" && msg.Request.Subtype == "initialize" && !s.ready {
			s.initID = msg.RequestID
		}
		if msg.Type == "control_request" && msg.Request.Subtype == "interrupt" && !s.ready {
			s.pending = nil
			s.pendingBytes = 0
		}
		if msg.Type == "user" && !s.ready {
			if s.pendingBytes+len(line) > startupBufferLimit {
				return fmt.Errorf("Claude startup prompt queue exceeds 8 MiB")
			}
			s.pending = append(s.pending, bytes.Clone(line))
			s.pendingBytes += len(line)
			return nil
		}
	}
	return s.send(proto.FStdin, line)
}
func (s *startupStream) EOF() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return io.ErrClosedPipe
	}
	if len(s.input) > 0 {
		if err := s.inputLine(s.input); err != nil {
			return s.fail(err)
		}
		s.input = nil
	}
	s.eof = true
	if len(s.pending) == 0 {
		return s.send(proto.FStdinEOF, nil)
	}
	return nil
}
func (s *startupStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	n := len(p)
	for len(p) > 0 {
		if s.ready {
			_, err := s.output.Write(p)
			return n, err
		}
		if s.rawOutput {
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				_, err := s.output.Write(p)
				return n, err
			}
			if _, err := s.output.Write(p[:i+1]); err != nil {
				return 0, err
			}
			p = p[i+1:]
			s.rawOutput = false
			continue
		}
		i := bytes.IndexByte(p, '\n')
		take := len(p)
		if i >= 0 {
			take = i + 1
		}
		if len(s.outputLine)+take > startupBufferLimit {
			if _, err := s.output.Write(s.outputLine); err != nil {
				return 0, err
			}
			s.outputLine = nil
			s.rawOutput = true
			continue
		}
		s.outputLine = append(s.outputLine, p[:take]...)
		p = p[take:]
		if i < 0 {
			continue
		}
		line := s.outputLine
		s.outputLine = nil
		if _, err := s.output.Write(line); err != nil {
			return 0, err
		}
		if err := s.observeOutput(line); err != nil {
			return 0, s.fail(err)
		}
	}
	return n, nil
}
func (s *startupStream) observeOutput(line []byte) error {
	if s.ready || s.failed || s.initID == "" {
		return nil
	}
	var msg struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != s.initID {
		return nil
	}
	if msg.Response.Subtype != "success" {
		return s.fail(fmt.Errorf("Claude initialization failed"))
	}
	s.ready = true
	if err := s.notice(fmt.Sprintf("Waldo：初始化完成（%.1f 秒），可以开始处理消息。", time.Since(s.started).Seconds())); err != nil {
		return err
	}
	for _, p := range s.pending {
		if err := s.send(proto.FStdin, p); err != nil {
			return err
		}
	}
	s.pending = nil
	s.pendingBytes = 0
	if s.eof {
		return s.send(proto.FStdinEOF, nil)
	}
	return nil
}
func (s *startupStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	// Preserve partial stdout, including on early exit, without interleaving JSON.
	if len(s.outputLine) > 0 {
		s.output.Write(s.outputLine)
		s.outputLine = nil
		s.rawOutput = true
	}
	if !s.ready && !s.rawOutput {
		s.notice("Waldo：启动未完成，会话已结束。请查看连接或 Claude 的错误信息。")
	}
	s.pending = nil
	s.input = nil
	s.pendingBytes = 0
}
