// Package cli implements the user-facing commands that run on A.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"waldo/internal/audit"
	"waldo/internal/bootstrap"
	"waldo/internal/config"
	"waldo/internal/host"
	"waldo/internal/profile"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/session"
	"waldo/internal/termfilter"
)

const usage = `waldo — Agent 跑在你的服务器上，它的"手"落在这台机器的沙箱里

用法:
  waldo setup <user@host> [--adopt-login=false] [--skip-deps]
                                             默认复用 B 的配置和登录，自动补齐依赖并检查
  waldo <agent> [--paseo] [参数...]           运行 Agent（内置: %s）；--paseo 桥接 Paseo MCP 和 skills
  waldo run [--] <命令> [参数...]             用空 profile 运行任意 CLI Agent
  waldo doctor                                检查两端依赖、时钟偏差、Agent 安装/登录
  waldo allow [--global] <域名>               给当前项目放行一个出网域名
  waldo ports                                 查看当前转发的端口
  waldo log [-n N] [-f]                       查看本机审计日志
  waldo link [--remove] <agent>               在 PATH 前放一个 <agent> → waldo <agent> 的包装
  waldo version

配置: %s
`

func loadConfig() (*config.Config, int) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return nil, 2
	}
	return cfg, 0
}

// Main dispatches A-side commands. args excludes the program name.
func Main(args []string) int {
	args, withPaseo := paseoPrefix(args)
	cfg, code := loadConfig()
	if cfg == nil {
		return code
	}
	profiles := cfg.Profiles()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, usage, strings.Join(profile.Names(profiles), ", "), config.Path())
		return 2
	}
	if withPaseo {
		if _, ok := profiles[args[0]]; !ok {
			fmt.Fprintln(os.Stderr, "waldo: --paseo 只用于已配置的 Agent")
			return 2
		}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Printf(usage, strings.Join(profile.Names(profiles), ", "), config.Path())
		return 0
	case "version", "--version":
		fmt.Println(proto.FullVersion())
		return 0
	case "setup":
		return setup(cfg, args[1:])
	case "doctor":
		return doctor(cfg)
	case "allow":
		return allow(args[1:])
	case "ports":
		return ports()
	case "log":
		return logCmd(args[1:])
	case "link":
		return link(args[1:])
	case "dev-sandbox":
		a := args[1:]
		if len(a) > 0 && a[0] == "--" {
			a = a[1:]
		}
		return session.DevSandbox(cfg, a)
	case "run":
		a := args[1:]
		if len(a) > 0 && a[0] == "--" {
			a = a[1:]
		}
		if len(a) == 0 {
			fmt.Fprintln(os.Stderr, "用法: waldo run [--] <命令> [参数...]")
			return 2
		}
		return session.Run(session.Options{Config: cfg, Profile: profile.Empty(a[0]), Args: a[1:]})
	}
	if p, ok := profiles[args[0]]; ok {
		agentArgs, enabled := paseoPrefix(args[1:])
		return session.Run(session.Options{Config: cfg, Profile: p, Args: agentArgs, Paseo: withPaseo || enabled})
	}
	fmt.Fprintf(os.Stderr, "waldo: 未知命令或 Agent %q（waldo help 查看用法；任意命令请用 waldo run -- %s）\n", args[0], args[0])
	return 2
}

// Only consume a wrapper flag before agent arguments. A literal --paseo
// supplied after -p or -- remains an agent prompt, rather than being removed.
func paseoPrefix(args []string) ([]string, bool) {
	enabled := false
	for len(args) > 0 && args[0] == "--paseo" {
		enabled = true
		args = args[1:]
	}
	return args, enabled
}

func setup(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	adopt := fs.Bool("adopt-login", true, "复用 B 上已有的 Agent 配置和登录；设为 false 停止共享已有目录")
	skipDeps := fs.Bool("skip-deps", false, "跳过自动安装系统依赖（仍运行 doctor）")
	var hostArg string
	var rest []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && hostArg == "" {
			hostArg = a
		} else {
			rest = append(rest, a)
		}
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "waldo: setup 只接受一个服务器地址")
		return 2
	}
	if hostArg == "" {
		hostArg = cfg.Host
	}
	if hostArg == "" {
		fmt.Fprintln(os.Stderr, "用法: waldo setup <user@host> [--adopt-login=false] [--skip-deps]")
		return 2
	}
	if !*skipDeps {
		fmt.Println("A（本机）：检查并安装缺少的系统依赖…")
		if err := bootstrap.Install("a", profileNeeds(cfg.Profiles()), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: A 端依赖初始化失败: %v\n", err)
			return 1
		}
	}
	if hostArg != cfg.Host {
		if err := config.SetHost(hostArg); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 1
		}
		cfg.Host = hostArg
		fmt.Printf("✓ 已写入 %s: host = %q\n", config.Path(), hostArg)
	}
	tr := session.SSHTransport(cfg, hostArg)
	if !*skipDeps {
		cmd, err := bootstrap.Command("b", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 1
		}
		fmt.Printf("B（%s）：检查并安装缺少的系统依赖…\n", termfilter.Sanitize(hostArg))
		if err := tr.RunInteractive(cmd, os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: B 端依赖初始化失败: %v\n", err)
			return 1
		}
	}
	if _, err := tr.Run([]string{"version"}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 1
	}
	fmt.Printf("✓ B 上已安装 waldo %s（%s/bin/waldo）\n", proto.FullVersion(), cfg.RemoteDirOrDefault())
	var paths []string
	if *adopt {
		for _, p := range cfg.Profiles() {
			paths = append(paths, p.State...)
		}
	}
	out, err := tr.Run([]string{"host", "--data-dir", "@DATA@", "--adopt=" + strconv.FormatBool(*adopt)}, []byte(strings.Join(paths, "\n")))
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 1
	}
	fmt.Print(indent(termfilter.Sanitize(string(out))))
	if *adopt {
		fmt.Println("✓ 已启用 B 上已有 Agent 配置和登录的复用（目录共享，单个文件首次复制）")
	} else {
		fmt.Println("✓ 已停止共享 B 上已有的 Agent 状态目录（保留 waldo 的独立状态和已有文件副本）")
	}
	return doctor(cfg)
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

func mark(ok, warn bool) string {
	switch {
	case ok:
		return "✓"
	case warn:
		return "!"
	}
	return "✗"
}

func profileNeeds(profiles map[string]profile.Profile) []string {
	needs := map[string]bool{}
	for _, p := range profiles {
		for _, n := range p.Needs {
			needs[n] = true
		}
	}
	var names []string
	for n := range needs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func doctor(cfg *config.Config) int {
	bad := 0
	distro := host.Distro()
	fmt.Println("A（本机）:")
	for _, bin := range []string{"ssh", "bwrap"} {
		p, err := exec.LookPath(bin)
		fmt.Printf("  %s %s %s\n", mark(err == nil, false), bin, p)
		if err != nil {
			bad++
			if h := host.InstallHint(distro, bin); h != "" {
				fmt.Printf("      安装: %s\n", h)
			}
		}
	}
	if out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "true").CombinedOutput(); err != nil {
		bad++
		fmt.Printf("  ✗ bwrap 无法创建沙箱: %s\n", strings.TrimSpace(string(out)))
	} else {
		fmt.Println("  ✓ bwrap 可以创建沙箱（user/pid/net 命名空间）")
	}
	home, _ := os.UserHomeDir()
	tc := sandbox.DetectToolchains(home)
	fmt.Printf("  ✓ 只读挂载的工具链: %s\n", strings.Join(shortHome(tc, home), " "))
	visible := func(p string) bool {
		if !strings.HasPrefix(p, home+"/") {
			return true
		}
		for _, d := range tc {
			if strings.HasPrefix(p, d+"/") {
				return true
			}
		}
		return false
	}
	profiles := cfg.Profiles()
	for _, n := range profileNeeds(profiles) {
		p, err := exec.LookPath(n)
		ok := err == nil && visible(p)
		detail := p
		if err == nil && !ok {
			detail += "（不在沙箱可见路径中，可用 [sandbox] expose_ro 追加）"
		}
		fmt.Printf("  %s %s %s\n", mark(ok, true), n, detail)
		if !ok {
			if h := host.InstallHint(distro, n); h != "" && err != nil {
				fmt.Printf("      安装: %s\n", h)
			}
		}
	}
	if cfg.Host == "" {
		fmt.Println("\nB: ✗ 未配置。运行 waldo setup <user@host>")
		return 1
	}
	fmt.Printf("\nB（%s）:\n", cfg.Host)
	tr := session.SSHTransport(cfg, cfg.Host)
	var req host.DoctorReq
	for _, n := range profile.Names(profiles) {
		req.Profiles = append(req.Profiles, profiles[n].Wire())
	}
	b, _ := json.Marshal(req)
	out, err := tr.Run([]string{"host", "--data-dir", "@DATA@", "--doctor"}, b)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return 1
	}
	var rep host.DoctorReport
	if err := json.Unmarshal(out, &rep); err != nil {
		fmt.Printf("  ✗ 无法解析 B 的报告: %v\n", err)
		return 1
	}
	san := termfilter.Sanitize
	fmt.Printf("  ✓ waldo %s（%s/%s %s）\n", san(rep.Version), san(rep.OS), san(rep.Arch), san(rep.Distro))
	for _, c := range rep.Checks {
		c.Name, c.Detail, c.Fix = san(c.Name), san(c.Detail), san(c.Fix)
		fmt.Printf("  %s %s %s\n", mark(c.OK, c.Warn), c.Name, c.Detail)
		if !c.OK && c.Fix != "" {
			fmt.Printf("      修复: %s\n", c.Fix)
		}
		if !c.OK && !c.Warn {
			bad++
		}
	}
	// B sampled its clock just before replying; ssh teardown is short.
	skew := time.Duration(rep.TimeUnixMs-time.Now().UnixMilli()) * time.Millisecond
	if skew < 0 {
		skew = -skew
	}
	okSkew := skew < 2*time.Second
	fmt.Printf("  %s 时钟偏差约 %v\n", mark(okSkew, true), skew.Round(10*time.Millisecond))
	if bad > 0 {
		return 1
	}
	return 0
}

func shortHome(ps []string, home string) []string {
	var out []string
	for _, p := range ps {
		if r, ok := strings.CutPrefix(p, home+"/"); ok {
			p = "~/" + r
		}
		out = append(out, p)
	}
	return out
}

func allow(args []string) int {
	global := false
	var doms []string
	for _, a := range args {
		if a == "--global" || a == "-g" {
			global = true
		} else {
			doms = append(doms, a)
		}
	}
	if len(doms) == 0 {
		fmt.Fprintln(os.Stderr, "用法: waldo allow [--global] <域名>...")
		return 2
	}
	key := "*"
	if !global {
		cwd, _ := os.Getwd()
		key = session.ProjectDir(cwd)
	}
	for _, d := range doms {
		d = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://"), "/")
		if err := config.AddAllowed(key, d); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 1
		}
		scope := "项目 " + key
		if key == "*" {
			scope = "所有项目"
		}
		fmt.Printf("✓ 已为%s放行 %s（对运行中的会话立即生效）\n", scope, d)
	}
	return 0
}

func ports() int {
	ents, _ := os.ReadDir(session.RunDir())
	n := 0
	for _, e := range ents {
		p := filepath.Join(session.RunDir(), e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st struct {
			PID     int    `json:"pid"`
			Session string `json:"session"`
			Agent   string `json:"agent"`
			Project string `json:"project"`
			Ports   []int  `json:"ports"`
		}
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		if st.PID <= 0 || syscall.Kill(st.PID, 0) != nil {
			os.Remove(p)
			continue
		}
		n++
		var ps []string
		for _, x := range st.Ports {
			ps = append(ps, "127.0.0.1:"+strconv.Itoa(x))
		}
		if len(ps) == 0 {
			ps = []string{"（无）"}
		}
		fmt.Printf("%s  %-8s %s\n    %s\n", st.Session, st.Agent, st.Project, strings.Join(ps, "  "))
	}
	if n == 0 {
		fmt.Println("没有运行中的会话")
	}
	return 0
}

func logCmd(args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	n := fs.Int("n", 50, "显示最后 N 条")
	follow := fs.Bool("f", false, "持续输出新事件")
	sess := fs.String("s", "", "只看某个会话")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dd := config.DataDir()
	if err := audit.Print(os.Stdout, dd, *n, *sess); err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 1
	}
	if !*follow {
		return 0
	}
	f, err := os.Open(audit.Path(dd))
	if err != nil {
		return 1
	}
	f.Seek(0, 2)
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		k, _ := f.Read(tmp)
		if k == 0 {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		buf = append(buf, tmp[:k]...)
		for {
			i := strings.IndexByte(string(buf), '\n')
			if i < 0 {
				break
			}
			line := string(buf[:i])
			buf = buf[i+1:]
			if *sess == "" || strings.Contains(line, `"s":"`+*sess+`"`) {
				fmt.Println(audit.Format(line))
			}
		}
	}
}

// LinksDir holds the wrappers created by `waldo link`.
func LinksDir() string { return filepath.Join(config.DataDir(), "links") }

func link(args []string) int {
	remove := false
	var names []string
	for _, a := range args {
		if a == "--remove" || a == "-d" {
			remove = true
		} else {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "用法: waldo link [--remove] <agent>...")
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir := LinksDir()
	os.MkdirAll(dir, 0o755)
	for _, n := range names {
		if strings.ContainsAny(n, "/ ") {
			fmt.Fprintf(os.Stderr, "waldo: 非法名字 %q\n", n)
			return 2
		}
		p := filepath.Join(dir, n)
		if remove {
			os.Remove(p)
			fmt.Printf("✓ 已移除 %s\n", p)
			continue
		}
		script := fmt.Sprintf("#!/bin/sh\n# created by `waldo link %s`\nexec %s %s \"$@\"\n", n, shq(exe), shq(n))
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("✓ 已创建 %s → waldo %s\n", p, n)
	}
	if !remove {
		first := strings.SplitN(os.Getenv("PATH"), ":", 2)[0]
		if first != dir {
			fmt.Printf("\n把下面这行加到 ~/.bashrc 或 ~/.zshrc，让它排在 PATH 最前面：\n  export PATH=%s:\"$PATH\"\n", shq(dir))
		}
	}
	return 0
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
