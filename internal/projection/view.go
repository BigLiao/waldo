// Package projection adds read-only session instruction files to the SFTP
// view. Ordinary file operations remain handled by pkg/sftp's OS server.
// It is presentation inside the sandbox, not a sandbox security boundary.
package projection

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"waldo/internal/proto"
)

const Reference = "Read [waldo.md](" + proto.ContextFile + ") for this session's environment, permissions and limitations. <!-- waldo:context -->"

type file struct {
	name  string
	data  []byte
	mtime time.Time
}

func (f *file) Name() string       { return f.name }
func (f *file) Size() int64        { return int64(len(f.data)) }
func (f *file) Mode() os.FileMode  { return 0o444 }
func (f *file) ModTime() time.Time { return f.mtime }
func (f *file) IsDir() bool        { return false }
func (f *file) Sys() any           { return nil }

// View is an immutable startup snapshot of instruction contents. The real
// files are read inside the sandbox, never by the trusted host process.
type View struct {
	project string
	files   map[string]*file
}

func New(project string, names []string) (*View, error) {
	root, err := filepath.EvalSymlinks(project)
	if err != nil {
		return nil, err
	}
	v := &View{project: root, files: map[string]*file{}}
	for _, name := range names {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\\\x00\r\n") {
			return nil, fmt.Errorf("invalid instruction filename %q", name)
		}
		p := filepath.Join(root, name)
		data, err := readInstructions(p)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read instruction file %s: %w", name, err)
		}
		// Only append our own line once, including when a project already has it.
		if !bytes.Contains(data, []byte(Reference)) {
			data = append([]byte{}, data...)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				data = append(data, '\n')
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, Reference...)
			data = append(data, '\n')
		}
		v.files[name] = &file{name: name, data: data, mtime: time.Now()}
	}
	return v, nil
}

func readInstructions(path string) ([]byte, error) {
	// An instruction filename can be replaced by a FIFO or device. Opening in
	// nonblocking mode and checking its type avoids hanging session startup.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("instruction file must be a regular file")
	}
	const maxSize = 4 << 20
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSize {
		return nil, fmt.Errorf("instruction file exceeds 4 MiB")
	}
	return b, nil
}

func (v *View) canonical(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(v.project, p)
	}
	p = filepath.Clean(p)
	if parent, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(parent, filepath.Base(p))
	}
	return p
}

func (v *View) virtual(p string) *file {
	p = v.canonical(p)
	if filepath.Dir(p) == v.project {
		return v.files[filepath.Base(p)]
	}
	return nil
}

func (v *View) root(p string) bool {
	p = v.canonical(p)
	if p == v.project {
		return true
	}
	r, err := filepath.EvalSymlinks(p)
	return err == nil && r == v.project
}

func (v *View) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if f := v.virtual(r.Filepath); f != nil {
		return bytes.NewReader(f.data), nil
	}
	return nil, os.ErrNotExist
}
func (*View) Filewrite(*sftp.Request) (io.WriterAt, error) { return nil, os.ErrPermission }
func (*View) Filecmd(*sftp.Request) error                  { return os.ErrPermission }

type listing []os.FileInfo

func (l listing) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[offset:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

func (v *View) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if f := v.virtual(r.Filepath); f != nil {
		if r.Method == "List" {
			return nil, syscall.ENOTDIR
		}
		return listing{f}, nil
	}
	if !v.root(r.Filepath) {
		return nil, os.ErrNotExist
	}
	if r.Method != "List" {
		st, err := os.Stat(v.project)
		if err != nil {
			return nil, err
		}
		return listing{st}, nil
	}
	entries, err := os.ReadDir(v.project)
	if err != nil {
		return nil, err
	}
	var out listing
	for _, e := range entries {
		if v.files[e.Name()] != nil {
			continue
		}
		st, err := e.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	for _, f := range v.files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (v *View) Lstat(r *sftp.Request) (sftp.ListerAt, error) { return v.Filelist(r) }
func (*View) Readlink(string) (string, error)                { return "", syscall.EINVAL }
