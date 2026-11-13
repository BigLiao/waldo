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

func TestContextGuideDescribesCacheAsConditional(t *testing.T) {
	p := &config.Policy{Project: "/project", ContentCacheTTL: 10 * time.Second}
	guide := string(contextGuide(p, "B", "test", "/project", []string{"/project"}))
	if !strings.Contains(guide, "only when the proxy and project-change monitoring are working") ||
		!strings.Contains(guide, "otherwise reads pass through to A") {
		t.Fatal("guide claims a configured cache is already active")
	}
}
