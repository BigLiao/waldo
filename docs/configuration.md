# 配置参考

> 面向使用者的入门见 [README](../README.md)。本文收录初始化细节与全部配置项。文中 **A** 指你正在使用的本机，**B** 指运行 Agent 的服务器。

## 命令一览

| 命令 | 作用 |
|---|---|
| `waldo setup <host> [--adopt-login=false] [--skip-deps]` | 自动安装两端缺少的系统依赖，写入 `host`，安装/升级 B 端二进制，默认复用 B 上已有的 Agent 配置与登录，运行 doctor；`--adopt-login=false` 停止共享已有状态目录；`--skip-deps` 跳过系统依赖安装 |
| `waldo doctor` | 两端依赖、bwrap 能否建沙箱、Agent 是否安装/已有登录态、时钟偏差 |
| `waldo allow [--global] <域名>` | 给当前项目（或所有项目）放行出网域名，运行中的会话立即生效 |
| `waldo ports` | 当前会话转发到 `127.0.0.1` 的端口 |
| `waldo log [-n N] [-f] [-s 会话]` | 审计日志：执行过的命令、写过的文件、出网、端口 |
| `waldo link [--remove] <agent>` | 生成 `<agent>` → `waldo <agent>` 的包装脚本 |
| `waldo dev-sandbox -- <命令>` | 调试用：只起 A 端沙箱（同样的策略），在里面跑一条命令 |

## 初始化与自动安装

在 A 上运行一次即可：

```sh
./waldo setup me@my-server
```

`setup` 先检查 A 的 `ssh`、`bwrap` 和各 Agent profile 的 `needs`，再通过 SSH 检查 B 的 `bwrap`、`sshfs`、`fusermount3`。缺少时会显示待安装的软件包，调用系统包管理器安装，然后重新检查命令是否可用；依赖齐全时不会调用包管理器。支持 `apt-get`、`dnf`、`yum`、`pacman`、`apk`，包括使用 `dnf` 的 TencentOS。

安装系统包需要 root 或 sudo 权限。在终端中运行时可以输入两端各自的 sudo 密码；无终端时使用 `sudo -n`，权限不足会立即停止并给出该机器上的手工安装命令。需要自行管理依赖时用 `./waldo setup me@my-server --skip-deps`，仍会运行 `doctor`。自定义 profile 中未知的缺失命令会提示手动安装。

B 的 SSH 服务和 Agent 程序需要提前准备。`setup` 默认复用各 Agent profile 的 `state` 中已有的配置与登录：状态目录直接绑定进胶囊，单个文件首次复制到胶囊 HOME，已有非空副本会保留。仍兼容显式 `--adopt-login` 或 `--adopt-login=true`；使用 `--adopt-login=false` 可停止已有状态目录的共享，保留 B 上的原文件、之前复制的文件及胶囊独立状态。如果没有已有登录，可在首次启动 Agent 时完成登录。

安装后 `doctor` 会继续检查命名空间及 B 的 `/dev/fuse`。初始化不会修改内核参数、系统安全策略或启动系统服务。

安装脚本也可以在源码目录单独运行，用于只准备某一端：

```sh
sh internal/bootstrap/install.sh a bash git rg   # 在 A 上运行
sh internal/bootstrap/install.sh b               # 在 B 上运行
```

## 网络中断与重连

运行中的会话默认启用 **90 秒限时重连**，无需配置。连接被检测为中断后，A 保留本地沙箱，B 保留原 Agent、终端和文件挂载；SSH 按退避间隔自动重试，重试间隔最长 5 秒。物理连接有心跳，静默断网通常约 5 秒后进入重连窗口。

重连继续使用原会话，已打开的 SFTP 文件句柄和命令通道保持不变，不会重新启动 Agent 或重新执行命令。断线期间，远端 Agent 的模型请求和本地已运行的命令可以继续；跨机文件访问、工具调用和输出可能等待恢复。每个方向最多缓冲 4 MiB，缓冲满后暂停写入，避免无限积压。

恢复成功后继续传输；90 秒内未能恢复则结束会话、清理进程和挂载，并返回退出码 `255`。等待期间按 `Ctrl+C` 可主动取消；如果链路不可达，B 最晚在自身的重连窗口到期后清理。已经写入项目的文件保留，之后可用 Agent 自带的对话恢复功能重新启动。

重试 SSH 使用非交互认证，不会在 Agent 界面里再次询问密码。请配置 SSH 密钥或可用的 ssh-agent；如果重试时无法完成非交互认证，会持续失败直到超时。关闭或杀掉本地客户端后暂不支持重新 attach；远端会话在窗口到期后退出。

## 配置文件

全部在 A 的 `~/.config/waldo/config.toml`，B 上没有配置文件：

```toml
host = "me@my-server"
# ssh_args   = ["-F", "/path/to/ssh_config"]   # 追加给 ssh 的参数（也可用环境变量 WALDO_SSH_ARGS）
# remote_dir = "~/.local/share/waldo"           # B 上的数据目录

[sandbox]
expose_ro = ["~/.config/nvim"]     # 自动探测的工具链之外再只读挂载
pass_env  = ["ANTHROPIC_MODEL", "MY_APP_*"]   # 额外传入沙箱和 Agent 的环境变量（glob）

[fs]
metadata_cache_ttl = "10s"         # 项目属性/路径缓存，默认 10 秒；"0s" 关闭
content_cache_ttl = "10s"          # SFTP 内容缓存最大年龄；"0s" 关闭代理、沿用旧挂载方式

[net]
allow = ["api.staging.example.com"]   # 在内置白名单（常见包仓库、GitHub）之上追加
ports = "auto"                        # "auto" | [3000, 5173] | "off"
host_ports = [5432]                   # 沙箱可访问的宿主机服务

[project."/home/me/work/shop"]
net.allow = ["api.example.com"]
net.host_ports = [5432, 6379]
fs.metadata_cache_ttl = "30s"      # 当前项目覆盖全局 TTL
fs.content_cache_ttl = "30s"

[agent.claude]                        # 覆盖/新增 Agent profile，见 docs/profiles.md
env = { CLAUDE_CODE_MAX_OUTPUT_TOKENS = "16000" }
```

`waldo allow` 写的是 `~/.config/waldo/allowed.json`，与 `config.toml` 合并。

## 轻量运行日志

无需额外服务，仍使用本机的 `audit.log` 和原有查看命令：

```sh
waldo claude --observe     # 或 waldo codex --observe；仅模型统计需要此参数
waldo log -f               # 在另一个终端查看；-s <会话ID> 筛选单次会话
```

| 日志类型 | 内容与耗时范围 |
|---|---|
| `exec` / `exec-end` | 关联 ID、程序名、退出状态、耗时；从 A 可信客户端收到命令请求，到观察到退出结果，包含沙箱执行、输出收尾和缓存失效屏障，不是纯程序运行时间，也不包含完整的 B 端工具调用时间 |
| `file-read` / `file-write` | 每 5 秒汇总已完成的 SFTP 读写请求次数和累计耗时，流结束时补一次汇总；包含 OPEN 和属性、目录等请求，次数不等于文件数，失败回复也算完成；`scope=sftp_rpc` |
| `model-call` | `--observe` 开启后，原生客户端报告的模型请求次数、错误数和累计 `duration_ms`；Codex 的流事件处理时间另列 `stream_event_ms`，不能将它或 HTTP 请求耗时当作完整生成时长 |

文件统计位于 A 的转发边界，只覆盖实际到达 A 的 RPC；B 的缓存命中，以及 A 上 shell 子进程直接进行的本地文件访问不在其中。各项耗时可重叠，尤其多个文件请求并发时，累计值可能超过会话时长；不能将这些值直接相加或当作会话占比。未观测到操作的时间不标记为“模型思考”。断流的命令记为 `disconnected`，没有退出帧时不推测退出码；未完成或超出统计容量的文件请求另记 `file-stats-dropped`。

模型统计复用 [Claude Code](https://code.claude.com/docs/en/monitoring-usage) 和 [Codex](https://learn.chatgpt.com/docs/config-file/config-advanced) 自带的异步 OpenTelemetry 日志，B 上临时启动仅监听回环地址、使用随机会话路径的 JSON 接收器，只提取固定数值字段，再复用现有 SSH 连接批量传到 A。不代理模型 API、不增加模型请求、不注入 MCP，也不修改持久配置。`--observe` 会为本次进程配置本地日志 exporter；需要保留原有日志 exporter 时不要开启。模型输入、响应和工具内容不写入新增统计日志；原有命令与写操作审计保持原样。

默认不开启客户端原生遥测；命令和文件统计始终保留。遥测导出、转发是尽力而为的，断线、接收器不可用或 Agent 版本不支持相关事件时可能缺少模型统计，不会回退为估算模型时间，也不会阻止命令和文件操作。自定义 Agent 可配置 `telemetry` 适配器，见 [profiles.md](profiles.md)。普通遥测事件经过批处理，日志出现时间会晚于实际操作；本实现没有逐请求文件明细、全量进程跟踪或模型调用的关键路径分析。

## 文件投射性能

项目默认使用 B 上的会话内 SFTP 缓存代理。完整读取过的小文件可在后续打开时直接由 B 应答，写入、截断、删除、重命名和其他修改仍转发到 A 的原有 SFTP 服务。缓存使用会话内存：内容预算 30 MiB、元数据预算 2 MiB，单文件最多 1 MiB，最多为缓存保留 256 个只读句柄；特殊文件和目录列表直通。它不是完整工作区镜像，也不跨会话持久保存。`content_cache_ttl` 默认 `10s`，是内容缓存的最大年龄，不会因反复读取而延长。缓存只有在实际 EOF 或同一文件句柄上的 FSTAT 确认完整性后才发布；关闭文件时仍没有 EOF、需要确认已捕获的内容是否完整时，首次关闭可能增加一次 FSTAT 确认往返。

A 在项目目录内递归监听文件变化，复用现有 SSH 连接推送失效通知。第一版收到变化后保守清除整个项目缓存。Agent 经 SFTP 发起的修改会先停止缓存命中，处理本机结果并清除缓存后再返回；转发命令结束时，正常情况等待本会话远端确认缓存失效；对异常订阅的单次屏障等待上限为 3 秒，超时断开并重新订阅。B 将退出结果交还 Agent 前还会同步清理一次缓存，覆盖通知流关闭和重订阅期间的时序；这不改变物理断线时等待传输恢复的行为。本机编辑器、Git 和其他进程的普通文件修改通过监听异步失效，仍存在通知传播窗口。

代理模式使用 `direct_io`，关闭 SSHFS 自身的属性、目录和内容页缓存，由代理统一控制缓存有效性。冷读、大于 1 MiB 的文件及目录列表仍回源，失去内核页缓存可能使部分大文件访问退化，依赖 mmap 的程序也需要验证；这类项目可关闭内容代理。`metadata_cache_ttl` 控制代理内成功的元数据查询缓存；不存在的路径不缓存。`/tmp`、Agent 共享运行目录和会话说明目录不使用项目代理。

初始目录监听安装期间请求直通 A，不阻塞会话启动。监听失败、事件队列溢出或通知中断时先禁用缓存并回源；A 会按有上限的退避重建完整监听，B 会重新订阅通知。只有完整监听和新的订阅握手恢复后才重新启用缓存，恢复前不会使用旧内容。降级与恢复记录在会话日志中。通知心跳有短租约，断线和恢复会清除缓存。进行中的旧请求不能在失效后重新填充缓存。监听无法覆盖任意写入方式：例如 mmap 写入、网络文件系统在其他机器上的修改、符号链接指向未监控目录，以及部分硬链接别名更新，可能需要回源读取；要求绕过缓存时可设置：

```toml
[project."/home/me/work/shop".fs]
metadata_cache_ttl = "0s"
content_cache_ttl = "0s"
```

两个 TTL 均支持 `0s` 到 `1m` 的整数秒，全局 `[fs]` 配置可被项目配置覆盖，新会话生效。为兼容旧配置：如果有效的 `metadata_cache_ttl` 是显式设置的 `0s`，且全局和当前项目都未显式设置 `content_cache_ttl`，内容缓存也保持关闭。显式的内容 TTL 优先。单独设置 `content_cache_ttl = "0s"` 会恢复旧的直接 SSHFS 模式，此时 `metadata_cache_ttl` 继续控制内核属性/路径缓存，A 上的变化可能在 TTL 内不可见。两个值都设为零才同时关闭这些缓存。`waldo log` 的会话事件及 B 的 `host.log` 记录配置，方便排查。

项目监听需要为目录建立 inotify watches，会使用本机用户的监听配额，当前没有子目录排除策略。监听覆盖不完整时继续回源而不会跳过不可读子树后照常缓存；依赖目录很大的项目可关闭内容代理，以避免长期重试或与 IDE 的监听竞争。大目录命中率、冷读、命令屏障和 mmap 行为仍需在真实双机及 FUSE 环境中评估，不能把协议层热读零上游请求视为整个 Agent 的提速比例。

## Agent 会话说明

启动 Agent 时，waldo 根据本次会话的实际配置生成只读的 `/run/waldo/context/waldo.md`，说明 A/B 的职责、项目与共享路径、命令执行位置、网络权限和已知限制。这个路径在 B 的文件视图和 A 的命令执行环境中都可读；B 上的 `WALDO_CONTEXT` 环境变量也指向它。

B 看到的项目根目录 `AGENTS.md`、`CLAUDE.md` 会保留原文，并在末尾追加一行对该说明文件的引用。文件不存在时提供只含引用的虚拟文件。这些指令文件是启动时生成的只读会话视图，不修改 A 项目中的文件、不留下 Git diff，也不会随重复启动累积引用。通过 A 端命令修改原指令文件后，下次会话会载入新内容。

可在 A 的 `config.toml` 中关闭该功能，或指定其他项目根目录指令文件（名称列表替换默认值）：

```toml
[context]
enabled = true
instruction_files = ["AGENTS.md", "CLAUDE.md", "GEMINI.md"]

# 也支持项目级覆盖
[project."/home/me/work/shop".context]
instruction_files = ["AGENTS.md", "CLAUDE.md", "AGENTS.override.md"]
```

`enabled = false` 同时关闭说明文件和引用注入；`instruction_files = []` 仅关闭指令文件注入。当前尚未实现目录排除名单，说明文件会如实提示 `.git`、`node_modules` 等仍在投射视图中；提示文字不代替权限控制。
