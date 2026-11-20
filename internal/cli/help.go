package cli

import (
	"fmt"
	"io"
	"strings"

	"waldo/internal/config"
	"waldo/internal/profile"
)

// Recognize wrapper help before loading configuration. Agent --help and
// arguments after run/-- remain the agent's own arguments.
func helpRequest(args []string) ([]string, bool) {
	if len(args) == 0 {
		return nil, false
	}
	isFlag := func(s string) bool { return s == "-h" || s == "--help" }
	if args[0] == "help" || isFlag(args[0]) {
		return args[1:], true
	}
	if args[0] == "remote" {
		if len(args) >= 2 && (args[1] == "help" || isFlag(args[1])) {
			return append([]string{"remote"}, args[2:]...), true
		}
		if len(args) == 3 && isFlag(args[2]) {
			return args[:2], true
		}
	}
	switch args[0] {
	case "setup", "doctor", "sessions", "ports", "log", "run", "allow", "link":
		if len(args) == 2 && isFlag(args[1]) {
			return args[:1], true
		}
	}
	return nil, false
}

const remoteHelp = `用法:
  waldo remote ls [--json]                           查看配置、默认远端 (*) 和本机会话数
  waldo remote status [名称] [--timeout 10s] [--json] 检查指定机器，省略名称检查默认远端
  waldo remote status --all [--timeout 10s] [--json]  检查全部机器，最多 4 台并发

ls 不连接 SSH。status 使用非交互 SSH，不安装或升级程序，不启动 Agent。
状态包括 ready、warning、unreachable、not-installed、version-mismatch、not-ready。
进一步帮助: waldo help remote status
`

var commandHelp = map[string]string{
	"remote": remoteHelp,
	"remote ls": `用法: waldo remote ls [--json]
别名: waldo remote list
显示名称、默认标记 (*)、SSH 目标和本机活跃会话数，不连接 SSH。
(host) 表示兼容原有顶层 host 配置。活跃客户端数量不代表 SSH 可达。
`,
	"remote status": `用法: waldo remote status [名称 | --all] [--timeout 10s] [--json]
省略名称检查默认远端；--all 检查全部配置，最多 4 台并发。
--timeout 为单台机器完整检查的超时，默认 10s，范围 100ms–1m。
--json 输出 JSON 数组，单台检查同样输出数组。

只读检查 SSH、已有 Waldo 版本和同版本的远端依赖，不安装、升级或启动 Claude。
依赖及状态目录检查不验证凭证有效期或剩余额度。
退出码: 0 正常或仅警告；1 连接、安装、版本或必要依赖异常；2 参数或配置选择错误。

示例:
  waldo remote status
  waldo remote status b2
  waldo remote status --all --timeout 5s --json
`,
	"setup": `用法: waldo setup [<user@host> | --remote <名称>] [--adopt-login=false] [--skip-deps]
省略目标使用默认远端。位置地址写入旧版顶层 host；--remote 初始化已配置机器，不修改默认项。
默认安装缺少的系统依赖，安装/更新 B 的 Waldo，复用 B 已有登录状态，并运行 doctor。
--skip-deps 跳过系统依赖安装；--adopt-login=false 停止共享已有状态目录，保留原文件。

示例:
  waldo setup ubuntu@server
  waldo setup --remote b2
`,
	"doctor": `用法: waldo doctor [--remote <名称>]
省略名称检查默认远端；检查两端依赖、沙箱、Agent 安装及状态目录。
doctor 沿用原行为，可能安装或更新 B 的 Waldo；只读连接检查请用 waldo remote status。
`,
	"sessions": `用法: waldo sessions [--remote <名称>] [--json]
读取本机活跃客户端记录，显示会话、远端、SSH 状态、Agent、PID 和项目，不连接 SSH。
SSH 状态: starting、connected、reconnecting；旧客户端未记录时为 unknown。
--remote 按记录名称筛选，不要求该远端仍在配置中。
`,
	"ports": `用法: waldo ports [--remote <名称>] [--json]
查看本机活跃会话转发到 127.0.0.1 的端口，不连接 SSH。
无参数保留原有文本格式；--json 输出结构化会话记录。
`,
	"log": `用法: waldo log [--remote <名称>] [-s 会话] [-n N] [-f]
读取本机审计日志。-n 默认 50 条；-f 持续跟踪；-s 与 --remote 可组合筛选。
已删除配置的远端仍可筛选历史日志；旧版未标记远端的日志无法按名称筛选。
`,
	"run": `用法: waldo run [--remote <名称>] [--observe[=true|false]] [--exec-mode host|remote|host-mcp] -- <命令> [参数...]
用空 profile 运行任意 CLI Agent。默认模式为 host（空 profile 不支持 MCP）；remote/host-mcp 需 profile 支持 MCP。
-- 后的参数原样传给命令，不再解析 Waldo 参数。
`,
	"allow": `用法: waldo allow [--global] <域名>...
为当前项目增加出网域名白名单；--global 应用于所有项目，运行中的会话立即生效。
`,
	"link": `用法: waldo link [--remove] <agent>...
创建 <agent> → waldo <agent> 的 PATH 包装脚本；--remove 移除包装。
`,
}

func showHelp(topic []string, out io.Writer) int {
	// Help must work even before setup or with a malformed config file.
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	key := strings.Join(topic, " ")
	if key == "remote list" {
		key = "remote ls"
	}
	if key == "" {
		fmt.Fprintf(out, usage, strings.Join(profile.Names(cfg.Profiles()), ", "), config.Path())
		return 0
	}
	if help, ok := commandHelp[key]; ok {
		fmt.Fprint(out, help)
		return 0
	}
	if _, ok := cfg.Profiles()[key]; ok {
		fmt.Fprintf(out, `用法: waldo %s [--remote <名称>] [--paseo] [--observe[=true|false]] [--exec-mode host|remote|host-mcp] [Agent 参数...]
Waldo 参数放在 Agent 参数之前；--paseo 由 Paseo 启动时使用。
host-mcp（支持 MCP 的 Agent 默认）: 同名 MCP 工具按路径分发；host: 命令在本机沙箱（其余 Agent 默认）；remote: 内置 Bash 在远端。
选择顺序: --remote、default_remote、原有 host；未设置 host 时，唯一命名远端自动选中。
Agent 自身的帮助: waldo %s --help（将连接远端）。
`, key, key)
		return 0
	}
	fmt.Fprintf(out, "waldo: 未知帮助主题 %q。运行 waldo help 查看命令。\n", key)
	return 2
}
