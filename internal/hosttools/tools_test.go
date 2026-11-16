package hosttools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEditAndConflict(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(p, []byte("one\ntwo\nthree\n"), 0600)
	v, e := Run("Read", Args{Path: p, Offset: 2, Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	r := v.(map[string]any)
	if r["content"] != "2\ttwo\n" || r["has_more"] != true || r["next_line"] != 3 {
		t.Fatal(r)
	}
	hash := r["sha256"].(string)
	v, e = Run("Edit", Args{Path: p, Old: "two", New: "second", SHA256: hash})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "one\nsecond\nthree\n" {
		t.Fatal(string(b))
	}
	if _, e = Run("Edit", Args{Path: p, Old: "one", New: "wrong", SHA256: hash}); e == nil {
		t.Fatal("accepted stale digest")
	}
	if _, e = Run("Write", Args{Path: p, Content: "wrong"}); e == nil {
		t.Fatal("overwrote existing file without version")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal("changed file permissions")
	}
	if _, e = Run("Write", Args{Path: p, Content: "short", SHA256: v.(map[string]any)["sha256"].(string)}); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "short" {
		t.Fatal(string(b))
	}
}
func TestNewWriteAndAmbiguousEdit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.txt")
	if _, e := Run("Write", Args{Path: p, Content: "x x"}); e != nil {
		t.Fatal(e)
	}
	a := Args{Path: p, Old: "x", New: "y", SHA256: digest([]byte("x x"))}
	if _, e := Run("Edit", a); e == nil {
		t.Fatal("accepted ambiguous match")
	}
	a.ReplaceAll = true
	if _, e := Run("Edit", a); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "y y" {
		t.Fatal(string(b))
	}
}
func TestReadBoundsAndBinary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "file")
	os.WriteFile(p, []byte("a\x00b"), 0600)
	if _, e := Run("Read", Args{Path: p}); e == nil {
		t.Fatal("accepted binary file")
	}
	os.WriteFile(p, []byte(strings.Repeat("a", maxOutput+1)), 0600)
	r, e := Run("Read", Args{Path: p})
	if e != nil {
		t.Fatal(e)
	}
	if r.(map[string]any)["line_too_large"] != true {
		t.Fatal(r)
	}
	f, _ := os.OpenFile(p, os.O_WRONLY, 0600)
	f.Truncate(maxFile + 1)
	f.Close()
	if _, e := Run("Read", Args{Path: p}); e == nil {
		t.Fatal("read oversized file")
	}
	if _, e := Run("Read", Args{Path: filepath.Dir(p)}); e == nil {
		t.Fatal("read directory")
	}
}
func TestSearchAndGlob(t *testing.T) {
	if _, e := exec.LookPath("rg"); e != nil {
		t.Skip("rg unavailable")
	}
	d := t.TempDir()
	p := filepath.Join(d, "a.py")
	os.WriteFile(p, []byte("needle\nneedle two\nother\n"), 0600)
	r, e := Run("Grep", Args{Path: d, Pattern: "needle", OutputMode: "content", Limit: 1})
	if e != nil {
		t.Fatal(e)
	}
	v := r.(map[string]any)
	if !strings.Contains(v["content"].(string), "a.py:1:needle") || v["has_more"] != true {
		t.Fatal(v)
	}
	r, e = Run("Grep", Args{Path: p, Pattern: "needle", OutputMode: "content", Offset: 1})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(r.(map[string]any)["content"].(string), ":2:needle two") {
		t.Fatal(r)
	}
	r, e = Run("Glob", Args{Path: d, Pattern: "**/*.py"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(r.(map[string]any)["content"].(string), "a.py") {
		t.Fatal(r)
	}
	r, e = Run("Grep", Args{Path: p, Pattern: "nomatch"})
	if e != nil || r.(map[string]any)["content"] != "" {
		t.Fatal(r, e)
	}
	if _, e = Run("Grep", Args{Path: p, Pattern: "["}); e == nil {
		t.Fatal("invalid regex accepted")
	}
}
func TestDecodeValidation(t *testing.T) {
	for _, raw := range []string{`{"path":"relative"}`, `{"path":"/x","extra":true}`, `{"path":"/x","limit":-1}`, `{"path":"/x"} {}`} {
		if _, e := Decode("Read", []byte(raw)); e == nil {
			t.Fatal(raw)
		}
	}
	if _, e := Run("Edit", Args{Path: "/x", Old: "a", New: "b"}); e == nil {
		t.Fatal("accepted missing read")
	}
	for _, def := range Definitions() {
		b, e := json.Marshal(def)
		if e != nil || len(b) == 0 {
			t.Fatal(e)
		}
	}
}

func TestNativeParametersAndPrivateVersion(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		fail      bool
	}{
		{"Read", `{"file_path":"/x","offset":1,"limit":2000,"location":"agent"}`, false},
		{"Edit", `{"file_path":"/x","old_string":"a","new_string":""}`, false},
		{"Write", `{"file_path":"/x","content":""}`, false},
		{"Read", `{"path":"/x"}`, true},
		{"Read", `{"file_path":"/x","limit":0}`, true},
		{"Write", `{"file_path":"/x","content":"x","expected_sha256":""}`, true},
		{"Grep", `{"pattern":"x","path":"src","output_mode":"content","-i":true,"-A":2,"head_limit":0}`, false},
		{"Glob", `{"pattern":"*.go","offset":2}`, true},
	} {
		a, e := DecodePublic(tt.name, []byte(tt.raw))
		if (e != nil) != tt.fail {
			t.Errorf("%s %s: %v", tt.name, tt.raw, e)
		}
		if e == nil {
			b, _ := json.Marshal(a)
			if _, e = Decode(tt.name, b); e != nil {
				t.Errorf("private worker round trip: %v", e)
			}
		}
	}
}

func TestSearchNativeModes(t *testing.T) {
	if _, e := exec.LookPath("rg"); e != nil {
		t.Skip("rg unavailable")
	}
	d := t.TempDir()
	p := filepath.Join(d, "a.py")
	os.WriteFile(p, []byte("before\nNEEDLE\nafter\nneedle again\n"), 0600)
	os.WriteFile(filepath.Join(d, "b.txt"), []byte("needle"), 0600)
	zero := 0
	no := false
	for _, tt := range []struct {
		a            Args
		want, absent string
	}{
		{Args{Path: d, Pattern: "needle", IgnoreCase: true, Glob: "*.py"}, "a.py", "b.txt"},
		{Args{Path: d, Pattern: "needle", IgnoreCase: true, Type: "py", OutputMode: "count"}, "a.py:2", "b.txt"},
		{Args{Path: p, Pattern: "NEEDLE", OutputMode: "content", Before: 1, After: 1, LineNumbers: &no}, "before", "a.py:2:"},
		{Args{Path: p, Pattern: "needle", IgnoreCase: true, OutputMode: "content", HeadLimit: &zero, Offset: 1}, "needle again", "NEEDLE"},
		{Args{Path: p, Pattern: "NEEDLE.*after", Multiline: true, OutputMode: "content"}, "after", "needle again"},
	} {
		v, e := Run("Grep", tt.a)
		if e != nil {
			t.Fatal(e)
		}
		out := v.(map[string]any)["content"].(string)
		if !strings.Contains(out, tt.want) || strings.Contains(out, tt.absent) {
			t.Fatalf("%+v: %q", tt.a, out)
		}
	}
}

func TestNotebookEdits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.ipynb")
	os.WriteFile(p, []byte(`{"cells":[{"id":"first","cell_type":"code","metadata":{},"source":["old"],"outputs":[]}],"nbformat":4,"nbformat_minor":5,"metadata":{}}`), 0600)
	version := func() string { b, _ := os.ReadFile(p); return digest(b) }
	if _, e := Run("NotebookEdit", Args{Path: p, SHA256: version(), CellID: "first", NewSource: "new"}); e != nil {
		t.Fatal(e)
	}
	if _, e := Run("NotebookEdit", Args{Path: p, SHA256: version(), CellID: "first", CellType: "markdown", EditMode: "insert", NewSource: "text"}); e != nil {
		t.Fatal(e)
	}
	if _, e := Run("NotebookEdit", Args{Path: p, SHA256: version(), CellID: "0", EditMode: "delete"}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	var book map[string]any
	json.Unmarshal(b, &book)
	cells := book["cells"].([]any)
	if len(cells) != 1 || cells[0].(map[string]any)["cell_type"] != "markdown" {
		t.Fatal(string(b))
	}
}

func TestNativeAliasUsesLongestDirectoryPrefix(t *testing.T) {
	t.Setenv("WALDO_FILE_ALIASES", `{"/logical/skill":"/staged/skill","/logical/skill/nested":"/staged/nested"}`)
	for in, want := range map[string]string{"/logical/skill/SKILL.md": "/staged/skill/SKILL.md", "/logical/skill/nested/ref.txt": "/staged/nested/ref.txt", "/logical/skill-other/file": "/logical/skill-other/file"} {
		if got := nativeAlias(in); got != want {
			t.Errorf("%s => %s want %s", in, got, want)
		}
	}
}
