package session

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"waldo/internal/profile"
	"waldo/internal/proto"
)

type startupFrame struct {
	kind byte
	data []byte
}

func startupFixture() (*startupStream, *bytes.Buffer, *[]startupFrame) {
	out := new(bytes.Buffer)
	s := newStartupStream(out)
	frames := new([]startupFrame)
	s.attach(func(k byte, p []byte) error { *frames = append(*frames, startupFrame{k, bytes.Clone(p)}); return nil })
	return s, out, frames
}

const startupInit = "{\"type\":\"control_request\",\"request_id\":\"init-1\",\"request\":{\"subtype\":\"initialize\"}}\n"
const startupUser = "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"private prompt\"}}\n"
const startupSuccess = "{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"init-1\",\"response\":{}}}\n"

func TestStartupQueuesPromptsButNotControlPlane(t *testing.T) {
	s, out, frames := startupFixture()
	defer s.Close()
	if !strings.Contains(out.String(), "正在启动") {
		t.Fatal("no immediate notice")
	}
	for _, part := range []string{startupInit[:17], startupInit[17:] + startupUser[:20], startupUser[20:]} {
		if e := s.Input([]byte(part)); e != nil {
			t.Fatal(e)
		}
	}
	if len(*frames) != 1 || string((*frames)[0].data) != startupInit {
		t.Fatal("prompt sent before ready")
	}
	control := "{\"type\":\"control_request\",\"request_id\":\"model\",\"request\":{\"subtype\":\"set_model\"}}\n"
	s.Input([]byte(control))
	s.EOF()
	if len(*frames) != 2 {
		t.Fatal("EOF overtook queued prompt")
	}
	unrelated := strings.Replace(startupSuccess, "init-1", "model", 1)
	if _, e := s.Write([]byte(unrelated)); e != nil {
		t.Fatal(e)
	}
	if s.ready || len(*frames) != 2 {
		t.Fatal("unrelated response released prompt")
	}
	for _, part := range []string{startupSuccess[:30], startupSuccess[30:]} {
		if _, e := s.Write([]byte(part)); e != nil {
			t.Fatal(e)
		}
	}
	if !s.ready || len(*frames) != 4 || string((*frames)[2].data) != startupUser || (*frames)[3].kind != proto.FStdinEOF {
		t.Fatal("prompt/EOF not released exactly once", *frames)
	}
	if !strings.Contains(out.String(), "初始化完成") || strings.Contains(out.String(), "private prompt") {
		t.Fatal("missing ready or prompt leak")
	}
	s.Write([]byte(startupSuccess))
	if len(*frames) != 4 {
		t.Fatal("released twice")
	}
	s.Input([]byte(startupUser))
	if len(*frames) != 5 {
		t.Fatal("subsequent messages not forwarded")
	}
}
func TestStartupFailureDoesNotSendQueuedPrompt(t *testing.T) {
	for _, failure := range []string{"error", "eof"} {
		t.Run(failure, func(t *testing.T) {
			s, out, frames := startupFixture()
			s.Input([]byte(startupInit + startupUser))
			if failure == "error" {
				if _, e := s.Write([]byte(strings.Replace(startupSuccess, "success", "error", 1))); e == nil {
					t.Fatal("initialization error ignored")
				}
			}
			s.Close()
			s.Close()
			if len(*frames) != 1 || s.pendingBytes != 0 || !strings.Contains(out.String(), "启动未完成") {
				t.Fatal("prompt sent or no failure notice")
			}
			if strings.Contains(out.String(), "private prompt") {
				t.Fatal("prompt leaked")
			}
		})
	}
}
func TestStartupInterruptDropsUnsentPrompts(t *testing.T) {
	s, _, frames := startupFixture()
	defer s.Close()
	s.Input([]byte(startupInit + startupUser))
	s.Input([]byte("{\"type\":\"control_request\",\"request_id\":\"stop\",\"request\":{\"subtype\":\"interrupt\"}}\n"))
	s.Write([]byte(startupSuccess))
	if len(*frames) != 2 {
		t.Fatal("canceled prompt released")
	}
}
func TestStartupStreamPreservesNativeOutput(t *testing.T) {
	s, out, _ := startupFixture()
	out.Reset()
	native := []byte("{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"result\"}\npartial")
	for _, p := range native {
		if _, e := s.Write([]byte{p}); e != nil {
			t.Fatal(e)
		}
	}
	s.Close()
	if !bytes.Equal(out.Bytes(), native) {
		t.Fatalf("changed partial/native output: %q", out.String())
	}
}
func TestStartupQueueIsBounded(t *testing.T) {
	s, out, _ := startupFixture()
	defer s.Close()
	s.Input([]byte(startupInit))
	line := `{"type":"user","message":{"content":"` + strings.Repeat("x", startupBufferLimit) + `"}}` + "\n"
	if e := s.Input([]byte(line)); e == nil {
		t.Fatal("unbounded prompt queue")
	}
	if s.pendingBytes != 0 || strings.Contains(out.String(), strings.Repeat("x", 50)) {
		t.Fatal("retained/leaked input")
	}
}
func TestStartupNoticesAreOnlyUIAssistantMessages(t *testing.T) {
	s, out, _ := startupFixture()
	s.Input([]byte(startupInit))
	s.Write([]byte(startupSuccess))
	s.Close()
	dec := json.NewDecoder(out)
	notices := 0
	for {
		var msg map[string]any
		e := dec.Decode(&msg)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if msg["type"] == "assistant" {
			notices++
			if msg["session_id"] != "" || msg["message"].(map[string]any)["model"] != "waldo" {
				t.Fatal("fabricated session/model metadata")
			}
		} else if msg["type"] != "control_response" {
			t.Fatal(msg)
		}
	}
	if notices != 2 {
		t.Fatal(notices)
	}
}
func TestStartupOnlyForPaseoClaudeSDK(t *testing.T) {
	base := Options{Paseo: true, Profile: profile.Profile{Telemetry: "claude"}, Args: []string{"--input-format", "stream-json", "--output-format=stream-json", "--permission-prompt-tool", "stdio"}}
	if !startupEnabled(base) {
		t.Fatal("SDK not detected")
	}
	for _, change := range []func(*Options){func(o *Options) { o.Paseo = false }, func(o *Options) { o.Profile.Telemetry = "codex" }, func(o *Options) { o.Args = []string{"-p", "hello", "--output-format", "stream-json"} }, func(o *Options) { o.Args = append(o.Args, "--output-format", "text") }} {
		o := base
		change(&o)
		if startupEnabled(o) {
			t.Fatal("modified ordinary CLI output")
		}
	}
}
