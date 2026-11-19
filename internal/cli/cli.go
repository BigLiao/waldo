// Package cli implements the user-facing commands that run on A.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
	"waldo/internal/wsl"
)

const usage = `waldo — Agent 跑在你的服务器上，它的"手"落在这台机器的沙箱里

用法:
  waldo setup [<user@host> | --remote <名称>] [--adopt-login=false] [--skip-deps]
                                             默认复用 B 的配置和登录，自动补齐依赖并检查
  waldo <agent> [--remote <名称>] [--paseo] [--observe[=true|false]] [--exec-mode host|remote|host-mcp] [参数...]
                                             运行 Agent（内置: %s）；--observe 控制模型及工具耗时统计
  waldo run [--] <命令> [参数...]             用空 profile 运行任意 CLI Agent
  waldo doctor [--remote <名称>]              检查两端依赖、时钟偏差、Agent 安装/登录
  waldo remote ls [--json]                    列出远端配置、默认选择和本机会话数（不连接 SSH）
  waldo remote status [名称 | --all]          限时检查 SSH、Waldo 版本和远端依赖，不安装或启动 Agent
  waldo sessions [--remote <名称>] [--json]   查看本机活跃会话与 SSH 连接状态
  waldo allow [--global] <域名>               给当前项目放行一个出网域名
  waldo ports [--remote <名称>] [--json]      查看当前转发的端口
  waldo log [--remote <名称>] [-n N] [-f]     查看本机审计日志
  waldo link [--remove] <agent>               在 PATH 前放一个 <agent> → waldo <agent> 的包装
  waldo version
  waldo help [命令 [子命令]]                 分级帮助，例如 waldo help remote status

示例:
  waldo claude                                原有单机配置直接使用，无需 --remote
  waldo claude --remote b2                     选择命名远端
  waldo remote status --all                    检查全部机器
  waldo help remote                           查看远端管理帮助

默认选择: --remote > default_remote > 原有 host；未设 host 时唯一命名远端自动选中。

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
	args, opts, err := wrapperPrefix(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 2
	}
	if topic, ok := helpRequest(args); ok {
		return showHelp(topic, os.Stdout)
	}
	cfg, code := loadConfig()
	if cfg == nil {
		return code
	}
	profiles := cfg.Profiles()
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, usage, strings.Join(profile.Names(profiles), ", "), config.Path())
		return 2
	}
	if opts.Paseo {
		if _, ok := profiles[args[0]]; !ok {
			fmt.Fprintln(os.Stderr, "waldo: --paseo 只用于已配置的 Agent")
			return 2
		}
	}
	if opts.Observe != nil && args[0] != "run" {
		if _, ok := profiles[args[0]]; !ok {
			fmt.Fprintln(os.Stderr, "waldo: --observe 只用于 Agent 或 run")
			return 2
		}
	}
	if opts.ExecMode != "" && args[0] != "run" {
		if _, ok := profiles[args[0]]; !ok {
			fmt.Fprintln(os.Stderr, "waldo: --exec-mode 只用于 Agent 或 run")
			return 2
		}
	}
	if opts.Remote != "" && args[0] != "run" && args[0] != "setup" && args[0] != "doctor" && args[0] != "sessions" && args[0] != "ports" && args[0] != "log" {
		if _, ok := profiles[args[0]]; !ok {
			fmt.Fprintln(os.Stderr, "waldo: --remote 只用于 Agent、run、setup、doctor、sessions、ports 或 log")
			return 2
		}
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(proto.FullVersion())
		return 0
	case "setup":
		return setup(cfg, withRemote(args[1:], opts.Remote))
	case "doctor":
		return doctorCommand(cfg, withRemote(args[1:], opts.Remote))
	case "remote":
		return remoteCmd(cfg, args[1:], os.Stdout)
	case "sessions":
		return sessionsCmd(withRemote(args[1:], opts.Remote), os.Stdout, false)
	case "allow":
		return allow(args[1:])
	case "ports":
		return sessionsCmd(withRemote(args[1:], opts.Remote), os.Stdout, true)
	case "log":
		return logCmd(withRemote(args[1:], opts.Remote))
	case "link":
		return link(args[1:])
	case "dev-sandbox":
		a := args[1:]
		if len(a) > 0 && a[0] == "--" {
			a = a[1:]
		}
		return session.DevSandbox(cfg, a)
	case "run":
		a, runOpts, err := wrapperPrefix(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 2
		}
		if runOpts.Paseo {
			fmt.Fprintln(os.Stderr, "waldo: --paseo 只用于已配置的 Agent")
			return 2
		}
		if len(a) > 0 && a[0] == "--" {
			a = a[1:]
		}
		if len(a) == 0 {
			fmt.Fprintln(os.Stderr, "用法: waldo run [--] <命令> [参数...]")
			return 2
		}
		runOpts.inherit(opts)
		return session.Run(session.Options{Config: cfg, Profile: profile.Empty(a[0]), Args: a[1:], Observe: runOpts.Observe, ExecMode: runOpts.ExecMode, Remote: runOpts.Remote})
	}
	if p, ok := profiles[args[0]]; ok {
		agentArgs, agentOpts, err := wrapperPrefix(args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 2
		}
		agentOpts.inherit(opts)
		return session.Run(session.Options{Config: cfg, Profile: p, Args: agentArgs, Paseo: agentOpts.Paseo, Observe: agentOpts.Observe, ExecMode: agentOpts.ExecMode, Remote: agentOpts.Remote})
	}
	fmt.Fprintf(os.Stderr, "waldo: 未知命令或 Agent %q（waldo help 查看用法；任意命令请用 waldo run -- %s）\n", args[0], args[0])
	return 2
}

// Only consume wrapper flags before agent arguments. Literal --paseo or
// --observe supplied after -p or -- remain agent arguments.
type wrapperOptions struct {
	Paseo    bool
	Observe  *bool
	ExecMode string
	Remote   string
}

func (o *wrapperOptions) inherit(parent wrapperOptions) {
	o.Paseo = o.Paseo || parent.Paseo
	if o.Observe == nil {
		o.Observe = parent.Observe
	}
	if o.ExecMode == "" {
		o.ExecMode = parent.ExecMode
	}
	if o.Remote == "" {
		o.Remote = parent.Remote
	}
}

func wrapperPrefix(args []string) ([]string, wrapperOptions, error) {
	var opts wrapperOptions
	for len(args) > 0 {
		switch args[0] {
		case "--paseo":
			opts.Paseo = true
		case "--observe":
			enabled := true
			opts.Observe = &enabled
		case "--observe=true", "--observe=false":
			enabled := args[0] == "--observe=true"
			opts.Observe = &enabled
		case "--remote":
			if len(args) < 2 {
				return nil, opts, fmt.Errorf("--remote 缺少远端名称")
			}
			if err := config.ValidateRemoteName(args[1]); err != nil {
				return nil, opts, err
			}
			opts.Remote = args[1]
			args = args[1:]
		case "--exec-mode":
			if len(args) < 2 {
				return nil, opts, fmt.Errorf("--exec-mode 缺少 host、remote 或 host-mcp")
			}
			opts.ExecMode = args[1]
			if err := config.ValidateExecMode(opts.ExecMode); err != nil {
				return nil, opts, err
			}
			args = args[1:]
		default:
			if value, ok := strings.CutPrefix(args[0], "--remote="); ok {
				if err := config.ValidateRemoteName(value); err != nil {
					return nil, opts, err
				}
				opts.Remote = value
				args = args[1:]
				continue
			}
			if value, ok := strings.CutPrefix(args[0], "--exec-mode="); ok {
				if err := config.ValidateExecMode(value); err != nil {
					return nil, opts, err
				}
				opts.ExecMode = value
				args = args[1:]
				continue
			}
			if strings.HasPrefix(args[0], "--observe=") {
				return nil, opts, fmt.Errorf("--observe expects true or false")
			}
			return args, opts, nil
		}
		args = args[1:]
	}
	return args, opts, nil
}

func withRemote(args []string, remote string) []string {
	if remote != "" {
		return append([]string{"--remote", remote}, args...)
	}
	return args
}

func setup(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	adopt := fs.Bool("adopt-login", true, "复用 B 上已有的 Agent 配置和登录；设为 false 停止共享已有目录")
	skipDeps := fs.Bool("skip-deps", false, "跳过自动安装系统依赖（仍运行 doctor）")
	remote := fs.String("remote", "", "初始化已配置的命名远端")
	var hostArg string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--remote" || a == "-remote" {
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if !strings.HasPrefix(a, "-") && hostArg == "" {
			hostArg = a
		} else {
			rest = append(rest, a)
		}
	}
	if code := managementFlags(fs, rest); code >= 0 {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "waldo: setup 只接受一个服务器地址")
		return 2
	}
	if flagSet(fs, "remote") && *remote == "" {
		fmt.Fprintln(os.Stderr, "waldo: --remote 缺少远端名称")
		return 2
	}
	if hostArg != "" && *remote != "" {
		fmt.Fprintln(os.Stderr, "waldo: --remote 与服务器地址不能同时使用，请在 [remote.<名称>] 配置 host")
		return 2
	}
	original := cfg
	legacyUpdate := hostArg != "" && hostArg != cfg.Host
	// A positional address retains legacy setup semantics and updates only host.
	// A named/default remote is initialized without changing any configuration.
	if hostArg != "" {
		selected := *cfg
		selected.Host = hostArg
		selected.DefaultRemote = ""
		cfg = &selected
	}
	selected, name, err := cfg.ResolveRemote(*remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 2
	}
	cfg = selected
	hostArg = cfg.Host
	if name != "" {
		fmt.Printf("远端 %s（%s）\n", name, termfilter.Sanitize(hostArg))
	}
	if !*skipDeps {
		fmt.Println("A（本机）：检查并安装缺少的系统依赖…")
		if err := bootstrap.Install("a", profileNeeds(cfg.Profiles()), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: A 端依赖初始化失败: %v\n", err)
			return 1
		}
	}
	if legacyUpdate {
		if err := config.SetHost(hostArg); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 1
		}
		original.Host = hostArg
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
	fmt.Printf("✓ B 上已安装 waldo %s（%s，按版本保存）\n", proto.FullVersion(), cfg.RemoteDirOrDefault())
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

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func doctorCommand(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	remote := fs.String("remote", "", "检查指定远端")
	if code := managementFlags(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 || flagSet(fs, "remote") && *remote == "" {
		fmt.Fprintln(os.Stderr, "用法: waldo doctor [--remote <名称>]")
		return 2
	}
	selected, name, err := cfg.ResolveRemote(*remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
		return 2
	}
	if name != "" {
		fmt.Printf("远端 %s\n", name)
	}
	return doctor(selected)
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

var checkLocalSandbox = sandbox.Probe

func doctor(cfg *config.Config) int {
	bad := 0
	distro := host.Distro()
	fmt.Println("A（本机）:")
	if wsl.IsWSL2() {
		mode := wsl.NetworkMode()
		fmt.Printf("  %s WSL2 网络模式: %s\n", mark(mode != "none" && mode != "unknown", true), mode)
		if mode == "none" {
			fmt.Println("      检查 WSL 启动错误与 Windows 用户目录中的 .wslconfig")
		}
		if bridge, err := wsl.NewPortBridge(); err != nil {
			fmt.Printf("  ! Windows 回环端口桥接不可用: %v\n", err)
		} else {
			bridge.Close()
		}
	}
	localBins := []string{"ssh", "bwrap"}
	if runtime.GOOS == "darwin" {
		localBins = []string{"ssh", "/usr/bin/sandbox-exec"}
	}
	for _, bin := range localBins {
		p, err := exec.LookPath(bin)
		fmt.Printf("  %s %s %s\n", mark(err == nil, false), bin, p)
		if err != nil {
			bad++
			if h := host.InstallHint(distro, bin); h != "" {
				fmt.Printf("      安装: %s\n", h)
			}
		}
	}
	if err := checkLocalSandbox(); err != nil {
		bad++
		fmt.Printf("  ✗ 本机沙箱验证失败: %v\n", err)
	} else {
		fmt.Println("  ✓ 本机沙箱可执行，平台隔离检查通过")
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

func logCmd(args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	n := fs.Int("n", 50, "显示最后 N 条")
	follow := fs.Bool("f", false, "持续输出新事件")
	sess := fs.String("s", "", "只看某个会话")
	remote := fs.String("remote", "", "只看指定远端的历史日志")
	if code := managementFlags(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "用法: waldo log [--remote <名称>] [-s 会话] [-n N] [-f]")
		return 2
	}
	if flagSet(fs, "remote") {
		if err := config.ValidateRemoteName(*remote); err != nil {
			fmt.Fprintf(os.Stderr, "waldo: %v\n", err)
			return 2
		}
	}
	filter := audit.Filter{Session: *sess, Remote: *remote}
	dd := config.DataDir()
	if err := audit.PrintFiltered(os.Stdout, dd, *n, filter); err != nil {
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
			if filter.Matches(line) {
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
