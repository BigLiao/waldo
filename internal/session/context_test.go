package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"waldo/internal/config"
	"waldo/internal/proto"
)

func TestContextGuideUsesEffectivePolicy(t *testing.T) {
	d := t.TempDir()
	t.Setenv("WALDO_CONFIG_DIR", d)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-appear-in-guide")
	os.WriteFile(filepath.Join(d, "allowed.json"), []byte(`{"*":["global.example"],"/project":["local.example"]}`), 0600)
	p := &config.Policy{Project: "/project", Allow: []string{"packages.example"}, HostPorts: []int{5432}, Ports: config.PortsMode{Mode: "list", List: []int{3000}}, ExposeRO: []string{"/toolchain"}}
	got := string(contextGuide(p, "remote-host", "test-agent", "/project/sub", []string{"/project", "/tmp", proto.ContextDir}))
	for _, want := range []string{"remote-host", "test-agent", "/project/sub", "packages.example", "global.example", "local.example", "5432", "3000", "/toolchain", proto.ContextFile} {
		if !strings.Contains(got, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if strings.Contains(got, "must-not-appear-in-guide") {
		t.Fatal("guide leaked credentials")
	}
}

func TestContextGuideDescribesPullValidatedCache(t *testing.T) {
	p := &config.Policy{Project: "/project", ContentCacheTTL: 10 * time.Second}
	guide := string(contextGuide(p, "B", "test", "/project", []string{"/project"}))
	if !strings.Contains(guide, "Every read-only open validates that file on A") ||
		!strings.Contains(guide, "no change notifications") {
		t.Fatal("guide does not describe pull-based validation")
	}
}

func TestContextGuideRemoteModeExplainsBothShells(t *testing.T) {
	t.Setenv("WALDO_CONFIG_DIR", t.TempDir())
	p := &config.Policy{Project: "/project", ExecMode: proto.ExecRemote}
	guide := string(contextGuide(p, "B", "test", "/project", []string{"/project"}))
	for _, want := range []string{"内置 Bash", "mcp__waldo__Bash", "未命中的读取仍需访问 A", "修改真实工作区", "虚拟环境", "localhost 指 B", "现有本机沙箱"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("remote guide lacks %q", want)
		}
	}
	if strings.Contains(guide, "Commands launched through PATH tools") {
		t.Fatal("remote guide still says PATH tools forward to A")
	}
}

func TestHostMCPGuide(t *testing.T) {
	t.Setenv("WALDO_CONFIG_DIR", t.TempDir())
	guide := string(contextGuide(&config.Policy{Project: "/project", ExecMode: proto.ExecHostMCP}, "B", "claude", "/project", nil))
	for _, want := range []string{"NotebookEdit 工具已禁用", "mcp__waldo__Bash", "每次调用独立", "Read"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(guide, "优先使用内置 Bash") {
		t.Fatal("contradictory instructions")
	}
}

func TestHostMCPGuideShortMetadataCache(t *testing.T) {
	t.Setenv("WALDO_CONFIG_DIR", t.TempDir())
	p := &config.Policy{Project: "/project", ExecMode: proto.ExecHostMCP, MetadataCacheTTL: 10 * time.Second, ContentCacheTTL: 10 * time.Second}
	guide := string(contextGuide(p, "B", "claude", "/project", nil))
	for _, want := range []string{"2s for attributes/path entries", "1s for missing paths", "MCP file tools on A bypass these caches"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("missing %q", want)
		}
	}
	p.MetadataCacheTTL = 0
	if strings.Contains(string(contextGuide(p, "B", "claude", "/project", nil)), "uses a short metadata cache") {
		t.Fatal("advertises disabled cache")
	}
}
