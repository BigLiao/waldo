// Package audit writes the host-side audit log (JSON lines). The log lives
// outside the sandbox, so nothing running on B or in the sandbox can erase it.
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	mu      sync.Mutex
	f       *os.File
	session string
	remote  string
	host    string
}

// OpenRemote attaches the resolved machine to every event, including tool
// timings, so concurrent sessions can be compared without joining session logs.
func OpenRemote(dataDir, session, remote, host string) (*Logger, error) {
	l, err := Open(dataDir, session)
	if err != nil {
		return nil, err
	}
	l.remote, l.host = remote, host
	return l, nil
}

// Path of the audit log in dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, "audit.log") }

// Open opens (creating) the audit log. A nil *Logger is valid and discards.
func Open(dataDir, session string) (*Logger, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(Path(dataDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f, session: session}, nil
}

// Log appends one event.
func (l *Logger) Log(kind string, fields map[string]any) {
	if l == nil {
		return
	}
	ev := map[string]any{"t": time.Now().Format(time.RFC3339Nano), "s": l.session, "kind": kind}
	for k, v := range fields {
		ev[k] = v
	}
	if l.host != "" {
		// Network events already use host for their destination. Keep the
		// session's SSH target in a separate additive field.
		ev["remote_host"] = l.host
	}
	if l.remote != "" {
		ev["remote"] = l.remote
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}

func (l *Logger) Close() {
	if l != nil {
		l.f.Close()
	}
}

// Print writes the last n events of the log in human-readable form.
func Print(w io.Writer, dataDir string, n int, session string) error {
	return PrintFiltered(w, dataDir, n, Filter{Session: session})
}

type Filter struct {
	Session string
	Remote  string
}

// Matches uses only top-level metadata; command contents cannot spoof a match.
func (f Filter) Matches(line string) bool {
	if f.Session == "" && f.Remote == "" {
		return true
	}
	var ev struct {
		Session string `json:"s"`
		Remote  string `json:"remote"`
	}
	return json.Unmarshal([]byte(line), &ev) == nil && (f.Session == "" || ev.Session == f.Session) && (f.Remote == "" || ev.Remote == f.Remote)
}

func PrintFiltered(w io.Writer, dataDir string, n int, filter Filter) error {
	f, err := os.Open(Path(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "（审计日志为空）")
			return nil
		}
		return err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if !filter.Matches(sc.Text()) {
			continue
		}
		lines = append(lines, sc.Text())
		if n > 0 && len(lines) > n*4 {
			lines = lines[len(lines)-n:]
		}
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		fmt.Fprintln(w, Format(l))
	}
	return sc.Err()
}

// Format renders one JSON log line.
func Format(line string) string {
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil {
		return line
	}
	t, _ := ev["t"].(string)
	if tt, err := time.Parse(time.RFC3339Nano, t); err == nil {
		t = tt.Local().Format("01-02 15:04:05")
	}
	s, _ := ev["s"].(string)
	kind, _ := ev["kind"].(string)
	delete(ev, "t")
	delete(ev, "s")
	delete(ev, "kind")
	remote, _ := ev["remote"].(string)
	delete(ev, "remote")
	var detail string
	switch kind {
	case "exec":
		argv, _ := json.Marshal(ev["argv"])
		a := string(argv)
		if r := []rune(a); len(r) > 240 {
			a = string(r[:240]) + "…"
		}
		detail = fmt.Sprintf("%s  (cwd %v)", a, ev["cwd"])
		if tool, ok := ev["tool"].(string); ok && tool != "" {
			detail = "tool=" + tool + " " + detail
		}
	case "write", "remove", "rename", "mkdir", "rmdir", "symlink", "link", "setstat":
		detail = fmt.Sprint(ev["path"])
		if to, ok := ev["to"]; ok {
			detail += " -> " + fmt.Sprint(to)
		}
	default:
		keys := make([]string, 0, len(ev))
		for k := range ev {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, ev[k]))
		}
		detail = strings.Join(parts, " ")
	}
	if remote != "" {
		detail = "remote=" + remote + " " + detail
	}
	return fmt.Sprintf("%s %s %-8s %s", t, s, kind, detail)
}
