# waldo 设计方案

> **一句话**：Agent（Claude Code / Codex / …）的进程和登录态放在你的服务器上，它的"手"——文件读写、命令执行、端口——落在你当前这台机器的沙箱里。在任何一台自己的机器上敲 `claude`，体验和本地原生一致。
>
> `waldo` 是暂定名（取自海因莱因小说里的远程机械手），可随时替换。

---

## 1. 目标与非目标

### 目标

| # | 目标 | 说明 |
|---|---|---|
| G1 | **一处登录，处处使用** | Agent 只在服务器 B 上登录一次；你的每台机器 A 都能用，会话历史和用户级配置天然共享 |
| G2 | **像原生一样简单** | `cd 项目 && claude`。参数、管道、退出码、权限确认界面与原生一致 |
| G3 | **代码和运行环境留在 A** | 不同步文件；`npm start` 起在 A 上，A 的浏览器/调试器直接联调 |
| G4 | **B 被攻破不波及 A 宿主机** | B 能触达的只有 A 上的一个轻量沙箱和当前项目目录 |
| G5 | **Agent 无关** | 核心不依赖任何 Agent 的私有机制；每个 Agent 只需一份很薄的声明式 profile |
| G6 | **轻** | 单个静态二进制、无常驻服务、不用 VM、不开端口、不管理证书 |

### 非目标

- **不是 API 反代**：不拦截、不改写、不转发模型请求。B 上跑的是未修改的官方客户端。
- **不做多人共享、账号池、额度轮换**：一个人、自己的账号、自己的机器。
- **不追求虚拟机级隔离**：共享内核的 namespace 沙箱，够用为止（见 §7.4）。
- v1 不支持 Windows；macOS 作为 A 放在 v2。

---

## 2. 使用体验

```sh
# 一次性：在 A 上安装并指向你的服务器（复用你已有的 SSH 配置和密钥）
curl -fsSL <install-url> | sh
waldo setup me@my-server        # 自动把同版本二进制装到 B，并检查两端依赖

# 日常
cd ~/work/shop
waldo claude                    # 交互式，和原生 claude 一样
waldo claude -p "跑一下测试" | tee out.txt     # 管道、退出码照常
waldo codex
waldo run -- aider              # 任意其他 CLI agent

# 想连 waldo 这个词都不打：
waldo link claude               # 在 PATH 前放一个 claude → waldo claude 的包装
claude                          # 从此就是它
```

辅助命令（都在 A 上执行）：

| 命令 | 作用 |
|---|---|
| `waldo doctor` | 检查两端依赖、时钟偏差、Agent 是否已安装/登录 |
| `waldo allow <域名>` | 给当前项目放行一个出网域名 |
| `waldo ports` | 查看当前转发的端口 |
| `waldo log` | 查看本机审计日志（执行过的命令、写过的文件） |

首次运行某个 Agent 时，它会像原生一样提示登录（打印一个 URL，在 A 的浏览器里打开、粘回验证码）。登录态落在 B 上，之后所有机器免登录。

---

## 3. 核心思路：环境投射

把"Agent 在哪跑"和"Agent 作用在哪"拆开。Agent 进程在 B 上的一个**胶囊（capsule）**里运行，胶囊里的环境是 A 的**投射**：

| Agent 做的事 | 胶囊里看到的 | 实际发生在 |
|---|---|---|
| 进程内读写文件（Read/Edit/Write） | 与 A **相同绝对路径**的项目目录 | A 沙箱（文件投射，实时，无同步） |
| 启动任何子进程（bash、git、rg、node、MCP server…） | PATH 上与 A 同名的可执行文件 | A 沙箱（执行转发） |
| 起服务监听端口 | — | A 沙箱；白名单端口自动出现在 A 的 `127.0.0.1` |
| 访问模型 API、读写自己的登录态和会话 | 胶囊内的 HOME | **B**（这是唯一留在 B 的东西） |
| 画 TUI、读键盘 | 一个 pty | A 的终端 |

这是 Plan 9 `cpu` 命令（把本地命名空间导出给远端进程）的变体，区别在于连**进程执行**也投射回本地。

**Agent 无关性来自这里**：我们不 hook Agent 的任何工具，只是给它一个"文件系统 + 可执行文件"都被替换过的普通 Linux 环境。凡是"读写文件 + 起子进程"的 CLI Agent 都适用。

---

## 4. 架构

```
A：你的任意一台机器                                       B：你的服务器
┌──────────────────────────────────────────┐            ┌─────────────────────────────────────────┐
│ 你的终端                                  │            │ waldo host   （每会话一个，随 SSH 启动/退出）│
│   ▲ tty                                   │            │   ├─ pty ⇄ Agent                         │
│ waldo（宿主机进程，可信）                  │   ssh -T   │   ├─ sshfs(passive) 挂载点               │
│   ├─ 终端转发 + 转义序列过滤               │◀══════════▶│   └─ stub 请求汇聚                       │
│   ├─ 出网代理（域名白名单）                │  单连接多路 │  ┌──── capsule (bwrap) ───────────────┐ │
│   ├─ 端口转发（白名单）                    │    复用     │  │ Agent（官方二进制，未修改）          │ │
│   └─ 审计日志                             │            │  │  HOME      = B 上的持久目录(登录态)  │ │
│        │ 管道（唯一通道）                  │            │  │  项目目录  = FUSE → A               │ │
│  ┌─────┴──── 沙箱 (bwrap) ─────────────┐  │            │  │  /tmp      = FUSE → A               │ │
│  │ waldo sandbox-serve                  │  │            │  │  PATH 各目录 = stub（全部转发到 A）   │ │
│  │   ├─ SFTP 服务（文件投射的服务端）    │  │            │  │  没有 shell、没有任何 B 的工具       │ │
│  │   ├─ exec 服务（bash/git/node/…）    │  │            │  └─────────────────────────────────────┘ │
│  │   └─ 监听端口探测                    │  │            └─────────────────────────────────────────┘
│  │ 可写：项目目录、/tmp、沙箱 HOME       │  │
│  │ 只读：系统与工具链；网络：隔离        │  │
│  └──────────────────────────────────────┘  │
└──────────────────────────────────────────┘
```

**一个二进制，四个角色**（按子命令/`argv[0]` 区分）：

| 角色 | 位置 | 职责 | 信任级别 |
|---|---|---|---|
| `waldo`（client） | A 宿主机 | 起 SSH、起沙箱、在两者间搬运字节；终端过滤、出网代理、端口转发、审计 | **可信**，代码量要小 |
| `waldo sandbox-serve` | A 沙箱内 | 解析并执行 B 发来的全部文件/执行请求 | 不可信侧（按已失陷设计） |
| `waldo host` | B | 搭胶囊、挂载、起 Agent、汇聚 stub 请求 | 不可信侧 |
| stub（`argv[0]` ≠ waldo） | B 胶囊内 | 把一次进程启动原样转发到 A | 不可信侧 |

---

## 5. 关键机制

### 5.1 传输：直接用系统 SSH

`waldo` 在 A 上执行 `ssh -T <host> waldo host`，协议跑在这条 SSH 会话的 stdin/stdout 上（与 git、rsync、VS Code Remote 相同的做法），其上用 yamux 做流多路复用。

- **零部署**：B 上没有常驻进程、不开新端口、没有证书要管。认证/加密/穿透 NAT/跳板机全部继承用户的 `~/.ssh/config`。
- **自动安装**：握手时校验版本，不一致就把匹配架构的二进制经 SSH 传到 B 的 `~/.local/share/waldo/`。两端永远同版本，无协议兼容负担。
- **强制收紧 SSH**：`-o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes`，不分配 SSH 层的 pty。B 无法借这条连接向 A 发起任何转发。
- 传输层抽象成"一条双向字节流"的接口，日后可加其他传输，v1 只做 SSH。

流的种类：`ctl`（握手、窗口大小、端口事件）、`tty`、`fs`（每个挂载一条）、`exec`（每个进程一条）、`net`（代理/端口转发的每条连接一条）。

### 5.2 B 端胶囊

`waldo host` 用 bwrap 拼一个合成的根文件系统来运行 Agent：

| 胶囊内路径 | 来源 | 读写 |
|---|---|---|
| `$HOME`（**字符串与 A 的家目录相同**） | B 上的 `~/.local/share/waldo/home/` | rw，持久。登录态、会话历史、用户级配置都在这里 |
| 项目目录（与 A 相同的绝对路径） | 文件投射 → A 沙箱 | rw |
| `/tmp` | 文件投射 → A 沙箱的会话级 `/tmp` | rw |
| profile 声明的"执行可见路径" | 文件投射 → A 沙箱 | rw |
| A 的 PATH 上每个目录（`/usr/bin`、`~/.nvm/…/bin` …） | stub 目录：A 上有的每个命令名 → stub | ro |
| Agent 二进制、动态库、CA 证书、`resolv.conf` | B 本机 | ro |

要点：

- **路径处处相同**。A 宿主机、A 沙箱、B 胶囊里，项目路径和 `$HOME` 是同一个字符串。Agent 记录的 cwd、写进命令里的路径、报错里的路径，三处通用。
- **胶囊里没有任何真实工具**。`/bin/bash`、`/usr/bin/env` 都是 stub，所以无论 Agent 用 PATH 查找还是写死绝对路径，子进程都落到 A。带 shebang 的脚本被直接执行时，内核会去启动解释器，而解释器是 stub，同样被转发。
- **顺带保护了 B**：被提示注入的 Agent 在 B 上除了自己的状态目录什么都碰不到。
- 胶囊 HOME 与 B 的真实家目录分开（避免单文件绑定在"写临时文件再 rename"时失败，也避免 Agent 读到 B 上的其他东西）。想复用 B 上已有的原生登录，可用 `waldo setup --adopt-login` 把现有状态目录链接进来。

### 5.3 执行转发

stub 是同一个静态二进制的多入口形态：

1. 被以任意名字启动（`bash`、`git`、`rg`…），通过胶囊内的 unix socket 把 `argv`、`cwd`、环境变量、自身的 stdio 交给 `waldo host`；
2. `waldo host` 为它开一条 `exec` 流到 A，`sandbox-serve` 在沙箱里以相同的 argv/cwd 启动真实进程（独立进程组）；
3. stdout/stderr/stdin 流式双向转发；stub 收到的信号转发给 A 上的进程组；退出码原样返回；
4. stub 被强杀或连接断开 → A 上对应进程组被终止。**连不上 A 时 stub 直接报错退出，绝不在 B 本地执行**。

约定：

- **环境变量**：胶囊的初始环境本就取自 A 沙箱（PATH、HOME、LANG、TERM、TZ…），所以 stub 把自己的环境整体带过去即可，只过滤 `WALDO_*` 和 profile 列出的凭证变量（如 `ANTHROPIC_API_KEY`、`CLAUDE_CODE_OAUTH_TOKEN`、`OPENAI_API_KEY`）。
- **一个会话一个沙箱实例**：所有被转发的进程共享同一个网络/PID/挂载命名空间和 `/tmp`，所以 `npm start` 起的服务能被随后的 `curl localhost:3000` 访问到，行为与本机一致。
- 一次 Bash 工具调用 = 一次 `bash -c …` = 一条 exec 流，额外延迟只有一个网络往返。
- MCP server（stdio）、hooks、statusline 命令都是 Agent 起的子进程，**自动**运行在 A 沙箱，无需特殊处理。

### 5.4 文件投射

线上协议直接用 **SFTP**，不自造协议：

- **A 端**：`sandbox-serve` 内嵌 SFTP 服务端（Go 的 `pkg/sftp`），A 不需要装 sshd。它运行在沙箱里，**路径约束由挂载命名空间保证**——沙箱里根本看不到项目以外的东西，不靠代码做路径校验。
- **B 端（v1）**：`sshfs -o passive` 把 SFTP 流挂成 FUSE 目录，再由 bwrap 绑定进胶囊。这是 Lima 等项目验证过的"反向 sshfs"做法，v1 的文件系统部分几乎不用写代码。
- **一致性与跨机延迟**：初版关闭属性/目录缓存；真实两机测试发现重复 stat 成为瓶颈，当前默认给项目目录设置 10 秒的内核属性/路径缓存。全局 `[fs] metadata_cache_ttl = "10s"` 支持项目覆盖，范围为 `0s` 到 `1m` 的整数秒，`"0s"` 关闭缓存，新会话生效。目录列表、负查询与共享运行路径仍不缓存，不叠加 SSHFS 用户态缓存，不新增跨次打开的内容缓存。操作者按约定人工避免 A/B 同时修改项目；A 上编辑或执行命令（包括 Agent 转发的命令）后的属性或路径替换仍允许延迟一个 TTL 加在途请求时间可见，写入仍在 close 返回前落到 A。大规模扫描优先走被转发到 A 的 `rg`/`git`。
- **B 端（v2）**：换成内置的 go-fuse 客户端（仍讲 SFTP），加带失效推送的缓存和目录预取。接口不变，纯性能优化。

### 5.5 A 端沙箱

沙箱后端是一个接口（输入：可写路径、只读路径、网络策略；输出：能在其中启动进程的句柄）。Linux 用 bubblewrap，macOS（v2）用 seatbelt。

Linux 默认策略：

| 项 | 策略 |
|---|---|
| 系统（`/usr`、`/etc` 等） | 只读 |
| 工具链（`~/.nvm`、`~/.cargo`、`~/go`、mise/asdf/pyenv/bun…） | 自动探测，只读挂载——**直接用宿主机已装的，不重复安装** |
| `$HOME` | 换成沙箱专用的持久目录（`~/.local/share/waldo/sandbox-home/`）。看不到 `~/.ssh`、`~/.aws`、`~/.gitconfig`、浏览器数据；npm/pip 缓存留在这里跨会话复用 |
| 项目目录 | 读写，原路径 |
| `.git/hooks`、`.git/config`、`.envrc`、`.vscode/`、`.idea/` | 只读（防止写入"你之后会在宿主机上自动执行"的东西） |
| `/tmp` | 会话级 tmpfs |
| 网络 | 独立网络命名空间，只有 loopback；见 §5.6 |
| 其他 | 独立 PID 命名空间、丢弃全部 capability、`--new-session`、`--die-with-parent` |

默认直接作用于项目目录（与原生行为一致）。`--isolated` 选项改为在一份 git worktree 上工作，审查后再合并。

沙箱里**没有通往 B 的任何通道和凭证**：它只有一根连到宿主机 `waldo` 进程的管道，且只能被动响应请求。

### 5.6 网络

沙箱内只有 loopback，三类流量都经管道由宿主机 `waldo` 中转并执行策略：

| 方向 | 机制 | 默认策略 |
|---|---|---|
| **沙箱 → 外网** | 沙箱内 `HTTP(S)_PROXY` 指向 `sandbox-serve` 的本地端口 → 宿主机代理按域名白名单放行 | 内置常见包仓库和 GitHub；其余拒绝，并在响应里提示 `waldo allow <域名>` |
| **A 宿主机 → 沙箱里的服务** | `sandbox-serve` 探测沙箱内新出现的监听端口 → 宿主机在 `127.0.0.1:<同端口>` 上转发 | 自动转发 ≥1024 且宿主机上空闲的端口；数据库等知名端口默认不转发（防止冒充本地服务） |
| **沙箱 → A 宿主机上的服务** | 显式声明的端口在沙箱内的 `127.0.0.1` 上可达 | 默认无；按项目配置 `host_ports = [5432]` |

依赖服务（数据库、Redis）由你在宿主机上用 Docker 自行启动，沙箱按端口白名单访问。沙箱内不提供 Docker。

策略文件只放在 `~/.config/waldo/`，沙箱内不可见、不可写——Agent 改不了自己的白名单。

### 5.7 终端

- 交互模式：B 上为 Agent 分配 pty，原始字节经 `tty` 流到 A；A 的终端置为 raw 模式，窗口大小变化经 `ctl` 流同步。
- 非交互模式（stdin/stdout 不是终端）：不分配 pty，三个标准流原样转发，退出码一致，可用于脚本和管道。
- **转义序列过滤**：宿主机 `waldo` 对 B 发来的输出做流式解析，丢弃 OSC 52（写剪贴板）、DCS/APC 等，只放行 TUI 需要的子集。

### 5.8 Agent profile

每个 Agent 一份内嵌的声明式配置，核心代码里没有任何 Agent 的名字：

```toml
[agent.claude]
bin    = "claude"                              # 在 B 上查找
env    = { USE_BUILTIN_RIPGREP = "0", DISABLE_AUTOUPDATER = "1" }
shared = ["~/.claude/shell-snapshots"]         # Agent 进程写、而命令执行侧要读的路径 → 投射到 A
scrub  = ["ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"]   # 不随命令带到 A 的变量
needs  = ["rg", "git"]                         # doctor 在 A 上检查

[agent.codex]
bin   = "codex"
args  = ["--sandbox", "danger-full-access"]    # 关闭自带沙箱，由 waldo 的沙箱接管（审批策略不变）
scrub = ["OPENAI_API_KEY"]
```

`waldo run -- <任意命令>` 使用空 profile。用户可在配置里追加或覆盖 profile。

---

## 6. 会话生命周期

1. A：读取策略 → 启动沙箱 → 采集环境事实（HOME、PATH 各目录及其中的命令名、系统类型、时钟）。
2. A：`ssh -T B waldo host`（必要时先自动安装）→ 握手（版本、会话描述、时钟偏差检查）。
3. B：建立 SFTP 挂载（项目目录、`/tmp`、profile 的 shared 路径）→ 生成 stub 目录 → 用 bwrap 启动胶囊，在其中运行 Agent。
4. 运行期：tty / fs / exec / net 各流并行。
5. Agent 退出 → 退出码传回 A → 卸载挂载、销毁沙箱（其中所有进程随之结束）。

**断线**：检测到 SSH 中断后，A/B 保留原会话并开启 90 秒恢复窗口。B 使用独立的每会话 host 进程持有胶囊和挂载，SSH 连接只转接到私有 Unix socket。yamux 下方的字节续传层使用消费偏移确认和接收偏移恢复，保留在途命令、SFTP 句柄和终端；每方向最多缓冲 4 MiB，缓冲满或等待跨机请求时阻塞。自动重试只接回原会话，超时则终止进程并清理。对话保存在 B 上，超时后可用 Agent 自带的恢复功能（如 `claude --resume`）启动新会话。客户端退出后重新 attach 尚未实现。

多台 A 可同时使用，各自是独立会话，共享 B 上的 Agent 状态目录（官方客户端本身支持多实例）。

---

## 7. 安全模型

### 7.1 信任边界

```
可信：A 宿主机上的 waldo 进程（只做：字节搬运、终端过滤、出网/端口策略、写日志）
─────────────────────────────── 边界 ───────────────────────────────
不可信：B 的一切、A 沙箱内的一切（含 sandbox-serve 自身）、模型输出
```

设计上假定 B 已被攻破、沙箱内进程已被控制，此时仍要保证 A 宿主机无恙。

### 7.2 B 被攻破时

| 攻击者能做的 | 攻击者做不到的 |
|---|---|
| 在 A 沙箱里执行任意命令 | 读 `~/.ssh`、云凭证、其他项目、浏览器数据（沙箱里不存在） |
| 读写当前项目目录 | 写 `.git/hooks`、`.git/config`、`.envrc`、编辑器配置（只读） |
| 访问白名单域名 | 访问 A 的内网、宿主机上未声明的本地服务（独立网络命名空间） |
| 在白名单范围内让 A 的 localhost 出现一个端口 | 占用数据库等知名端口、绑定到非 loopback 地址 |
| 往你的终端输出内容 | 写剪贴板、借 SSH 转发 agent/端口/X11（过滤、禁用） |
| 拿到 B 上的 Agent 登录态（B 自身的损失） | 改动 waldo 的策略文件、清除宿主机上的审计日志 |

### 7.3 留给用户的一道关

沙箱里产出的东西进入宿主机世界，只有一条路：你在宿主机上亲手运行它。需要凭证的操作（`git push`、发布）在宿主机上由你完成，这同时是一次自然的审查点。`waldo log` 和 `git diff` 是配套工具；`--isolated`（worktree）把这道关做成强制的。

### 7.4 明确接受的残余风险

- 共享内核：内核漏洞可导致沙箱逃逸。个人开发场景下接受；沙箱后端是接口，需要时可换 VM/gVisor 而不动上层。
- 项目目录本身不隔离（默认模式）：模型可以改坏工作区——与原生 Agent 相同，靠 git 兜底。
- 项目里的普通可执行内容（`package.json` scripts、`Makefile`）无法设为只读，在宿主机上运行前应看一眼 diff。

---

## 8. 配置

零配置可用；全部配置在 A 的 `~/.config/waldo/config.toml`：

```toml
host = "me@my-server"                     # SSH 目标

[sandbox]
expose_ro = ["~/.nvm", "~/.cargo", "~/go"]   # 自动探测结果之外再追加

[net]
allow = ["registry.npmjs.org", "github.com", "pypi.org"]
ports = "auto"                            # "auto" | [3000, 5173] | "off"

[project."/home/me/work/shop"]            # 按项目路径覆盖
net.allow      = ["api.staging.example.com"]
net.host_ports = [5432, 6379]             # 沙箱可访问的宿主机服务
```

B 上没有配置文件；一切由 A 在握手时下发。

---

## 9. 代码结构与选型

**语言：Go**——单静态二进制、交叉编译简单，且 pty（`creack/pty`）、SFTP（`pkg/sftp`）、多路复用（`hashicorp/yamux`）、FUSE（`hanwen/go-fuse`，v2）都有成熟库。许可证建议 Apache-2.0。

```
cmd/waldo/              单一多入口二进制
internal/
  cli/                  run / setup / doctor / allow / ports / log / link
  session/              A 端编排：沙箱 + 传输 + 各流的接线
  transport/            传输接口；ssh 实现；远端自动安装
  proto/                流类型与消息定义、版本握手
  host/                 B 端：胶囊构建、pty、挂载管理、stub 汇聚
  stub/                 B 端：进程启动转发
  serve/                A 沙箱内：SFTP 服务、exec 服务、端口探测
  sandbox/              沙箱接口；bwrap（linux）、seatbelt（darwin, v2）
  netpolicy/            出网代理、端口转发、策略
  termfilter/           终端转义序列过滤
  profile/              内嵌的 Agent profile
  audit/                审计日志
test/
  e2e/                  用一个假 Agent 跑通全链路（本机回环充当 A 和 B）
  attack/               沙箱回归测试：读 ~/.ssh、连宿主机端口、写 .git/hooks、
                        访问未放行域名、从 B 侧请求 SSH 转发……必须全部失败
docs/                   DESIGN.md、SECURITY.md、profiles.md
```

依赖要求：

| 机器 | 需要 |
|---|---|
| A（Linux） | `ssh`、`bubblewrap`；使用 claude profile 时需要 `rg`、`git` |
| B（Linux） | `sshd`、`bubblewrap`、`sshfs`（fuse3）、已安装的 Agent |

`waldo doctor` 逐项检查并给出对应发行版的安装命令。

---

## 10. 设计取舍

| 决策 | 选择 | 放弃的方案及原因 |
|---|---|---|
| 工具怎么落到 A | 环境投射（FUSE + stub） | **MCP 中继**：要替换 Agent 的原生工具，丢失其调优，且每个 Agent 做法不同。**Agent 专用 hook**（如 shell 前缀变量）：不通用，且漏掉写死绝对路径和直接起子进程的情况 |
| 传输 | 系统 SSH 的 stdio | **自建 hub + mTLS**：多一个常驻服务、一个端口、一套证书管理，而用户本来就有 SSH |
| 身份隔离 | 沙箱没有任何通往 B 的通道 | **双证书**：能达到同样目的，但更复杂；现在沙箱只持有一根被动的管道 |
| 文件协议 | SFTP（`pkg/sftp` + `sshfs passive`） | **自定义 FUSE RPC**：v1 要多写一整个文件系统；SFTP 是标准协议，可调试，可平滑换成自研客户端 |
| A 端隔离 | bubblewrap | **VM**：多占数 GB、要重装工具链、启动慢。后端是接口，保留升级余地 |
| 路径约束 | 靠挂载命名空间 | **代码里做路径校验**：容易出竞态和符号链接漏洞 |
| 工作区 | 默认直接用项目目录 | **强制 worktree**：安全但偏离"像原生一样"，改为可选的 `--isolated` |
| B 端环境 | bwrap 胶囊 | **直接在 B 上跑**：路径对不齐需要 root 建目录；绝对路径的 `/bin/bash` 会悄悄在 B 上执行 |

---

## 11. 已知限制与待验证项

**限制（v1）**

- 项目目录之外的文件，Agent 的进程内读写工具看不到（通过命令 `cat` 仍可读沙箱可见的部分）。需要时用 `expose_ro` 追加目录。
- 沙箱内无 Docker、无 git 凭证、无图形界面/剪贴板访问（粘贴图片等功能不可用）。
- 只有遵守 `HTTP(S)_PROXY` 的程序能出网；SSH 方式的 git remote 在沙箱内不可用。
- 键盘回显延迟 = 到 B 的网络往返，与 SSH 相同。
- A 为 macOS 而 B 为 Linux 时，Agent 自述的平台与工具实际运行的平台不一致，需要在 profile 里追加一段提示说明"以命令执行环境为准"（v2 处理）。
- 胶囊内被直接执行（不经 shell）的项目内原生二进制不会被转发。

**实现时需逐项实测确认**

| 项 | 假设 | 若不成立 |
|---|---|---|
| Claude Code 的 shell 快照与 cwd 记录文件位置 | 分别在 `~/.claude/shell-snapshots` 和 `/tmp` | 调整 profile 的 `shared` 列表 |
| `USE_BUILTIN_RIPGREP=0` 使其改用 PATH 上的 `rg` | 成立 | 退回到内置 rg 经 FUSE 扫描（慢但可用） |
| Codex 自带沙箱会妨碍 stub | 需要关闭 | 去掉 profile 里的参数 |
| **时钟偏差** | Agent 用 B 的时钟与文件 mtime（A 的时钟）比较，偏差大时会误报"文件已被修改" | 握手时检测并告警；v2 在投射层按偏差校正 mtime |
| 关闭缓存后的 FUSE 延迟可接受 | 进程内文件操作量小 | 提前做 v2 的缓存客户端 |
| Codex 等 Agent 的首次登录在无浏览器的 B 上可完成 | 有设备码/粘贴验证码流程 | `waldo login` 临时转发回调端口 |

---

## 12. 里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| **M0 沙箱** | bwrap 策略、出网代理、端口转发、`test/attack` | 攻击测试全部失败；沙箱内 `npm install && npm start`，宿主机浏览器可访问 |
| **M1 传输与终端** | SSH 传输、自动安装、多路复用、pty 转发、B 端胶囊（暂无投射） | `waldo run -- <TUI 程序>` 在 B 胶囊内运行，显示与按键正常 |
| **M2 执行转发** | stub、exec 流、信号与退出码 | 胶囊内 `bash -c 'uname -n; exit 3'` 输出 A 的主机名并返回 3 |
| **M3 文件投射** | 内嵌 SFTP 服务、sshfs passive 挂载 | `waldo claude` 端到端可用：读、改、跑测试、起服务联调 |
| **M4 好用** | profile（claude、codex）、`doctor`、`link`、`allow`、端口自动转发、转义过滤、审计日志 | 新机器从安装到用上 ≤ 3 条命令 |
| **M5 之后** | macOS 作为 A；脱离/重连；自研 FUSE 客户端与缓存；`--isolated` | — |
