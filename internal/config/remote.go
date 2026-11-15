package config

import (
	"fmt"
	"strings"
)

// RemoteCfg selects a machine, whose SSH user owns its agent login and state.
// Nil ssh_args inherits the global setting; an explicit [] clears it.
type RemoteCfg struct {
	Host      string   `toml:"host"`
	SSHArgs   []string `toml:"ssh_args"`
	RemoteDir string   `toml:"remote_dir"`
}

func ValidateRemoteName(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return fmt.Errorf("远端名称应为 1–64 个字母、数字、点、下划线或连字符")
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && strings.ContainsRune("._-", c) {
			continue
		}
		return fmt.Errorf("无效的远端名称 %q", name)
	}
	return nil
}

// ResolveRemote returns a copy so another concurrent session cannot change
// this session's transport. Explicit selection wins over default_remote; the
// legacy host is used when neither names a remote. A single named remote is
// implicit only without a legacy host. Never fall back after selection errors.
func (c *Config) ResolveRemote(name string) (*Config, string, error) {
	if name == "" {
		name = c.DefaultRemote
		if name == "" && strings.TrimSpace(c.Host) == "" && len(c.Remote) == 1 {
			for only := range c.Remote {
				name = only
			}
		}
	}
	resolved := *c
	if name != "" {
		if err := ValidateRemoteName(name); err != nil {
			return nil, "", err
		}
		r, ok := c.Remote[name]
		if !ok {
			return nil, "", fmt.Errorf("未配置远端 %q，请在 %s 添加 [remote.%s]", name, Path(), name)
		}
		resolved.Host = r.Host
		if r.SSHArgs != nil {
			resolved.SSHArgs = append([]string{}, r.SSHArgs...)
		}
		if r.RemoteDir != "" {
			resolved.RemoteDir = r.RemoteDir
		}
	}
	if strings.TrimSpace(resolved.Host) == "" {
		if name == "" && len(c.Remote) > 1 {
			return nil, "", fmt.Errorf("配置了多台远端，请设置 default_remote 或使用 --remote <名称>")
		}
		return nil, "", fmt.Errorf("未配置服务器地址：设置 host 或 default_remote / [remote.<名称>].host")
	}
	resolved.SSHArgs = append([]string(nil), resolved.SSHArgs...)
	return &resolved, name, nil
}
