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
| B → A 的流 | 接受 `fs`（SFTP）和 `exec`；启用项目内容缓存时，额外接受当前项目的单个 `fs-watch` 失效订阅；显式使用 `--paseo` 时，接受当前会话的 `paseo` MCP 流。其余一律关闭并记审计日志（`TestRejectUnknownStreamsFromB`、`TestRejectCacheWatchOutsideProject`、`TestOnlyOneCacheWatchMaySubscribePerSession`） |
| 沙箱 → 宿主机的流 | 只接受 `connect`（出网代理）和 `hostport`（声明过的宿主机端口） |
| 出网 | 按域名白名单放行；解析后拒绝回环/内网/链路本地/CGNAT 地址；直接按解析出的 IP 连接（无二次解析） |
| 端口转发 | 只在 `127.0.0.1`/`::1` 上监听；≥1024、非数据库等知名端口、宿主机上空闲才转发 |
| 终端 | Agent 输出：丢弃 OSC 52（剪贴板）、未列入白名单的 OSC、DCS/APC/PM/SOS、回显标题的 CSI 20t/21t；ssh 转发的 B 端 stderr 同样过滤；握手错误、doctor 报告等结构化字段去掉全部转义序列和控制字符 |
| SSH | `-T -e none`、`ForwardAgent=no`、`ForwardX11=no`、`ClearAllForwardings=yes`、`PermitLocalCommand=no`、`Tunnel=no` |

路径约束不靠代码：SFTP 服务端和命令执行都在沙箱里，看不到的东西就是不存在。

`fs-watch` 只连接 B 与 A 沙箱内的项目监听器，不提供新的宿主机文件访问能力。可信客户端验证项目路径和订阅数量后，重新构造消息头转发，并写入 `fs-watch` 审计事件；重试订阅也必须经过相同检查。监听或通知不可用时禁用缓存，只有重新建立完整监听和通知握手后才恢复缓存命中。读缓存不绕过写审计，所有修改仍经 A 的原有服务执行。

`--observe` 额外接受当前会话的单个 `observe` 数值统计流，不能附带文件路径、执行请求或连接目的地。A 只记录经过范围和单调性检查的固定计数与耗时，不提供额外宿主机访问能力。B 的临时模型遥测接收器只监听回环地址，路径使用随机会话令牌；它不转发任何 HTTP 请求，也不记录原始负载。统计来自不可信 B 和客户端遥测，适合排查性能，不能充当完整性或安全证明。

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

## 回归测试

`test/attack` 里每一项都必须失败：读 `~/.ssh`、读项目外文件、读/改 waldo 配置和 allow 列表、读审计日志、写 `.git/hooks`、`git config core.fsmonitor`、替换 `.git`、写系统目录、连接未声明的宿主机端口、经代理访问未放行域名、经代理访问宿主机 IP、白名单域名解析到回环、绕过代理直连、直接 DNS、抽象 unix socket、看到宿主机进程、看到宿主机网卡。另有正向对照（写项目文件、git、声明过的宿主机端口、白名单域名）确保失败来自策略而不是测试本身坏了。

## 已知残余风险

- 共享内核：内核漏洞可导致沙箱逃逸。
- 默认模式下项目目录可写：Agent 可以改坏工作区，也可以修改 `package.json` scripts、`Makefile` 等你之后会在宿主机上运行的内容——运行前先看 `git diff`。
- **会话开始时不存在的受保护路径**（如新建 `.envrc`、在非 git 目录里 `git init`、`.git/commondir`）无法预先设为只读；waldo 在会话结束时检测到它们出现/被替换会打印警告并记日志，但不会阻止。
- 终端仍会显示 B 发来的普通文本，可能具有误导性。
- B 上的 Agent 登录态属于 B 自身的损失面。
