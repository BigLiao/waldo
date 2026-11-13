# Agent profile

每个 Agent 一份声明式配置，内置的在 `internal/profile/profiles.toml`。核心代码里没有任何 Agent 的名字。用户可在 `~/.config/waldo/config.toml` 用同名 `[agent.<name>]` 覆盖字段（未写的字段沿用内置值），或新增 Agent。

| 字段 | 含义 |
|---|---|
| `bin` | 在 B 上查找的可执行文件（B 的 PATH、`~/.local/bin` 等常见目录，最后用登录 shell 的 `command -v`）。可写绝对路径或 `~/...` |
| `args` | 放在用户参数之前的固定参数 |
| `env` | 给 Agent 进程的额外环境变量 |
| `shared` | Agent 进程写、而命令执行侧（A）要读的路径，会投射到 A 沙箱（`/tmp` 和项目目录总是投射） |
| `scrub` | 凭证类变量：保留在 B 上的 Agent 环境里，但绝不随命令带到 A |
| `needs` | `waldo doctor` 在 A 上检查的命令 |
| `state` | Agent 在 B 上的状态路径（`~/` 开头）：doctor 据此判断是否已登录；`waldo setup` 默认把其中已存在的**目录**绑定进胶囊（与 B 上原生 Agent 共用），**文件**首次复制到胶囊 HOME，已有非空副本保留（单文件绑定会让 rename 覆盖失败） |
| `mcp_config_flag` | `--paseo` 使用的 MCP 配置参数；目前支持 Claude 风格的 `--mcp-config` JSON/文件参数 |
| `skill_dir` | `--paseo` 只读投影已安装 Paseo skills 的目标目录，例如 `~/.claude/skills` |
| `telemetry` | 通过配置或 `--observe` 启用的原生遥测适配器：`"claude"`、`"codex"` 或 `"none"`；内置 profile 已配置，自定义 profile 未配置时只记录命令和文件请求 |

内置：

```toml
[agent.claude]
bin    = "claude"
env    = { USE_BUILTIN_RIPGREP = "0", DISABLE_AUTOUPDATER = "1" }
shared = ["~/.claude/shell-snapshots"]
scrub  = ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"]
needs  = ["rg", "git", "bash"]
state  = ["~/.claude", "~/.claude.json"]

[agent.codex]
bin    = "codex"
args   = ["--sandbox", "danger-full-access"]   # 由 waldo 的沙箱接管（审批策略不变）
scrub  = ["OPENAI_API_KEY", "CODEX_API_KEY"]
needs  = ["git", "bash"]
state  = ["~/.codex"]
```

`waldo run -- <命令>` 使用空 profile（只有 `bin`）。

`setup` 默认复用上面 `state` 声明的现有配置与登录，无需加参数。显式 `--adopt-login`、`--adopt-login=true` 仍可使用。需要停止共享已有状态目录时运行 `waldo setup --adopt-login=false`：这只清除目录绑定名单，不删除 B 上的原文件、此前复制的单文件或胶囊独立状态。再次开启复用时，已有非空单文件副本仍不会被覆盖。

## Agent 二进制如何进入胶囊

- ELF 二进制：解析符号链接后只读绑定到 `/run/waldo/agent/<名字>` 再执行。
- `#!` 脚本（例如 npm 安装的 node 脚本）：解释器从 **B** 上解析（`/usr/bin/env node` → B 的 node），绑定到 `/run/waldo/agent/interp-<名字>`；脚本所在的 `node_modules` 树按原路径只读绑定。否则解释器会是 stub，Agent 本身就会被转发到 A 去执行。
- 胶囊里 A 的 PATH 上每个目录都是 stub 目录，所以 Agent 起的任何子进程（包括写死的 `/bin/bash`）都在 A 上运行。Agent 包内部自带的原生二进制（不在 PATH 目录里）仍在 B 上运行。

## 实测记录

| Agent | 版本 | 结论 |
|---|---|---|
| Claude Code | 2.1.322 | `-p` 与交互式 TUI 均可用；Bash 工具在 A 沙箱执行；Write 工具（临时文件 + rename）经 FUSE 落到 A；shell 快照在 `~/.claude/shell-snapshots`、cwd 记录文件在 `/tmp`，与 profile 一致 |
| Codex | — | 未在本机实测（未安装）；`--sandbox danger-full-access` 参数沿用设计假设 |
