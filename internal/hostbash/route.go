package hostbash

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type mountRoute struct{ path, side string }
type pathRouter struct {
	mounts []mountRoute
	cwd    string
}

func parseMounts(r io.Reader, cwd string) (*pathRouter, error) {
	out := &pathRouter{cwd: cwd}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 1<<20)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scan.Scan() {
		parts := strings.SplitN(scan.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 2 {
			continue
		}
		side := "agent"
		if right[0] == "fuse.sshfs" && strings.HasPrefix(unescape.Replace(right[1]), "waldo:") {
			side = "host"
		}
		out.mounts = append(out.mounts, mountRoute{filepath.Clean(unescape.Replace(left[4])), side})
	}
	if e := scan.Err(); e != nil {
		return nil, e
	}
	if len(out.mounts) == 0 {
		return nil, fmt.Errorf("cannot determine sandbox mount ownership")
	}
	return out, nil
}
func newRouter(cwd string) (*pathRouter, error) {
	f, e := os.Open("/proc/self/mountinfo")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return parseMounts(f, cwd)
}
func beneath(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}
func (r *pathRouter) owner(path string) string {
	side := "agent"
	best := -1
	for _, m := range r.mounts {
		if beneath(path, m.path) && len(m.path) >= best {
			best = len(m.path)
			side = m.side
		}
	}
	return side
}

// Resolve a local symlink without probing projected paths. For a new file,
// resolve its existing parent, then restore the missing suffix.
func (r *pathRouter) resolve(path string) (string, error) {
	tail := []string{}
	p := path
	for {
		if r.owner(p) == "host" {
			for i := len(tail) - 1; i >= 0; i-- {
				p = filepath.Join(p, tail[i])
			}
			return p, nil
		}
		real, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}
func (r *pathRouter) route(path, location string, search bool) (string, string, error) {
	if path == "" {
		path = r.cwd
	}
	if !filepath.IsAbs(path) {
		if !search {
			return "", "", fmt.Errorf("path must be absolute")
		}
		path = filepath.Join(r.cwd, path)
	}
	path = filepath.Clean(path)
	if location == "host" {
		return path, "host", nil
	}
	if location != "" && location != "auto" && location != "agent" {
		return "", "", fmt.Errorf("invalid location")
	}
	side := r.owner(path)
	if side == "agent" {
		real, e := r.resolve(path)
		if e != nil {
			return "", "", e
		}
		path = real
		side = r.owner(path)
	}
	if location == "agent" && side == "host" {
		return "", "", fmt.Errorf("%s is projected from host; it is not an agent-local file", path)
	}
	if search && side == "agent" {
		for _, m := range r.mounts {
			if m.side == "host" && beneath(m.path, path) {
				return "", "", fmt.Errorf("search crosses a host projection at %s; narrow path or specify location=host", m.path)
			}
		}
	}
	return path, side, nil
}
