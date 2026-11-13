// Package profile holds the declarative agent profiles. The rest of waldo
// never mentions an agent by name.
package profile

import (
	_ "embed"
	"fmt"
	"sort"

	"github.com/BurntSushi/toml"

	"waldo/internal/proto"
)

//go:embed profiles.toml
var builtin string

// Profile describes how to run one agent.
type Profile struct {
	Telemetry     string            `toml:"telemetry"` // claude | codex | none; opt-in local model counters
	Name          string            `toml:"-"`
	Bin           string            `toml:"bin"`
	Args          []string          `toml:"args"`
	Env           map[string]string `toml:"env"`
	Shared        []string          `toml:"shared"`          // written by the agent, read by commands -> projected to A
	Scrub         []string          `toml:"scrub"`           // never forwarded with commands to A
	Needs         []string          `toml:"needs"`           // commands doctor checks on A
	State         []string          `toml:"state"`           // agent state under HOME on B (login check, --adopt-login)
	MCPConfigFlag string            `toml:"mcp_config_flag"` // inline JSON MCP configuration argument
	SkillDir      string            `toml:"skill_dir"`       // native skill directory, home-relative
}

type file struct {
	Agent map[string]Profile `toml:"agent"`
}

// Builtin returns the embedded profiles.
func Builtin() map[string]Profile {
	var f file
	if _, err := toml.Decode(builtin, &f); err != nil {
		panic(fmt.Sprintf("embedded profiles.toml: %v", err))
	}
	for k, p := range f.Agent {
		p.Name = k
		f.Agent[k] = p
	}
	return f.Agent
}

// Merge overlays user profiles on top of the built-in ones. Fields set by the
// user replace the built-in value; unset fields are inherited.
func Merge(user map[string]Profile) map[string]Profile {
	out := Builtin()
	for k, u := range user {
		b, ok := out[k]
		if !ok {
			u.Name = k
			out[k] = u
			continue
		}
		if u.Bin != "" {
			b.Bin = u.Bin
		}
		if u.Telemetry != "" {
			b.Telemetry = u.Telemetry
		}
		if u.Args != nil {
			b.Args = u.Args
		}
		if u.Shared != nil {
			b.Shared = u.Shared
		}
		if u.Scrub != nil {
			b.Scrub = u.Scrub
		}
		if u.Needs != nil {
			b.Needs = u.Needs
		}
		if u.State != nil {
			b.State = u.State
		}
		if u.MCPConfigFlag != "" {
			b.MCPConfigFlag = u.MCPConfigFlag
		}
		if u.SkillDir != "" {
			b.SkillDir = u.SkillDir
		}
		if u.Env != nil {
			if b.Env == nil {
				b.Env = map[string]string{}
			}
			for ek, ev := range u.Env {
				b.Env[ek] = ev
			}
		}
		out[k] = b
	}
	return out
}

// Names returns the sorted profile names.
func Names(m map[string]Profile) []string {
	var n []string
	for k := range m {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Wire converts a profile to its wire form.
func (p Profile) Wire() proto.Profile {
	return proto.Profile{Name: p.Name, Bin: p.Bin, Args: p.Args, Env: p.Env, Shared: p.Shared, Scrub: p.Scrub, State: p.State, Telemetry: p.Telemetry}
}

// Empty returns the profile used by `waldo run`.
func Empty(bin string) Profile { return Profile{Name: "run", Bin: bin} }
