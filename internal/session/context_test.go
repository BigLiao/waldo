package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
