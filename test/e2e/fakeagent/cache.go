package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cacheProbe deliberately synchronizes external changes over stdio. No
// forwarded command runs between warming a file and observing those changes,
// so freshness there depends on the watcher rather than the exec barrier.
func cacheProbe() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	source := filepath.Join(cwd, "cache-source")
	renamed := filepath.Join(cwd, "cache-renamed")
	input := bufio.NewScanner(os.Stdin)
	handshake := func(phase string) error {
		fmt.Println("CACHE_READY " + phase)
		if !input.Scan() || input.Text() != "continue" {
			return fmt.Errorf("cache probe %s handshake: %v", phase, input.Err())
		}
		return nil
	}
	warm := func(path, want string) error {
		for i := 0; i < 3; i++ {
			b, err := os.ReadFile(path)
			if err != nil || string(b) != want {
				return fmt.Errorf("warm %s: got %q, error %v", path, b, err)
			}
		}
		return nil
	}
	await := func(name string, check func() bool) error {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if check() {
				result("cache_"+name, true)
				return nil
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("cache %s did not become fresh within 3s (TTL is 60s)", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	contents := func(path, want string) bool {
		b, err := os.ReadFile(path)
		return err == nil && string(b) == want
	}
	if err := warm(source, "aaaa"); err != nil {
		return err
	}
	if err := handshake("replace"); err != nil {
		return err
	}
	if err := await("external_replace", func() bool { return contents(source, "bbbb") }); err != nil {
		return err
	}
	if err := warm(source, "bbbb"); err != nil {
		return err
	}
	if err := os.WriteFile(source, []byte("cccc"), 0o600); err != nil {
		return err
	}
	if !contents(source, "cccc") {
		return fmt.Errorf("SFTP write succeeded but the immediate read was stale")
	}
	result("cache_write_then_read", true)
	if err := warm(source, "cccc"); err != nil {
		return err
	}
	if err := metadataCommand(`printf '%s' dddd > "$1"`, source); err != nil {
		return err
	}
	if !contents(source, "dddd") {
		return fmt.Errorf("forwarded command completed but the immediate read was stale")
	}
	result("cache_exec_then_read", true)
	if err := warm(source, "dddd"); err != nil {
		return err
	}
	if _, err := os.ReadDir(cwd); err != nil {
		return err
	}
	if err := handshake("rename"); err != nil {
		return err
	}
	if err := await("external_rename", func() bool {
		_, err := os.Stat(source)
		return os.IsNotExist(err) && contents(renamed, "dddd")
	}); err != nil {
		return err
	}
	if err := warm(renamed, "dddd"); err != nil {
		return err
	}
	if err := handshake("delete"); err != nil {
		return err
	}
	if err := await("external_delete", func() bool {
		_, err := os.ReadFile(renamed)
		return os.IsNotExist(err)
	}); err != nil {
		return err
	}
	result("cache_ok", true)
	return nil
}
