package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"waldo/internal/config"
)

type remoteBinding struct {
	Remote    string `json:"remote,omitempty"`
	Host      string `json:"host"`
	RemoteDir string `json:"remote_dir"`
	Transport string `json:"transport_sha256"`
}

type sessionRoute struct {
	dir     string
	binding remoteBinding
}

func newSessionRoute(cfg *config.Config, remote string) *sessionRoute {
	tr := SSHTransport(cfg, cfg.Host)
	// Include effective SSH options without persisting their possibly sensitive
	// values. SSH alias resolution remains the responsibility of OpenSSH.
	b, _ := json.Marshal([]any{tr.Host, tr.ExtraArgs, cfg.RemoteDirOrDefault()})
	h := sha256.Sum256(b)
	return &sessionRoute{
		dir: filepath.Join(config.DataDir(), "session-routes"),
		binding: remoteBinding{Remote: remote, Host: cfg.Host,
			RemoteDir: cfg.RemoteDirOrDefault(), Transport: hex.EncodeToString(h[:])},
	}
}

func (r *sessionRoute) bindingPath(identity string) string {
	h := sha256.Sum256([]byte(identity))
	return filepath.Join(r.dir, hex.EncodeToString(h[:])+".json")
}

func (r *sessionRoute) check(identity string) error {
	b, err := os.ReadFile(r.bindingPath(identity))
	if err != nil {
		return err
	}
	var old remoteBinding
	if json.Unmarshal(b, &old) != nil || old.Transport == "" {
		return fmt.Errorf("会话远端绑定记录损坏，拒绝启动")
	}
	if old.Transport != r.binding.Transport {
		return fmt.Errorf("会话已绑定远端 %q（%s），当前目标或 SSH/remote_dir 配置不同；请使用原远端配置恢复，或新建会话", old.Remote, old.Host)
	}
	return nil
}

// bind is immutable per identity. Publish a complete file with link(2) so
// concurrent launches cannot overwrite another machine's binding.
func (r *sessionRoute) bind(identity string) error {
	if err := r.check(identity); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(r.dir, ".route-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	data, _ := json.Marshal(r.binding)
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(f.Name(), r.bindingPath(identity)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return r.check(identity)
		}
		return err
	}
	return nil
}

// Check an existing Paseo identity before claiming a fresh native ID. A launch
// rejected for a changed remote must not leave that ID bound to the wrong B.
func (r *sessionRoute) prepare(paseoIdentity string, args []string) error {
	if paseoIdentity != "" {
		if err := r.check(paseoIdentity); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := r.checkArgs(args); err != nil {
		return err
	}
	if paseoIdentity != "" {
		return r.bind(paseoIdentity)
	}
	return nil
}

var nativeSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Explicit native IDs can be checked before connecting. --continue and named
// session selectors remain scoped to the chosen machine; we never guess a host.
func (r *sessionRoute) checkArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "-p" || a == "--print" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // The optional prompt is not a flag.
			}
			continue
		}
		flag, value, inline := strings.Cut(a, "=")
		if flag != "--resume" && flag != "-r" && flag != "--session-id" {
			continue
		}
		if !inline && i+1 < len(args) {
			i++
			value = args[i]
		}
		if nativeSessionID.MatchString(value) {
			if err := r.bind("claude:" + strings.ToLower(value)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *sessionRoute) observer(args []string) *routeObserver {
	if r == nil {
		return nil
	}
	format := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if args[i] == "--output-format" && i+1 < len(args) {
			i++
			format = args[i]
		} else if value, ok := strings.CutPrefix(args[i], "--output-format="); ok {
			format = value
		}
	}
	if format != "stream-json" {
		return nil
	}
	return &routeObserver{route: r}
}

// Observe only native session metadata in stream-json, preserving all output
// bytes in the relay. No prompts, tokens, or tool contents are saved here.
type routeObserver struct {
	route *sessionRoute
	line  []byte
	skip  bool
	done  bool
}

func (o *routeObserver) observe(p []byte) error {
	if o.done {
		return nil
	}
	const maxLine = 1 << 20
	for len(p) > 0 {
		n := bytes.IndexByte(p, '\n')
		take := len(p)
		if n >= 0 {
			take = n + 1
		}
		if len(o.line)+take > maxLine {
			o.skip = true
			o.line = nil
		}
		if !o.skip {
			o.line = append(o.line, p[:take]...)
		}
		p = p[take:]
		if n < 0 {
			continue
		}
		if !o.skip {
			var msg struct {
				Type      string `json:"type"`
				Subtype   string `json:"subtype"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(o.line, &msg) == nil && (msg.Type == "system" && msg.Subtype == "init" || msg.Type == "result") && nativeSessionID.MatchString(msg.SessionID) {
				id := strings.ToLower(msg.SessionID)
				if err := o.route.bind("claude:" + id); err != nil {
					return err
				}
				// One native process owns one top-level session. Once recorded,
				// leave subsequent model/tool output entirely on the fast path.
				o.done = true
				o.line = nil
				return nil
			}
		}
		o.line = nil
		o.skip = false
	}
	return nil
}
