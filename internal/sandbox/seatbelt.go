package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Seatbelt confines native macOS tools without replacing their filesystem.
type Seatbelt struct{}

func (Seatbelt) Command(s Spec) (*exec.Cmd, error) {
	profile, err := SeatbeltProfile(s)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", profile, s.Exe, "sandbox-serve")
	cmd.Dir, cmd.Env = s.Project, s.Env
	return cmd, nil
}

// SeatbeltProfile grants actual paths only. No application-level path check is
// used as a substitute for the inherited kernel policy.
func SeatbeltProfile(s Spec) (string, error) {
	if s.Project == "" || s.SandboxHome == "" || s.TempDir == "" || s.Exe == "" {
		return "", fmt.Errorf("seatbelt: incomplete spec")
	}
	var b strings.Builder
	b.WriteString(`(version 1)
(deny default)
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow sysctl-read)
(allow file-read-metadata)
(allow file-read* (literal "/"))
(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo") (global-name "com.apple.TrustEvaluationAgent"))
(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx") (literal "/dev/null") (literal "/dev/zero") (literal "/dev/random") (literal "/dev/urandom"))
(allow file-read* file-write* file-ioctl (regex #"^/dev/ttys[0-9]+$"))
(allow ipc-posix-sem)
`)
	paths := func(ops, p string) error {
		if !filepath.IsAbs(p) || filepath.Clean(p) == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("seatbelt: invalid access root %q", p)
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		fmt.Fprintf(&b, "(allow %s (subpath %s))\n", ops, strconv.Quote(p))
		return nil
	}
	for _, p := range append([]string{"/System", "/usr", "/bin", "/sbin", "/private/etc", "/Library/Developer", "/Library/Apple/System/Library", "/Applications/Xcode.app", "/opt/homebrew", "/private/var/db/timezone", "/private/var/db/xcode_select_link", "/Library/Preferences/com.apple.dt.Xcode.plist", "/Library/Preferences/com.apple.dt.CommandLineTools.plist", s.Exe, s.ContextDir}, s.ExposeRO...) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		if err := paths("file-read*", p); err != nil {
			return "", err
		}
	}
	for _, p := range []string{s.Project, s.SandboxHome, s.TempDir} {
		if err := paths("file-read* file-write*", p); err != nil {
			return "", err
		}
	}
	// The executable can itself live in the writable source checkout. Keep it
	// read-only even there, including replacement through parent-directory rename.
	exe, err := filepath.EvalSymlinks(s.Exe)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "(deny file-write* (regex #%s))\n", strconv.Quote("^"+casePattern(exe)+"$"))
	for parent := filepath.Dir(exe); parent != "/" && (parent == s.Project || strings.HasPrefix(parent, filepath.Clean(s.Project)+"/")); parent = filepath.Dir(parent) {
		fmt.Fprintf(&b, "(deny file-write-unlink (regex #%s))\n", strconv.Quote("^"+casePattern(parent)+"$"))
	}
	for _, m := range s.ReadOnlyMounts {
		if m.Source != m.Target {
			return "", fmt.Errorf("seatbelt: skill must be staged at target")
		}
		if err := paths("file-read*", m.Target); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", strconv.Quote(m.Target))
	}
	// Explicit denies also cover names absent at launch. ASCII case variants
	// resolve to the same objects on the usual case-insensitive APFS volumes.
	project, err := filepath.EvalSymlinks(s.Project)
	if err != nil {
		return "", err
	}
	names := append(append([]string(nil), ProtectedNames...), ".git/commondir", ".git/config.worktree")
	if st, err := os.Lstat(filepath.Join(project, ".git")); err != nil || !st.IsDir() {
		names = append(names, ".git")
	}
	for _, name := range names {
		pattern := casePattern(filepath.Join(project, name))
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", strconv.Quote(filepath.Join(project, name)))
		fmt.Fprintf(&b, "(deny file-write* (regex #%s))\n", strconv.Quote("^"+pattern+"(/|$)"))
	}
	for _, p := range []string{project, filepath.Join(project, ".git")} {
		fmt.Fprintf(&b, "(deny file-write-unlink (regex #%s))\n", strconv.Quote("^"+casePattern(p)+"$"))
	}
	if s.ProxyPort <= 0 || s.ProxyPort > 65535 {
		return "", fmt.Errorf("seatbelt: proxy is required")
	}
	for _, p := range append([]int{s.ProxyPort}, s.HostPorts...) {
		if p <= 0 || p > 65535 {
			return "", fmt.Errorf("seatbelt: invalid network port")
		}
		fmt.Fprintf(&b, "(allow network-outbound (remote ip %s))\n", strconv.Quote(fmt.Sprintf("localhost:%d", p)))
	}
	for _, p := range s.ListenPorts {
		if p <= 0 || p > 65535 {
			return "", fmt.Errorf("seatbelt: invalid listen port")
		}
		addr := strconv.Quote(fmt.Sprintf("localhost:%d", p))
		fmt.Fprintf(&b, "(allow network-bind network-inbound (local ip %s))\n", addr)
	}
	return b.String(), nil
}

func casePattern(p string) string {
	var b strings.Builder
	for _, c := range p {
		if c >= 'a' && c <= 'z' {
			fmt.Fprintf(&b, "[%c%c]", c, c-'a'+'A')
		} else if c >= 'A' && c <= 'Z' {
			fmt.Fprintf(&b, "[%c%c]", c, c-'A'+'a')
		} else {
			switch c {
			case '.', '+', '*', '?', '(', ')', '{', '}', '^', '$', '|':
				b.WriteString("[" + string(c) + "]")
			case '[':
				b.WriteString("[[]")
			case ']':
				b.WriteString("[]]")
			default:
				b.WriteRune(c)
			}
		}
	}
	return b.String()
}
