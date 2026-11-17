package transport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"waldo/internal/proto"
	"waldo/internal/util"
)

func TestConcurrentVersionInstallationNeverReplacesPublishedBinary(t *testing.T) {
	dir := t.TempDir()
	tr := &SSH{RemoteDir: dir}
	candidate := "#!/bin/sh\ncase \"${0##*/}\" in waldo*) ;; *) exit 11 ;; esac\nprintf '%s\\n' " + util.ShellQuote(proto.FullVersion()) + "\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(candidate)))
	// An older installation belongs to other clients and must remain intact.
	legacy := filepath.Join(dir, "bin", "waldo")
	os.MkdirAll(filepath.Dir(legacy), 0700)
	os.WriteFile(legacy, []byte("legacy-client"), 0700)
	install := func(body, hash string) error {
		cmd := exec.Command("sh", "-c", tr.installScript(hash))
		cmd.Stdin = strings.NewReader(body)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := install(candidate, digest); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	target := filepath.Join(dir, "bin", versionKey(), "waldo")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	changed := candidate + "# different candidate with same claimed version\n"
	if err := install(changed, fmt.Sprintf("%x", sha256.Sum256([]byte(changed)))); err == nil {
		t.Fatal("immutable version was replaced")
	}
	after, err := os.Stat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("published inode changed", err)
	}
	if err := install(candidate, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted corrupt upload")
	}
	if b, _ := os.ReadFile(target); string(b) != candidate {
		t.Fatal("published bytes changed")
	}
	if b, _ := os.ReadFile(legacy); string(b) != "legacy-client" {
		t.Fatal("legacy installation changed")
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(target), "waldo-upload.*")); len(files) != 0 {
		t.Fatal("staged uploads leaked", files)
	}
	if strings.Contains(tr.bootstrap("--reconnect-attach token"), `B="$D/bin/waldo"`) {
		t.Fatal("reconnect uses mutable legacy path")
	}
}
