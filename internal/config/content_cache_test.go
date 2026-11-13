package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestContentCachePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, source, project string
		want                  time.Duration
	}{
		{"default", "", "/project", 10 * time.Second},
		{"global", "[fs]\ncontent_cache_ttl = \"30s\"", "/project", 30 * time.Second},
		{"disabled", "[fs]\ncontent_cache_ttl = \"0s\"", "/project", 0},
		{"project", "[fs]\ncontent_cache_ttl = \"30s\"\n[project.\"/project\".fs]\ncontent_cache_ttl = \"0s\"", "/project", 0},
		{"other project", "[project.\"/other\".fs]\ncontent_cache_ttl = \"0s\"", "/project", 10 * time.Second},
		{"legacy global bypass", "[fs]\nmetadata_cache_ttl = \"0s\"", "/project", 0},
		{"legacy project bypass", "[project.\"/project\".fs]\nmetadata_cache_ttl = \"0s\"", "/project", 0},
		{"explicit content overrides legacy bypass", "[fs]\nmetadata_cache_ttl = \"0s\"\ncontent_cache_ttl = \"10s\"", "/project", 10 * time.Second},
		{"explicit global content is inherited", "[fs]\ncontent_cache_ttl = \"10s\"\n[project.\"/project\".fs]\nmetadata_cache_ttl = \"0s\"", "/project", 10 * time.Second},
		{"explicit project content overrides legacy bypass", "[fs]\nmetadata_cache_ttl = \"0s\"\n[project.\"/project\".fs]\ncontent_cache_ttl = \"30s\"", "/project", 30 * time.Second},
		{"unrelated legacy project does not disable cache", "[project.\"/other\".fs]\nmetadata_cache_ttl = \"0s\"", "/project", 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := loadCacheConfig(t, tc.source).PolicyFor(tc.project)
			if err != nil {
				t.Fatal(err)
			}
			if p.ContentCacheTTL != tc.want {
				t.Fatalf("got %s, want %s", p.ContentCacheTTL, tc.want)
			}
		})
	}
}

func TestContentCacheValidation(t *testing.T) {
	for _, value := range []string{"", "invalid", "-1s", "500ms", "61s"} {
		for _, table := range []string{"fs", `project."/project".fs`} {
			c := loadCacheConfig(t, fmt.Sprintf("[%s]\ncontent_cache_ttl = %q", table, value))
			if _, err := c.PolicyFor("/project"); err == nil || !strings.Contains(err.Error(), "fs.content_cache_ttl") {
				t.Fatalf("%s/%q: %v", table, value, err)
			}
		}
	}
}
