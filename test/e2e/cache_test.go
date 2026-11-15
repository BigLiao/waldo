package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContentCacheFreshness(t *testing.T) {
	fuse, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("content cache mount test requires usable /dev/fuse: %v", err)
	}
	fuse.Close()
	e, agent := setup(t)
	e.WriteConfig("[fs]\nmetadata_cache_ttl = \"60s\"\ncontent_cache_ttl = \"60s\"\n")
	source := filepath.Join(e.Project, "cache-source")
	renamed := filepath.Join(e.Project, "cache-renamed")
	if err := os.WriteFile(source, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123456100)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	cmd := e.Cmd("run", "--", agent, "cache")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		stdin.Close()
		stdout.Close()
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	timer := time.AfterFunc(45*time.Second, func() { cmd.Process.Kill() })
	defer timer.Stop()
	var output strings.Builder
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		output.WriteString(line + "\n")
		phase, ready := strings.CutPrefix(line, "CACHE_READY ")
		if !ready {
			continue
		}
		if seen[phase] {
			t.Fatalf("repeated phase %q", phase)
		}
		seen[phase] = true
		switch phase {
		case "edit":
			if err := os.WriteFile(source, []byte("eeee"), 0600); err != nil {
				t.Fatal(err)
			}
			changed := original.ModTime().Add(100 * time.Nanosecond)
			if err := os.Chtimes(source, changed, changed); err != nil {
				t.Fatal(err)
			}
		case "replace":
			// Same size and timestamp: inode identity must detect an atomic
			// replacement even though the high-precision mtime is preserved.
			tmp := filepath.Join(e.Project, "cache-replacement")
			if err := os.WriteFile(tmp, []byte("bbbb"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(tmp, original.ModTime(), original.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, source); err != nil {
				t.Fatal(err)
			}
		case "rename":
			if err := os.Rename(source, renamed); err != nil {
				t.Fatal(err)
			}
		case "delete":
			if err := os.Remove(renamed); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown cache phase %q", phase)
		}
		if _, err := fmt.Fprintln(stdin, "continue"); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("content cache probe: %v\n%s\nstderr:\n%s", err, output.String(), stderr.String())
	}
	results := parseResults(output.String())
	for _, name := range []string{"external_replace", "external_edit", "write_then_read", "exec_then_read", "external_rename", "external_delete", "ok"} {
		if results["cache_"+name] != "true" {
			t.Errorf("cache_%s = %q\n%s", name, results["cache_"+name], output.String())
		}
	}
	for _, phase := range []string{"replace", "edit", "rename", "delete"} {
		if !seen[phase] {
			t.Errorf("missing external modification phase %q", phase)
		}
	}
	log, err := os.ReadFile(filepath.Join(e.BData, "log", "host.log"))
	if err != nil || !strings.Contains(string(log), "content_cache_ttl=60s") {
		t.Errorf("content cache proxy was not mounted: %v\n%s", err, log)
	}
	if log, err := os.ReadFile(filepath.Join(e.AData, "audit.log")); err != nil || strings.Contains(string(log), `"kind":"fs-watch"`) {
		t.Errorf("passive cache used notifications: %v", err)
	}
	waitNoMounts(t)
}
