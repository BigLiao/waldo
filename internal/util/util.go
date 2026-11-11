// Package util holds small helpers shared by several roles.
package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// YamuxConfig is the multiplexer configuration used on every link.
func YamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.MaxStreamWindowSize = 1 << 20
	c.AcceptBacklog = 1024
	return c
}

// RWC glues a reader and a writer into an io.ReadWriteCloser.
type RWC struct {
	R io.Reader
	W io.Writer
	C []io.Closer
}

func (r RWC) Read(p []byte) (int, error)  { return r.R.Read(p) }
func (r RWC) Write(p []byte) (int, error) { return r.W.Write(p) }
func (r RWC) Close() error {
	for _, c := range r.C {
		c.Close()
	}
	return nil
}

type closeWriter interface{ CloseWrite() error }

func halfClose(c io.Closer) {
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// Splice copies in both directions, propagating EOF as a half-close where the
// transport supports it, and closes both ends once both directions are done.
func Splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(b, a); halfClose(b) }()
	go func() { defer wg.Done(); io.Copy(a, b); halfClose(a) }()
	wg.Wait()
	a.Close()
	b.Close()
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExpandHome replaces a leading "~" with home.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Exists reports whether p exists (without following a final symlink).
func Exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// IsDir reports whether p is a directory (following symlinks).
func IsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Within reports whether p equals dir or is below it.
func Within(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// EnvGet returns the value of key in a KEY=VALUE list.
func EnvGet(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// EnvSet replaces or appends key in a KEY=VALUE list.
func EnvSet(env []string, key, val string) []string {
	out := env[:0:0]
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k != key {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}

// EnvFilter returns env without the keys for which drop returns true.
func EnvFilter(env []string, drop func(key string) bool) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if !drop(k) {
			out = append(out, e)
		}
	}
	return out
}

// IsProxyVar reports whether key is one of the proxy environment variables.
func IsProxyVar(key string) bool {
	switch strings.ToUpper(key) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "FTP_PROXY":
		return true
	}
	return false
}
