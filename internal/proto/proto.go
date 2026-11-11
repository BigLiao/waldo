// Package proto defines the wire format shared by all waldo roles.
//
// Every yamux stream starts with one length-prefixed JSON Header that says
// what the stream is for. exec and io streams then carry Frames; fs streams
// carry raw SFTP; net streams carry a Reply followed by raw bytes.
package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Version is the human-readable release version, overridden from the release tag.
var Version = "0.1.0"

// Build may be set with -ldflags "-X waldo/internal/proto.Build=..." for
// release builds (so that binaries of different architectures share an id).
// When empty, a hash of the running executable is used, which makes every dev
// rebuild trigger a reinstall on B.
var Build string

var (
	fullOnce sync.Once
	full     string
)

// FullVersion is the string both ends must agree on exactly.
func FullVersion() string {
	fullOnce.Do(func() {
		b := Build
		if b == "" {
			b = "dev"
			if exe, err := os.Executable(); err == nil {
				if f, err := os.Open(exe); err == nil {
					h := sha256.New()
					if _, err := io.Copy(h, f); err == nil {
						b = hex.EncodeToString(h.Sum(nil))[:12]
					}
					f.Close()
				}
			}
		}
		full = Version + "+" + b
	})
	return full
}

// Banner is the first line `waldo host` writes on stdout before yamux starts.
const BannerPrefix = "WALDO-HOST "

// NeedInstall is printed by the remote bootstrap script when the binary on B
// is missing or has the wrong version.
const NeedInstall = "WALDO-NEED-INSTALL"

// ContextDir is projected to B and visible to commands executed on A as well.
const ContextDir = "/run/waldo/context"
const ContextFile = ContextDir + "/waldo.md"

// Stream kinds.
const (
	KindCtl      = "ctl"      // A client -> B host: Hello / HelloAck
	KindIO       = "io"       // A client -> B host: agent stdio (frames)
	KindFS       = "fs"       // B host -> A sandbox: SFTP
	KindExec     = "exec"     // B host -> A sandbox: one forwarded process (frames)
	KindSandbox  = "sandbox"  // A client -> A sandbox: config, facts, port events
	KindConnect  = "connect"  // A sandbox -> A client: outbound proxy connection
	KindHostPort = "hostport" // A sandbox -> A client: connection to a host service
	KindDial     = "dial"     // A client -> A sandbox: forwarded localhost port
)

type Header struct {
	Kind string   `json:"kind"`
	Path string   `json:"path,omitempty"` // fs: root path being mounted
	Exec *ExecReq `json:"exec,omitempty"`
	Host string   `json:"host,omitempty"` // connect
	Port int      `json:"port,omitempty"` // connect, hostport, dial
}

// ExecReq describes one process start that a stub forwards to A.
type ExecReq struct {
	Path string   `json:"path"` // absolute path of the program to execute
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
	TTY  bool     `json:"tty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// Reply answers a connect/hostport/dial request before raw bytes flow.
type Reply struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg,omitempty"`
}

// PathDir is one directory of A's PATH as seen inside the A sandbox.
type PathDir struct {
	Dir    string   `json:"dir"`
	Real   string   `json:"real"`
	Names  []string `json:"names"`
	OnPath bool     `json:"on_path"`
}

// Facts is what the A sandbox reports about itself.
type Facts struct {
	Home     string    `json:"home"`
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	UID      int       `json:"uid"`
	GID      int       `json:"gid"`
	Env      []string  `json:"env"`
	PathDirs []PathDir `json:"path_dirs"`
}

// SandboxConfig is what the A client tells sandbox-serve at startup.
type SandboxConfig struct {
	BaseEnv          []string `json:"base_env"`
	ExecEnv          []string `json:"exec_env"` // forced onto every forwarded exec (proxy vars)
	ProxyPort        int      `json:"proxy_port"`
	HostPorts        []int    `json:"host_ports"`
	Mkdirs           []string `json:"mkdirs"`
	ContextProject   string   `json:"context_project,omitempty"`
	InstructionFiles []string `json:"instruction_files,omitempty"`
}

// PortEvent carries the full current set of listening ports in the sandbox.
type PortEvent struct {
	Ports []int `json:"ports"`
}

// Profile is the agent profile as sent to B.
type Profile struct {
	Name   string            `json:"name"`
	Bin    string            `json:"bin"`
	Args   []string          `json:"args"`
	Env    map[string]string `json:"env"`
	Shared []string          `json:"shared"`
	Scrub  []string          `json:"scrub"`
	State  []string          `json:"state"`
}

// Hello is the session description A sends to B.
type Hello struct {
	Version                 string   `json:"version"`
	Session                 string   `json:"session"`
	Profile                 Profile  `json:"profile"`
	Argv                    []string `json:"argv"`
	Cwd                     string   `json:"cwd"`
	Project                 string   `json:"project"`
	Mounts                  []string `json:"mounts"` // absolute paths to project from A (project first)
	Facts                   Facts    `json:"facts"`
	Interactive             bool     `json:"interactive"`
	Rows                    uint16   `json:"rows"`
	Cols                    uint16   `json:"cols"`
	TimeUnixMs              int64    `json:"time_unix_ms"`
	MetadataCacheTTLSeconds int64    `json:"metadata_cache_ttl_seconds"`
}

type HelloAck struct {
	Version string `json:"version"`
	SkewMs  int64  `json:"skew_ms"`
	Error   string `json:"error,omitempty"`
}

// Exit is the payload of an FExit frame.
type Exit struct {
	Code   int `json:"code"`
	Signal int `json:"signal,omitempty"`
}

// Resize is the payload of an FResize frame.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

const maxMsg = 8 << 20

// WriteMsg writes a uint32 big-endian length followed by JSON.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadMsg reads one message written by WriteMsg.
func ReadMsg(r io.Reader, v any) error {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(lb[:])
	if n > maxMsg {
		return fmt.Errorf("message too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Frame types for exec and io streams.
const (
	FStdin    byte = 1
	FStdinEOF byte = 2
	FStdout   byte = 3
	FStderr   byte = 4
	FSignal   byte = 5 // payload: 1 byte signal number
	FResize   byte = 6 // payload: JSON Resize
	FExit     byte = 7 // payload: JSON Exit
	FError    byte = 8 // payload: message for the user's stderr
)

const maxFrame = 1 << 20

// FrameWriter serialises frames from several goroutines.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (f *FrameWriter) Write(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		chunk := p
		if len(chunk) > maxFrame {
			chunk = p[:maxFrame]
		}
		var hdr [5]byte
		hdr[0] = typ
		binary.BigEndian.PutUint32(hdr[1:], uint32(len(chunk)))
		if _, err := f.w.Write(append(hdr[:], chunk...)); err != nil {
			return err
		}
		p = p[len(chunk):]
		if len(p) == 0 {
			return nil
		}
	}
}

func (f *FrameWriter) WriteJSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.Write(typ, b)
}

// Writer returns an io.Writer that emits frames of the given type.
func (f *FrameWriter) Writer(typ byte) io.Writer { return frameTypeWriter{f, typ} }

type frameTypeWriter struct {
	f   *FrameWriter
	typ byte
}

func (w frameTypeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.f.Write(w.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

var ErrFrameTooLarge = errors.New("frame too large")

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return hdr[0], b, nil
}

// DecodeJSON is a small helper for frame payloads.
func DecodeJSON(b []byte, v any) error { return json.Unmarshal(b, v) }
