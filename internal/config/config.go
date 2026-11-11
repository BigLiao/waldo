// Package config loads ~/.config/waldo/config.toml and the per-project
// allow list written by `waldo allow`. Everything here lives on A's host and
// is never visible inside the sandbox.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"waldo/internal/profile"
	"waldo/internal/util"
)

// ConfigDir is where the user's policy lives.
func ConfigDir() string {
	if d := os.Getenv("WALDO_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "waldo")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "waldo")
}

// DataDir is where waldo keeps state on this machine (sandbox HOME, audit
// log, run state; on B also the installed binary and agent HOME).
func DataDir() string {
	if d := os.Getenv("WALDO_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "waldo")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "waldo")
}

type SandboxCfg struct {
	ExposeRO []string `toml:"expose_ro"`
	PassEnv  []string `toml:"pass_env"` // extra env vars (glob patterns) passed to sandbox and agent
}

type ContextCfg struct {
	Enabled          *bool    `toml:"enabled"`
	InstructionFiles []string `toml:"instruction_files"`
}

type FSCfg struct {
	MetadataCacheTTL *string `toml:"metadata_cache_ttl"`
}

type NetCfg struct {
	Allow        []string `toml:"allow"`
	Ports        any      `toml:"ports"` // "auto" | "off" | [3000, 5173]
	HostPorts    []int    `toml:"host_ports"`
	AllowPrivate *bool    `toml:"allow_private"`
}

type ProjectCfg struct {
	Net     NetCfg     `toml:"net"`
	Sandbox SandboxCfg `toml:"sandbox"`
	Context ContextCfg `toml:"context"`
	FS      FSCfg      `toml:"fs"`
}

type Config struct {
	Host      string                     `toml:"host"`
	SSHArgs   []string                   `toml:"ssh_args"`
	RemoteDir string                     `toml:"remote_dir"`
	Sandbox   SandboxCfg                 `toml:"sandbox"`
	Context   ContextCfg                 `toml:"context"`
	FS        FSCfg                      `toml:"fs"`
	Net       NetCfg                     `toml:"net"`
	Project   map[string]ProjectCfg      `toml:"project"`
	Agent     map[string]profile.Profile `toml:"agent"`
}

// Path of the main config file.
func Path() string { return filepath.Join(ConfigDir(), "config.toml") }

// Load reads the config file; a missing file yields the zero config.
func Load() (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if _, err := toml.Decode(string(b), c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	return c, nil
}

// RemoteDirOrDefault is the data directory on B ("~" is expanded remotely).
func (c *Config) RemoteDirOrDefault() string {
	if c.RemoteDir != "" {
		return c.RemoteDir
	}
	return "~/.local/share/waldo"
}

// Profiles returns built-in profiles overlaid with the user's.
func (c *Config) Profiles() map[string]profile.Profile { return profile.Merge(c.Agent) }

// PortsMode is the parsed form of net.ports.
type PortsMode struct {
	Mode string // auto | off | list
	List []int
}

// Policy is the effective policy for one project.
type Policy struct {
	Project          string
	Allow            []string // static allow list (defaults + config)
	Ports            PortsMode
	HostPorts        []int
	AllowPrivate     bool
	ExposeRO         []string
	PassEnv          []string
	ContextEnabled   bool
	InstructionFiles []string
	MetadataCacheTTL time.Duration
}

// DefaultAllow lists common package registries and GitHub.
var DefaultAllow = []string{
	"registry.npmjs.org", "registry.yarnpkg.com", "registry.npmmirror.com",
	"pypi.org", "files.pythonhosted.org",
	"github.com", "*.github.com", "*.githubusercontent.com",
	"proxy.golang.org", "sum.golang.org", "goproxy.cn",
	"crates.io", "index.crates.io", "static.crates.io",
	"rubygems.org", "index.rubygems.org",
	"repo.maven.apache.org", "repo1.maven.org",
	"nodejs.org", "deno.land", "jsr.io",
}

func parsePorts(v any) (PortsMode, error) {
	switch t := v.(type) {
	case nil:
		return PortsMode{}, nil
	case string:
		switch t {
		case "auto", "off":
			return PortsMode{Mode: t}, nil
		}
		return PortsMode{}, fmt.Errorf("net.ports: want \"auto\", \"off\" or a list, got %q", t)
	case []any:
		pm := PortsMode{Mode: "list"}
		for _, e := range t {
			n, ok := e.(int64)
			if !ok || n < 1 || n > 65535 {
				return PortsMode{}, fmt.Errorf("net.ports: bad port %v", e)
			}
			pm.List = append(pm.List, int(n))
		}
		return pm, nil
	}
	return PortsMode{}, fmt.Errorf("net.ports: unsupported value %v", v)
}

func parseMetadataCacheTTL(raw string) (time.Duration, error) {
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("fs.metadata_cache_ttl: %w", err)
	}
	if ttl < 0 || ttl > time.Minute || ttl%time.Second != 0 {
		return 0, fmt.Errorf("fs.metadata_cache_ttl: expected a whole-second duration between 0s and 1m, got %q", raw)
	}
	return ttl, nil
}

// PolicyFor computes the policy for a project directory.
func (c *Config) PolicyFor(project string) (*Policy, error) {
	home, _ := os.UserHomeDir()
	p := &Policy{Project: project, Ports: PortsMode{Mode: "auto"}, ContextEnabled: true,
		InstructionFiles: []string{"AGENTS.md", "CLAUDE.md"}, MetadataCacheTTL: 10 * time.Second}
	applyContext := func(c ContextCfg) {
		if c.Enabled != nil {
			p.ContextEnabled = *c.Enabled
		}
		if c.InstructionFiles != nil {
			p.InstructionFiles = append([]string{}, c.InstructionFiles...)
		}
	}
	applyContext(c.Context)
	if c.FS.MetadataCacheTTL != nil {
		ttl, err := parseMetadataCacheTTL(*c.FS.MetadataCacheTTL)
		if err != nil {
			return nil, err
		}
		p.MetadataCacheTTL = ttl
	}
	p.Allow = append(p.Allow, DefaultAllow...)
	p.Allow = append(p.Allow, c.Net.Allow...)
	pm, err := parsePorts(c.Net.Ports)
	if err != nil {
		return nil, err
	}
	if pm.Mode != "" {
		p.Ports = pm
	}
	p.HostPorts = append(p.HostPorts, c.Net.HostPorts...)
	if c.Net.AllowPrivate != nil {
		p.AllowPrivate = *c.Net.AllowPrivate
	}
	p.ExposeRO = append(p.ExposeRO, c.Sandbox.ExposeRO...)
	p.PassEnv = append(p.PassEnv, c.Sandbox.PassEnv...)
	for k, pc := range c.Project {
		if filepath.Clean(util.ExpandHome(k, home)) != project {
			continue
		}
		p.Allow = append(p.Allow, pc.Net.Allow...)
		pm, err := parsePorts(pc.Net.Ports)
		if err != nil {
			return nil, err
		}
		if pm.Mode != "" {
			p.Ports = pm
		}
		if pc.Net.HostPorts != nil {
			p.HostPorts = pc.Net.HostPorts
		}
		if pc.Net.AllowPrivate != nil {
			p.AllowPrivate = *pc.Net.AllowPrivate
		}
		p.ExposeRO = append(p.ExposeRO, pc.Sandbox.ExposeRO...)
		p.PassEnv = append(p.PassEnv, pc.Sandbox.PassEnv...)
		applyContext(pc.Context)
		if pc.FS.MetadataCacheTTL != nil {
			ttl, err := parseMetadataCacheTTL(*pc.FS.MetadataCacheTTL)
			if err != nil {
				return nil, err
			}
			p.MetadataCacheTTL = ttl
		}
	}
	seenInstructions := map[string]bool{}
	var names []string
	for _, name := range p.InstructionFiles {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\\\x00\r\n") {
			return nil, fmt.Errorf("context.instruction_files: expected a project-root filename, got %q", name)
		}
		if !seenInstructions[name] {
			names = append(names, name)
			seenInstructions[name] = true
		}
	}
	p.InstructionFiles = names
	for i, e := range p.ExposeRO {
		p.ExposeRO[i] = filepath.Clean(util.ExpandHome(e, home))
	}
	return p, nil
}

// ---- dynamic allow list (`waldo allow`) ----

// AllowedPath is the file `waldo allow` writes.
func AllowedPath() string { return filepath.Join(ConfigDir(), "allowed.json") }

// Allowed maps a project path (or "*" for all projects) to extra domains.
type Allowed map[string][]string

func LoadAllowed() (Allowed, error) {
	a := Allowed{}
	b, err := os.ReadFile(AllowedPath())
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", AllowedPath(), err)
	}
	return a, nil
}

// AddAllowed records domain for project ("*" = every project).
func AddAllowed(project, domain string) error {
	a, err := LoadAllowed()
	if err != nil {
		return err
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, d := range a[project] {
		if d == domain {
			return nil
		}
	}
	a[project] = append(a[project], domain)
	sort.Strings(a[project])
	b, _ := json.MarshalIndent(a, "", "  ")
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	tmp := AllowedPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, AllowedPath())
}

// SetHost writes `host = "..."` into config.toml, creating it if needed.
func SetHost(host string) error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	line := fmt.Sprintf("host = %q", host)
	b, err := os.ReadFile(Path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out string
	if len(b) == 0 {
		out = line + "                     # SSH target (B)\n\n" +
			"[net]\n# allow = [\"api.example.com\"]   # extra outbound domains\n# ports = \"auto\"\n"
	} else {
		lines := strings.Split(string(b), "\n")
		replaced := false
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "[") {
				break
			}
			if strings.HasPrefix(t, "host") && strings.Contains(t, "=") {
				k, _, _ := strings.Cut(t, "=")
				if strings.TrimSpace(k) == "host" {
					lines[i] = line
					replaced = true
					break
				}
			}
		}
		if !replaced {
			lines = append([]string{line}, lines...)
		}
		out = strings.Join(lines, "\n")
	}
	return os.WriteFile(Path(), []byte(out), 0o600)
}
