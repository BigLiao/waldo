package host

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"waldo/internal/proto"
)

func TestProjectMetadataCacheScope(t *testing.T) {
	for _, tc := range []struct {
		project, mount string
		want           bool
	}{
		{"/work/project", "/work/project", true},
		{"/tmp/test-project", "/tmp/test-project", true},
		{"/work/project", "/work/project/../project", true},
		{"/work/project", "/tmp", false},
		{"/tmp", "/tmp", false},
		{proto.ContextDir, proto.ContextDir, false},
		{"/work/project", "/home/user/.claude/shell-snapshots", false},
		{"/work/project", "/work/other", false},
	} {
		if got := projectMetadataCacheable(tc.project, tc.mount); got != tc.want {
			t.Errorf("project=%q mount=%q cacheable=%v want %v", tc.project, tc.mount, got, tc.want)
		}
	}
}

func TestSSHFSCacheOptions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ttl       time.Duration
		cacheable bool
		wantTTL   string
	}{
		{"project-default", 10 * time.Second, true, "10"},
		{"project-configured", 5 * time.Second, true, "5"},
		{"project-maximum", time.Minute, true, "60"},
		{"project-disabled", 0, true, "0"},
		{"tmp-uncached", time.Second, false, "0"},
		{"shared-uncached", time.Minute, false, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := sshfsCacheOptions(tc.ttl, tc.cacheable)
			values := make(map[string]string, len(options))
			for _, option := range options {
				key, value, _ := strings.Cut(option, "=")
				if _, exists := values[key]; exists {
					t.Fatalf("duplicate mount option %q in %q", key, options)
				}
				values[key] = value
			}
			for key, want := range map[string]string{
				"dir_cache":        "no",
				"negative_timeout": "0",
				"attr_timeout":     tc.wantTTL,
				"entry_timeout":    tc.wantTTL,
			} {
				if got := values[key]; got != want {
					t.Errorf("%s = %q, want %q (options %q)", key, got, want, options)
				}
			}
			// Only kernel metadata caching is enabled. Directory-listing and
			// file-data caches would add independent staleness guarantees.
			for key := range values {
				switch key {
				case "dir_cache", "negative_timeout", "attr_timeout", "entry_timeout":
				default:
					t.Errorf("unexpected cache behavior %q in %q", key, options)
				}
			}
			for _, key := range []string{"attr_timeout", "entry_timeout", "negative_timeout"} {
				if _, err := strconv.Atoi(values[key]); err != nil {
					t.Errorf("%s must be an integer number of seconds: %q", key, values[key])
				}
			}
		})
	}
}

func TestMountCachePolicyByModeAndScope(t *testing.T) {
	for _, tc := range []struct {
		name, mode, mount     string
		configured            int64
		proxy                 bool
		wantTTL, wantNegative time.Duration
	}{
		{"host-proxy", proto.ExecHost, "/project", 10, true, 0, 0},
		{"remote-proxy", proto.ExecRemote, "/project", 10, true, 0, 0},
		{"host-direct", proto.ExecHost, "/project", 10, false, 10 * time.Second, 0},
		{"remote-direct", proto.ExecRemote, "/project", 10, false, 10 * time.Second, 0},
		{"mcp-proxy", proto.ExecHostMCP, "/project", 10, true, 2 * time.Second, time.Second},
		{"mcp-direct", proto.ExecHostMCP, "/project", 10, false, 2 * time.Second, time.Second},
		{"mcp-shorter-config", proto.ExecHostMCP, "/project", 1, true, time.Second, time.Second},
		{"mcp-disabled", proto.ExecHostMCP, "/project", 0, true, 0, 0},
		{"mcp-readonly-skill", proto.ExecHostMCP, "/home/agent/.claude/skills/paseo", 10, false, 2 * time.Second, time.Second},
		{"mcp-readonly-disabled", proto.ExecHostMCP, "/home/agent/.claude/skills/paseo", 0, false, 0, 0},
		{"host-readonly-skill", proto.ExecHost, "/home/agent/.claude/skills/paseo", 10, false, 0, 0},
		{"mcp-runtime", proto.ExecHostMCP, "/home/agent/.claude/shell-snapshots", 10, false, 0, 0},
		{"mcp-guide", proto.ExecHostMCP, proto.ContextDir, 10, false, 0, 0},
		{"mcp-tmp", proto.ExecHostMCP, "/tmp", 10, false, 0, 0},
		{"mcp-path-boundary", proto.ExecHostMCP, "/home/agent/.claude/skills/paseo-other", 10, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hello := proto.Hello{Project: "/project", ExecMode: tc.mode, MetadataCacheTTLSeconds: tc.configured, ReadOnlyMounts: []string{"/home/agent/.claude/skills/paseo", proto.ContextDir, "/tmp"}}
			opts, ttl, negative := sshfsMountCacheOptions(hello, tc.mount, tc.proxy)
			if ttl != tc.wantTTL || negative != tc.wantNegative {
				t.Fatalf("ttl=%s negative=%s", ttl, negative)
			}
			values := map[string]string{}
			for _, opt := range opts {
				k, v, _ := strings.Cut(opt, "=")
				if _, exists := values[k]; exists {
					t.Fatalf("duplicate %s", k)
				}
				values[k] = v
			}
			wantTTL := strconv.Itoa(int(tc.wantTTL / time.Second))
			wantNegative := strconv.Itoa(int(tc.wantNegative / time.Second))
			if values["attr_timeout"] != wantTTL || values["entry_timeout"] != wantTTL || values["negative_timeout"] != wantNegative || values["dir_cache"] != "no" {
				t.Fatal(opts)
			}
			if _, ok := values["direct_io"]; ok != tc.proxy {
				t.Fatalf("page caching changed: %v", opts)
			}
		})
	}
}
