package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadCacheConfig(t *testing.T, source string) *Config {
	t.Helper()
	t.Setenv("WALDO_CONFIG_DIR", t.TempDir())
	if err := os.WriteFile(Path(), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMetadataCacheTTLPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		project string
		want    time.Duration
	}{
		{"default", "", "/project", 10 * time.Second},
		{"larger global cache", "[fs]\nmetadata_cache_ttl = \"30s\"\n", "/project", 30 * time.Second},
		{"larger project override", "[fs]\nmetadata_cache_ttl = \"10s\"\n[project.\"/project\".fs]\nmetadata_cache_ttl = \"30s\"\n", "/project", 30 * time.Second},
		{"global", "[fs]\nmetadata_cache_ttl = \"4s\"\n", "/project", 4 * time.Second},
		{"global disabled", "[fs]\nmetadata_cache_ttl = \"0s\"\n", "/project", 0},
		{"project zero overrides global", "[fs]\nmetadata_cache_ttl = \"4s\"\n[project.\"/project\".fs]\nmetadata_cache_ttl = \"0s\"\n", "/project", 0},
		{"other project inherits global", "[fs]\nmetadata_cache_ttl = \"4s\"\n[project.\"/project\".fs]\nmetadata_cache_ttl = \"0s\"\n", "/other", 4 * time.Second},
		{"project without fs inherits global", "[fs]\nmetadata_cache_ttl = \"4s\"\n[project.\"/project\".context]\nenabled = false\n", "/project", 4 * time.Second},
		{"project overrides default", "[project.\"/project\".fs]\nmetadata_cache_ttl = \"8s\"\n", "/project", 8 * time.Second},
		{"unrelated project keeps default", "[project.\"/project\".fs]\nmetadata_cache_ttl = \"8s\"\n", "/other", 10 * time.Second},
		{"maximum", "[fs]\nmetadata_cache_ttl = \"1m\"\n", "/project", time.Minute},
		{"whole seconds expressed as milliseconds", "[fs]\nmetadata_cache_ttl = \"2000ms\"\n", "/project", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := loadCacheConfig(t, tc.source)
			p, err := c.PolicyFor(tc.project)
			if err != nil {
				t.Fatal(err)
			}
			if p.MetadataCacheTTL != tc.want {
				t.Fatalf("cache TTL = %v, want %v", p.MetadataCacheTTL, tc.want)
			}
		})
	}
}

func TestMetadataCacheTTLValidation(t *testing.T) {
	for _, value := range []string{"", "invalid", "-1s", "500ms", "1.5s", "61s", "1h"} {
		for _, scope := range []string{"global", "project"} {
			t.Run(scope+"/"+value, func(t *testing.T) {
				table := "fs"
				if scope == "project" {
					table = `project."/project".fs`
				}
				c := loadCacheConfig(t, fmt.Sprintf("[%s]\nmetadata_cache_ttl = %q\n", table, value))
				if _, err := c.PolicyFor("/project"); err == nil || !strings.Contains(err.Error(), "fs.metadata_cache_ttl") {
					t.Fatalf("expected fs.metadata_cache_ttl validation error for %q, got %v", value, err)
				}
			})
		}
	}
}

func TestMetadataCacheTTLProjectHomeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	c := loadCacheConfig(t, "[project.\"~/work/shop\".fs]\nmetadata_cache_ttl = \"0s\"\n")
	p, err := c.PolicyFor(filepath.Join(home, "work", "shop"))
	if err != nil || p.MetadataCacheTTL != 0 {
		t.Fatalf("expanded project override: %+v, error %v", p, err)
	}
}
