package session

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"waldo/internal/netpolicy"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/util"
)

type nativeRuntime struct {
	sources map[string]string
	paths   map[string]string
	tmp     string
	root    string
	proxy   *netpolicy.HTTPProxy
	env     []string
}

func (n *nativeRuntime) Close() {
	if n.proxy != nil {
		n.proxy.Close()
	}
	os.RemoveAll(n.root)
}

func prepareNative(o *SandboxOptions, spec *sandbox.Spec, base *[]string, policy *netpolicy.Policy) (_ *nativeRuntime, err error) {
	n := &nativeRuntime{paths: map[string]string{}, sources: map[string]string{}}
	n.root, err = os.MkdirTemp(o.DataDir, "runtime-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			n.Close()
		}
	}()
	root, err := filepath.EvalSymlinks(n.root)
	if err != nil {
		return nil, err
	}
	n.root = root
	n.tmp = filepath.Join(n.root, "tmp")
	if err = os.MkdirAll(n.tmp, 0700); err != nil {
		return nil, err
	}
	n.paths["/tmp"] = n.tmp
	if spec.ContextDir != "" {
		n.paths[proto.ContextDir] = spec.ContextDir
	}
	if !o.Unsandboxed {
		spec.SandboxHome, err = filepath.EvalSymlinks(spec.SandboxHome)
		if err != nil {
			return nil, err
		}
		for i, p := range o.Mkdirs {
			rel, e := filepath.Rel(o.Home, p)
			if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return nil, fmt.Errorf("invalid shared HOME path %q", p)
			}
			dest := filepath.Join(spec.SandboxHome, rel)
			n.paths[p], o.Mkdirs[i] = dest, dest
		}
	}
	for i, m := range spec.ReadOnlyMounts {
		dest := filepath.Join(n.root, "skills", fmt.Sprint(i))
		if err = copySkill(m.Source, dest); err != nil {
			return nil, err
		}
		rel, e := filepath.Rel(o.Home, m.Target)
		if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("invalid skill target %q", m.Target)
		}
		target := m.Target
		if !o.Unsandboxed {
			target = filepath.Join(spec.SandboxHome, rel)
		}
		n.paths[m.Target] = target
		n.sources[target] = dest
		spec.ReadOnlyMounts[i] = sandbox.ReadOnlyMount{Source: dest, Target: dest}
	}
	if !o.Unsandboxed {
		n.proxy, err = netpolicy.StartHTTPProxy(policy)
		if err != nil {
			return nil, err
		}
		spec.TempDir, spec.ProxyPort, spec.HostPorts = n.tmp, n.proxy.Port, o.Policy.HostPorts
		switch o.Policy.Ports.Mode {
		case "list":
			spec.ListenPorts = append([]int(nil), o.Policy.Ports.List...)
		case "off":
		default:
			fmt.Fprintln(os.Stderr, "waldo: macOS 自动端口发布不可用；开发服务请配置 [net] ports = [3000, 5173]")
		}
		*base = util.EnvSet(*base, "HOME", spec.SandboxHome)
		// Proxy credentials stay on A.
		n.env = proxyEnvURL(n.proxy.URL)
		for _, key := range []string{"CLANG_MODULE_CACHE_PATH", "SWIFT_MODULECACHE_PATH"} {
			*base = util.EnvSet(*base, key, filepath.Join(spec.SandboxHome, ".cache", key))
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		*base = util.EnvSet(*base, key, n.tmp)
		n.env = append(n.env, key+"="+n.tmp)
	}
	home, _ := util.EnvGet(*base, "HOME")
	n.env = append(n.env, "HOME="+home)
	if len(n.sources) > 0 {
		mappings, _ := json.Marshal(n.sources)
		if len(mappings) > 65536 {
			return nil, fmt.Errorf("too many native skill aliases")
		}
		n.env = append(n.env, "WALDO_FILE_ALIASES="+string(mappings))
	}
	spec.Env = append([]string(nil), (*base)...)
	if spec.ContextDir != "" {
		guide := string(o.ContextGuide)
		guide = strings.ReplaceAll(guide, proto.ContextFile, filepath.Join(spec.ContextDir, "waldo.md"))
		for from, to := range n.paths {
			guide = strings.ReplaceAll(guide, fmt.Sprintf("%q", from), fmt.Sprintf("%q", to))
		}
		for target, source := range n.sources {
			guide += fmt.Sprintf("\nRead-only skill: B path %q; A helper commands use %q. File tools translate this alias automatically.\n", target, source)
		}
		if o.Unsandboxed {
			guide += "\nA uses its real HOME and a session-specific TMPDIR. B /tmp is separate; use the projected temporary path when sharing files. Detached daemons may outlive session cleanup.\n"
		} else {
			guide += "\nmacOS native sandbox: HOME and TMPDIR are real private paths. B /tmp is separate; use the advertised shared tmp path. DNS is resolved by the authenticated proxy. Development listeners require explicit configured ports. Detached daemons may outlive session cleanup while retaining sandbox restrictions.\n"
		}
		err = os.WriteFile(filepath.Join(spec.ContextDir, "waldo.md"), []byte(guide), 0444)
		if err != nil {
			return nil, err
		}
	}
	return n, nil
}

func copySkill(source, dest string) error {
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill contains non-regular file: %s", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600|info.Mode().Perm()&0111)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
