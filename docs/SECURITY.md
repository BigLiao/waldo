# waldo 安全说明

## 信任边界

```
可信：A 宿主机上的 waldo 进程（字节搬运、终端过滤、出网/端口策略、审计日志）
─────────────────────────────── 边界 ───────────────────────────────
不可信：B 的一切、A 沙箱内的一切（含 sandbox-serve 自身）、模型输出
```

设计假定 B 已被攻破、沙箱内进程已被控制，此时仍要保证 A 宿主机无恙。

## 可信进程做了什么、没做什么

| 方向 | 可信进程的处理 |
|---|---|
| B → A 的流 | 接受 `fs`（SFTP）和 `exec`；显式使用 `--paseo` 时接受当前会话的 `paseo` MCP 流。旧的 `fs-watch` 订阅和其他未知流一律关闭并记日志（`TestRejectUnknownStreamsFromB`、`TestLegacyCacheNotificationsAlwaysRejected`） |
| 沙箱 → 宿主机的流 | 只接受 `connect`（出网代理）和 `hostport`（声明过的宿主机端口） |
| 出网 | 按域名白名单放行；解析后拒绝回环/内网/链路本地/CGNAT 地址；直接按解析出的 IP 连接（无二次解析） |
| 端口转发 | 只在 `127.0.0.1`/`::1` 上监听；≥1024、非数据库等知名端口、宿主机上空闲才转发 |
| 终端 | Agent 输出：丢弃 OSC 52（剪贴板）、未列入白名单的 OSC、DCS/APC/PM/SOS、回显标题的 CSI 20t/21t；ssh 转发的 B 端 stderr 同样过滤；握手错误、doctor 报告等结构化字段去掉全部转义序列和控制字符 |
| SSH | `-T -e none`、`ForwardAgent=no`、`ForwardX11=no`、`ClearAllForwardings=yes`、`PermitLocalCommand=no`、`Tunnel=no` |

路径约束不靠代码：SFTP 服务端和命令执行都在内核沙箱里。Linux 通过挂载限制可见范围，macOS 通过 Seatbelt 拒绝未经授权的读取与写入。

文件缓存只使用原 SFTP 流中的只读版本校验扩展，不使用目录监听、通知订阅或后台推送。A 的沙箱实际打开文件并返回该句柄的纳秒修改时间、大小及文件身份；缓存命中也必须重新打开并校验，因此不跳过 A 的打开权限检查。写入和其他文件操作继续由原 SFTP 服务处理，不扩大宿主机访问范围。按时间戳判断属于弱一致性，不构成内容完整性证明；同时间戳、大小和身份的改写可能漏检，打开后的缓存快照也不保证实时反映并发修改。

通过配置或 `--observe` 开启模型统计时，额外接受当前会话的单个 `observe` 数值统计流，不能附带文件路径、执行请求或连接目的地。A 只记录经过范围和单调性检查的固定计数与耗时，不提供额外宿主机访问能力。B 的临时模型遥测接收器只监听回环地址，路径使用随机会话令牌；它不转发任何 HTTP 请求，也不记录原始负载。统计来自不可信 B 和客户端遥测，适合排查性能，不能充当完整性或安全证明。

Claude 的 B 侧工具计时同样由 `--observe` 开启，通过临时内联 settings 添加公开 HTTP hooks；不执行 hook shell、不修改持久配置、不返回权限决策或上下文。接收器使用 B 回环地址和随机会话路径，只将固定工具标识、状态及耗时异步写到 B 的 `log/tool-timing.jsonl`，原生 hook 的完整输入输出仅在本地接收时被消费，不落盘或转发 A；负载、并发、队列和未完成调用数量均有上限。事件可缺失或被 B 伪造，不作为审计完整性保证；已有 hooks 和组织策略仍然生效。

## 可选远端命令模式

`[execution] mode = "remote"` 或 `--exec-mode remote` 将普通 Bash 子命令留在 B 的胶囊中执行。B 的标准工具目录只读可见，B 的 HOME 和根目录不会整体挂载；项目仍经原 SFTP 投影，所有写入影响 A 的工作区。B 的普通命令沿用 B 的网络，不受 A 的出网白名单限制，也不写入 A 的命令执行日志。

注入的 `Bash` 是 Waldo 二进制自带的 stdio MCP，不开放 HTTP 端口，也不读取或转发其他全局 MCP 配置。其默认命令经过现有 Unix socket、SSH exec 通道及 A 沙箱执行，显式 `location=agent` 则在 B 胶囊内执行，不新增对 A 的访问权限；命令环境取自 A 的原始沙箱环境，再执行原有凭证清理与网络代理策略，不把 B 的环境变量传给 A。工具选择文案仅供 Agent 使用，不能作为权限边界；超时和取消通过关闭通道终止未退出的命令，已完成的文件修改不会回滚。

## 可选 Paseo 桥接

`waldo claude --paseo` 显式授权远端 agent 使用启动它的 Paseo MCP。必须由 Paseo 提供 HTTP MCP 配置、`callerAgentId` 和 Bearer 认证；缺少时启动失败，不会降级为仅投影 skills。版本和登录状态诊断命令不启动桥接。

- 本机固定转发目标为启动配置中的回环地址 `/mcp/agents`，固定调用者和认证；远端不能改成其他地址、路径、身份或任意宿主机端口。HTTP 重定向不跟随，MCP 会话 ID 只能使用当前桥接收到的值。
- 真实 Bearer 凭据保留在本机进程内存中，不发送给远端；MCP 配置从启动审计日志中脱敏。远端入口使用独立随机认证、临时回环端口，随 Waldo 会话关闭，转发复用原 SSH 连接。
- 本机 `~/.claude/skills/`（优先）和 `~/.agents/skills/` 中已安装的 `paseo`、`paseo-*` 目录只读挂载到 agent 的技能目录，保留引用文件；不继承其他全局 MCP、skills、登录文件或整个主目录。
- **这是额外的能力授权，不受项目文件沙箱约束。** Paseo 工具可以管理其他 agents 和工作区；这些 agents 是否受 Waldo 约束，取决于它们选择的 provider。凭据留在本机不能阻止远端通过已授权工具执行操作。仅在接受该权限范围时开启 `--paseo`。

## A 沙箱（bubblewrap）

- 独立 user/pid/net/ipc/uts/cgroup 命名空间，`--cap-drop ALL`、`--new-session`、`--die-with-parent`。
- 网络命名空间里只有 `lo`；抽象 unix socket（X11、D-Bus 会话总线）也因此不可达。
- `/usr`、`/etc` 只读；`$HOME` 换成 `~/.local/share/waldo/sandbox-home`；工具链目录只读。
- 项目目录读写；已存在的 `.git/hooks`、`.git/config`、`.git/info`、`.envrc`、`.vscode/`、`.idea/`、`.devcontainer/` 只读；`.git` 绑定到自身，不能被改名替换。
- 策略文件（`~/.config/waldo/`）和审计日志（`~/.local/share/waldo/audit.log`）在沙箱内不可见。

WSL2 项目应放在 Linux 文件系统中。Waldo 拒绝把 Windows 挂载盘上的项目或项目内的 Windows 子挂载交给可写沙箱，因为不同路径可能访问同一个文件并使只读保护失效。[Windows 与 WSL2 使用说明](configuration.md#windows-与-wsl2)

在 WSL2 中，`net.windows_ports` 显式授权沙箱访问 Windows 上指定的 IPv4 回环 TCP 端口。可信 A 启动使用固定脚本的 Windows PowerShell 桥接，流内容作为 TCP 数据处理。连接会记录 `windows-port` 审计事件。获准的端口可能提供调试、修改或执行能力，应按项目只授权所需端口。

## A 沙箱 macOS Seatbelt

Mac 使用默认拒绝策略，允许项目、私有 HOME/TMPDIR 读写，系统工具链只读。受保护路径同时拒绝写入、创建与替换；测试覆盖 `.git` 大小写别名和 Unicode 等价路径。不存在 PID、网络或挂载命名空间，不承诺 Linux 的进程和目录不可见性。

Mac 出网代理只在回环随机端口监听，HTTP 与 CONNECT 均要求会话认证，转发前移除认证头；连接关闭时撤销代理并清理既有连接。凭据不进入发送给 B 的 Facts。未经配置的外网、回环目标及 DNS IPC 不放行；共享端口的隔离差异见 [macOS 使用说明](macos.md)。

B 的登录 shell 凭证、模型和服务配置只加载到 B Agent；profile 的 scrub 清单继续确保它们不进入 A 的转发命令环境。B 程序版本并存不改变 Agent 状态共享范围。

原生进程组收到 TERM，2 秒后仍未退出则 KILL，可信 A 等待最多 5 秒；脱离进程组的 daemon 不保证全部清除。它继承 Seatbelt 权限，但仍可能继续改写已授权项目或使用已允许端口。这里只保证受管命令的生命周期，不能把退出解释为撤销全部文件权限。

## 回归测试

`test/attack` 里每一项都必须失败：读 `~/.ssh`、读项目外文件、读/改 waldo 配置和 allow 列表、读审计日志、写 `.git/hooks`、`git config core.fsmonitor`、替换 `.git`、写系统目录、连接未声明的宿主机端口、经代理访问未放行域名、经代理访问宿主机 IP、白名单域名解析到回环、绕过代理直连、直接 DNS、抽象 unix socket、看到宿主机进程、看到宿主机网卡。另有正向对照（写项目文件、git、声明过的宿主机端口、白名单域名）确保失败来自策略而不是测试本身坏了。

## 已知残余风险

- 共享内核：内核漏洞可导致沙箱逃逸。
- 默认模式下项目目录可写：Agent 可以改坏工作区，也可以修改 `package.json` scripts、`Makefile` 等你之后会在宿主机上运行的内容——运行前先看 `git diff`。
- **Linux 会话开始时不存在的受保护路径**（如新建 `.envrc`、在非 git 目录里 `git init`、`.git/commondir`）无法预先设为只读；waldo 在会话结束时检测到它们出现/被替换会打印警告并记日志，但不会阻止。
- 终端仍会显示 B 发来的普通文本，可能具有误导性。
- B 上的 Agent 登录态属于 B 自身的损失面。
