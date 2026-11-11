package config

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestContextPolicy(t *testing.T) {
	c := &Config{}
	p, err := c.PolicyFor("/project")
	if err != nil {
		t.Fatal(err)
	}
	if !p.ContextEnabled || !reflect.DeepEqual(p.InstructionFiles, []string{"AGENTS.md", "CLAUDE.md"}) {
		t.Fatalf("defaults: %+v", p)
	}
	_, err = toml.Decode(`[context]
enabled = false
instruction_files = ["GEMINI.md", "GEMINI.md"]
[project."/project".context]
enabled = true
instruction_files = []
`, c)
	if err != nil {
		t.Fatal(err)
	}
	p, err = c.PolicyFor("/project")
	if err != nil || !p.ContextEnabled || len(p.InstructionFiles) != 0 {
		t.Fatalf("project override: %+v %v", p, err)
	}
	p, err = c.PolicyFor("/other")
	if err != nil || p.ContextEnabled || !reflect.DeepEqual(p.InstructionFiles, []string{"GEMINI.md"}) {
		t.Fatalf("global: %+v %v", p, err)
	}
	for _, name := range []string{"", "..", "../AGENTS.md", "/AGENTS.md", "nested/AGENTS.md"} {
		c.Context.InstructionFiles = []string{name}
		if _, err = c.PolicyFor("/other"); err == nil {
			t.Errorf("invalid name accepted: %q", name)
		}
	}
}
