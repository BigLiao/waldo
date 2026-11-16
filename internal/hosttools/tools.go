// Package hosttools runs file operations inside A's existing sandbox.
package hosttools

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const maxFile = 16 << 20
const maxOutput = 64 << 10

// Args also carries private fields used between the MCP and its sandbox worker.
// Public schemas are defined separately and match Claude's file-tool inputs.
type Args struct {
	Path         string `json:"path,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	NotebookPath string `json:"notebook_path,omitempty"`
	Location     string `json:"location,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Pages        string `json:"pages,omitempty"`
	AllowLarge   bool   `json:"allow_large,omitempty"`
	Content      string `json:"content"`
	Old          string `json:"old_string"`
	New          string `json:"new_string"`
	SHA256       string `json:"expected_sha256,omitempty"`
	ReplaceAll   bool   `json:"replace_all,omitempty"`
	Glob         string `json:"glob,omitempty"`
	Type         string `json:"type,omitempty"`
	OutputMode   string `json:"output_mode,omitempty"`
	IgnoreCase   bool   `json:"-i,omitempty"`
	OnlyMatching bool   `json:"-o,omitempty"`
	LineNumbers  *bool  `json:"-n,omitempty"`
	Before       int    `json:"-B,omitempty"`
	After        int    `json:"-A,omitempty"`
	Around       int    `json:"-C,omitempty"`
	Context      int    `json:"context,omitempty"`
	HeadLimit    *int   `json:"head_limit,omitempty"`
	Multiline    bool   `json:"multiline,omitempty"`
	CellID       string `json:"cell_id,omitempty"`
	NewSource    string `json:"new_source"`
	CellType     string `json:"cell_type,omitempty"`
	EditMode     string `json:"edit_mode,omitempty"`
}

func Known(name string) bool {
	switch name {
	case "Read", "Write", "Edit", "Grep", "Glob", "NotebookEdit":
		return true
	}
	return false
}
func Decode(name string, raw []byte) (Args, error) {
	var a Args
	if len(raw) > 1<<20 {
		return a, fmt.Errorf("arguments exceed 1 MiB")
	}
	if !Known(name) {
		return a, fmt.Errorf("unknown file tool")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&a); e != nil {
		return a, e
	}
	if d.Decode(new(any)) != io.EOF {
		return a, fmt.Errorf("expected one arguments object")
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil {
		return a, e
	}
	required := []string{}
	switch name {
	case "Read", "Write", "Edit":
		if a.FilePath != "" {
			if a.Path != "" && a.Path != a.FilePath {
				return a, fmt.Errorf("conflicting paths")
			}
			a.Path = a.FilePath
		}
		if a.Path == "" {
			return a, fmt.Errorf("file_path is required")
		}
	case "NotebookEdit":
		if a.NotebookPath != "" {
			a.Path = a.NotebookPath
		}
		if a.Path == "" {
			return a, fmt.Errorf("notebook_path is required")
		}
	}
	switch name {
	case "Grep", "Glob":
		required = []string{"pattern"}
	case "Write":
		required = []string{"content"}
	case "Edit":
		required = []string{"old_string", "new_string"}
	case "NotebookEdit":
		required = []string{"new_source"}
	}
	for _, key := range required {
		var v string
		if raw, ok := fields[key]; !ok || string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
			return a, fmt.Errorf("%s must be a string", key)
		}
	}
	if a.Path != "" && ((!filepath.IsAbs(a.Path) && name != "Grep" && name != "Glob") || strings.ContainsRune(a.Path, 0)) {
		return a, fmt.Errorf("file paths must be absolute")
	}
	if a.Location != "" && a.Location != "auto" && a.Location != "host" && a.Location != "agent" {
		return a, fmt.Errorf("location must be auto, host or agent")
	}
	if a.Offset < 0 || a.Limit < 0 || a.Before < 0 || a.After < 0 || a.Around < 0 || a.Context < 0 || (a.HeadLimit != nil && *a.HeadLimit < 0) {
		return a, fmt.Errorf("line counts must be nonnegative")
	}
	if name == "Edit" && a.Old == "" {
		return a, fmt.Errorf("old_string must not be empty")
	}
	if a.SHA256 != "" {
		b, e := hex.DecodeString(a.SHA256)
		if e != nil || len(b) != 32 {
			return a, fmt.Errorf("invalid file version")
		}
	}
	if name == "Grep" && (a.OutputMode != "" && a.OutputMode != "content" && a.OutputMode != "count" && a.OutputMode != "files_with_matches") {
		return a, fmt.Errorf("invalid output_mode")
	}
	if name == "NotebookEdit" {
		if a.EditMode != "" && a.EditMode != "replace" && a.EditMode != "insert" && a.EditMode != "delete" {
			return a, fmt.Errorf("invalid edit_mode")
		}
		if a.CellType != "" && a.CellType != "code" && a.CellType != "markdown" {
			return a, fmt.Errorf("invalid cell_type")
		}
	}
	return a, nil
}

func digest(b []byte) string              { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func readFile(f *os.File) ([]byte, error) { return readFileLimit(f, maxFile) }
func readFileLimit(f *os.File, maxFile int) ([]byte, error) {
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("regular files only")
	}
	if st.Size() > int64(maxFile) {
		return nil, fmt.Errorf("file exceeds %d MiB", maxFile>>20)
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(maxFile)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("file exceeds %d MiB", maxFile>>20)
	}
	if !utf8.Valid(b) || bytes.ContainsRune(b, 0) {
		return nil, fmt.Errorf("UTF-8 text files only; use Bash for binary/notebook transformations")
	}
	return b, nil
}

func Run(name string, a Args) (any, error) { return RunWithRG(name, a, "rg") }
func RunWithRG(name string, a Args, rg string) (any, error) {
	switch name {
	case "Read":
		if result, handled, e := readSpecial(a, rg); handled {
			return result, e
		}
		f, e := os.Open(a.Path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		maxRead := maxFile
		maxResult := maxOutput
		if a.AllowLarge {
			maxRead = 64 << 20
			maxResult = 1 << 20
		}
		b, e := readFileLimit(f, maxRead)
		if e != nil {
			return nil, e
		}
		lines := strings.Split(string(b), "\n")
		if len(b) == 0 {
			lines = nil
		} else if b[len(b)-1] == '\n' {
			lines = lines[:len(lines)-1]
		}
		start := a.Offset
		if start == 0 {
			start = 1
		}
		limit := a.Limit
		if limit == 0 {
			limit = 2000
		}
		var out strings.Builder
		next := min(start-1, len(lines))
		truncated := false
		for next < len(lines) && next < start-1+limit {
			line := fmt.Sprintf("%d\t%s\n", next+1, lines[next])
			if out.Len()+len(line) > maxResult {
				truncated = true
				break
			}
			out.WriteString(line)
			next++
		}
		return map[string]any{"path": a.Path, "sha256": digest(b), "content": out.String(), "total_lines": len(lines), "next_line": next + 1, "has_more": next < len(lines), "line_too_large": truncated && out.Len() == 0}, nil
	case "Grep", "Glob":
		return search(name, a, rg)
	case "NotebookEdit":
		return notebookEdit(a)
	case "Edit", "Write":
		return modify(name, a)
	}
	return nil, fmt.Errorf("unknown tool")
}

func modify(name string, a Args) (any, error) {
	if name == "Edit" && a.SHA256 == "" {
		return nil, fmt.Errorf("Read the file before editing")
	}
	if len(a.Content) > maxFile {
		return nil, fmt.Errorf("content too large")
	}
	flags := os.O_RDWR
	if a.SHA256 == "" {
		flags |= os.O_CREATE | os.O_EXCL
	}
	if a.SHA256 == "" {
		if e := os.MkdirAll(filepath.Dir(a.Path), 0755); e != nil {
			return nil, e
		}
	}
	f, e := os.OpenFile(a.Path, flags, 0644)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	// Serialize Waldo file writers. External writers need not take this lock;
	// the expected digest detects changes already present at this boundary.
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return nil, e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	b, e := readFile(f)
	if e != nil {
		return nil, e
	}
	if a.SHA256 != "" && digest(b) != a.SHA256 {
		return nil, fmt.Errorf("file changed: read it again before editing")
	}
	result := a.Content
	if name == "Edit" {
		count := strings.Count(string(b), a.Old)
		if count == 0 || (!a.ReplaceAll && count != 1) {
			return nil, fmt.Errorf("old_string matched %d times; expected one (or use replace_all)", count)
		}
		n := 1
		if a.ReplaceAll {
			n = -1
		}
		result = strings.Replace(string(b), a.Old, a.New, n)
	}
	if len(result) > maxFile {
		return nil, fmt.Errorf("result too large")
	}
	if _, e = f.Seek(0, 0); e != nil {
		return nil, e
	}
	if _, e = io.WriteString(f, result); e != nil {
		return nil, e
	}
	if e = f.Truncate(int64(len(result))); e != nil {
		return nil, e
	}
	if e = f.Sync(); e != nil {
		return nil, e
	}
	return map[string]any{"path": a.Path, "sha256": digest([]byte(result)), "bytes": len(result)}, nil
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		b.Buffer.Write(p[:min(n, 4096-b.Len())])
	}
	return n, nil
}
func search(name string, a Args, rg string) (any, error) {
	args := []string{"--no-config", "--color=never"}
	if name == "Grep" {
		mode := a.OutputMode
		if mode == "" {
			mode = "files_with_matches"
		}
		switch mode {
		case "files_with_matches":
			args = append(args, "--files-with-matches")
		case "count":
			args = append(args, "--count", "--with-filename")
		case "content":
			args = append(args, "--with-filename", "--no-heading")
			if a.LineNumbers == nil || *a.LineNumbers {
				args = append(args, "--line-number")
			}
			if a.OnlyMatching {
				args = append(args, "--only-matching")
			}
			for _, pair := range []struct {
				flag string
				n    int
			}{{"-B", a.Before}, {"-A", a.After}, {"-C", a.Around}, {"-C", a.Context}} {
				if pair.n > 0 {
					args = append(args, pair.flag, strconv.Itoa(pair.n))
				}
			}
		}
		if a.IgnoreCase {
			args = append(args, "--ignore-case")
		}
		if a.Multiline {
			args = append(args, "--multiline", "--multiline-dotall")
		}
		if a.Glob != "" {
			args = append(args, "--glob", a.Glob)
		}
		if a.Type != "" {
			args = append(args, "--type", a.Type)
		}
		args = append(args, "--", a.Pattern, a.Path)
	} else {
		args = append(args, "--files", "--sortr=modified", "--glob", a.Pattern, "--", a.Path)
	}
	cmd := exec.Command(rg, args...)
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	limit := 200
	if a.HeadLimit != nil {
		limit = *a.HeadLimit
		if limit == 0 {
			limit = int(^uint(0) >> 1)
		}
	}
	if a.Limit > 0 {
		limit = a.Limit
	}
	var result strings.Builder
	// Stream bounded lines; avoid buffering a huge match or an entire search.
	r := bufioReader(out)
	index, count := 0, 0
	more := false
	var readErr error
	for {
		line, e := r.ReadString('\n')
		if len(line) > 0 {
			if index >= a.Offset {
				if count >= limit || result.Len()+len(line) > maxOutput {
					more = true
					break
				}
				result.WriteString(line)
				count++
			}
			index++
		}
		if e != nil {
			if e != io.EOF {
				readErr = e
			}
			break
		}
	}
	if more || readErr != nil {
		cmd.Process.Kill()
	}
	out.Close()
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if !more && waitErr != nil {
		if ex, ok := waitErr.(*exec.ExitError); !ok || ex.ExitCode() != 1 {
			return nil, fmt.Errorf("rg: %v: %s", waitErr, stderr.String())
		}
	}
	return map[string]any{"content": result.String(), "has_more": more, "next_offset": a.Offset + count, "line_too_large": more && count == 0}, nil
}

// boundedLineReader caps a single result line as well as total output.
type boundedLineReader struct{ r *bufio.Reader }

func bufioReader(r io.Reader) *boundedLineReader {
	return &boundedLineReader{bufio.NewReaderSize(r, maxOutput+1)}
}
func (r *boundedLineReader) ReadString(delim byte) (string, error) {
	b, e := r.r.ReadSlice(delim)
	if e == bufio.ErrBufferFull {
		return "", fmt.Errorf("matching line exceeds 64 KiB; narrow the query or use Bash")
	}
	return string(b), e
}

// Main is invoked through the existing exec protocol inside A's sandbox.
func Main(args []string) int {
	if len(args) != 1 && !(len(args) == 3 && args[1] == "--rg") {
		return 2
	}
	raw, e := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
	if e != nil {
		return 2
	}
	a, e := Decode(args[0], raw)
	var result any
	if e == nil {
		a.Path = nativeAlias(a.Path)
		rg := "rg"
		if len(args) == 3 {
			rg = args[2]
		}
		result, e = RunWithRG(args[0], a, rg)
	}
	if e != nil {
		json.NewEncoder(os.Stderr).Encode(map[string]string{"error": e.Error()})
		return 1
	}
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		return 1
	}
	return 0
}

// Native skill aliases are presentation inside the sandbox, never an access
// check. Kernel filesystem policy still applies to the mapped destination.
func nativeAlias(path string) string {
	var roots map[string]string
	if raw := os.Getenv("WALDO_FILE_ALIASES"); len(raw) <= 65536 {
		json.Unmarshal([]byte(raw), &roots)
	}
	best := ""
	for root := range roots {
		if filepath.IsAbs(root) && (path == root || strings.HasPrefix(path, root+"/")) && len(root) > len(best) {
			best = root
		}
	}
	if best != "" && filepath.IsAbs(roots[best]) {
		return filepath.Join(roots[best], strings.TrimPrefix(path, best))
	}
	return path
}
