package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObservePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, source, project string
		want                  bool
	}{
		{"default off", "", "/project", false},
		{"global on", "[observe]\nenabled = true", "/project", true},
		{"global off", "[observe]\nenabled = false", "/project", false},
		{"project on", "[project.\"/project\".observe]\nenabled = true", "/project", true},
		{"project off overrides global on", "[observe]\nenabled = true\n[project.\"/project\".observe]\nenabled = false", "/project", false},
		{"project on overrides global off", "[observe]\nenabled = false\n[project.\"/project\".observe]\nenabled = true", "/project", true},
		{"other project inherits global", "[observe]\nenabled = true\n[project.\"/project\".observe]\nenabled = false", "/other", true},
		{"omitted project setting inherits global", "[observe]\nenabled = true\n[project.\"/project\".fs]\nmetadata_cache_ttl = '0s'", "/project", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := loadCacheConfig(t, tc.source).PolicyFor(tc.project)
			if err != nil {
				t.Fatal(err)
			}
			if p.ObserveEnabled != tc.want {
				t.Fatalf("ObserveEnabled = %t, want %t", p.ObserveEnabled, tc.want)
			}
		})
	}
}

func TestObservePolicyExpandsProjectHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	c := loadCacheConfig(t, "[project.\"~/work/shop\".observe]\nenabled = true")
	p, err := c.PolicyFor(filepath.Join(home, "work", "shop"))
	if err != nil || !p.ObserveEnabled {
		t.Fatalf("expanded project: policy=%+v, err=%v", p, err)
	}
}

func TestObserveEnabledRequiresBoolean(t *testing.T) {
	for _, source := range []string{"[observe]\nenabled = 'true'", "[project.\"/project\".observe]\nenabled = 1"} {
		t.Setenv("WALDO_CONFIG_DIR", t.TempDir())
		if err := os.WriteFile(Path(), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("accepted non-boolean observe.enabled: %s", source)
		}
	}
}
