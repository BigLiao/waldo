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

[execution]
mode = "host"                     # host 默认；remote 远端 Bash；host-mcp 同名 MCP 工具按 location 分发

[sandbox]
expose_ro = ["~/.config/nvim"]     # 自动探测的工具链之外再只读挂载
pass_env  = ["ANTHROPIC_MODEL", "MY_APP_*"]   # 额外传入沙箱和 Agent 的环境变量（glob）

[fs]
metadata_cache_ttl = "10s"         # 内核属性/路径缓存；host-mcp 最多 2s；"0s" 关闭
content_cache_ttl = "10s"          # 已读内容的闲置保留时间；"0s" 关闭代理

[observe]
enabled = true                   # 默认启用模型调用统计，可省略 --observe；默认 false

[net]
allow = ["api.staging.example.com"]   # 在内置白名单（常见包仓库、GitHub）之上追加
ports = "auto"                        # "auto" | [3000, 5173] | "off"
host_ports = [5432]                   # 沙箱可访问的宿主机服务

[project."/home/me/work/shop"]
net.allow = ["api.example.com"]
net.host_ports = [5432, 6379]
fs.metadata_cache_ttl = "30s"      # 当前项目覆盖全局 TTL
fs.content_cache_ttl = "30s"
observe.enabled = false          # 此项目关闭全局模型统计设置
execution.mode = "remote"        # 此项目覆盖全局命令模式

[agent.claude]                        # 覆盖/新增 Agent profile，见 docs/profiles.md
env = { CLAUDE_CODE_MAX_OUTPUT_TOKENS = "16000" }
```

`waldo allow` 写的是 `~/.config/waldo/allowed.json`，与 `config.toml` 合并。

Paseo 的 Claude SDK 流式会话会显示“Waldo 正在启动”和初始化完成耗时。初始化期间暂存用户消息，SDK 控制请求照常通过；收到真实 `initialize` 成功应答后再发送消息。提示仅显示在聊天流，不发给模型；初始化完成不代表所有异步 MCP 连接都已完成。Paseo 开始消费首轮输出流后可见；创建 Agent 页面的等待状态仍由 Paseo 控制。普通 CLI 输出保持原格式。

## 命令执行模式

`host-mcp` 禁用 Claude 内置 Bash、Read、Grep、Glob、Edit、Write、NotebookEdit，提供同名 MCP 工具（如 `mcp__waldo__Read`），参数沿用原工具，额外增加 `location = auto | host | agent`。使用 `waldo claude --paseo --exec-mode host-mcp` 或 `[execution] mode = "host-mcp"` 开启，未设置 `MCP_TIMEOUT` 时，MCP 启动等待上限为 180 秒，以容纳投影文件造成的启动阻塞。项目挂载仍用于说明和 skills 加载；B 保留原生 shell，仅启动时 PATH 中的 Git 探测转发 A。

文件工具默认按挂载来源分发：项目/共享路径走 A，B 的 memory、会话记录和 `/tmp` 留在 B；项目位于 `/tmp` 下时该子目录仍走 A。`host` 强制访问 A，`agent` 访问 B 本地文件（不能绕过项目投影）；找不到不会换端重试，跨两端的目录搜索需拆开。Read/Edit/Write 使用 `file_path`，Grep/Glob 使用 `pattern` 和可选 `path`，NotebookEdit 使用 `notebook_path`。先 Read 再修改已有文件，版本检查由 MCP 内部管理，不需要传 hash；外部变更后需重读，此检查不承诺与外部写入者严格互斥。

Read 默认 2000 行、文本输出最多 64 KiB，`allow_large` 提高到 1 MiB（文件上限从 16 MiB 提高到 64 MiB）。支持图片（最多 4 MiB）；PDF `pages` 目前只提取文字，需要所选端安装 pdfinfo/pdftotext。Grep 支持原有过滤、上下文、输出模式和 `head_limit`/`offset`；Glob 按修改时间排序。参数 JSON 上限 1 MiB，搜索输出上限 64 KiB；Write 自动创建父目录。

默认 `host` 沿用原行为：B 上的 PATH 命令转发到 A 的沙箱。可在全局或项目配置中选择 `remote`，也可用启动参数临时覆盖（参数放在 Agent 参数之前）：

```sh
waldo claude --exec-mode remote
waldo claude --paseo --exec-mode host  # 临时覆盖配置，恢复本机执行
```

`remote` 模式下，内置 Bash 及普通子命令在 B 的胶囊中执行，使用 B 的标准系统工具（`/usr/bin`、`/usr/local/bin` 等）；Python 等所需依赖需在 B 安装。Waldo 自动注入 stdio MCP 工具 `mcp__waldo__Bash`，需要 A 的虚拟环境、构建测试依赖或本机服务时调用它。工具复用当前 SSH 的原有 exec 通道，默认在 A 沙箱执行，`location=agent` 可指定 B；不增加 MCP 安装步骤或宿主机权限。与 `--paseo` 可同时使用；目前内置 Claude 支持，自定义 profile 需声明 `mcp_config_flag = "--mcp-config"`，使用相同配置格式。Codex 和空 profile 暂不支持此模式，可用 `--exec-mode host` 覆盖全局设置。

`Bash` 使用原有 `command`、`timeout`（毫秒）、`description`、`run_in_background` 和 `dangerouslyDisableSandbox` 参数，加 `location`。auto 默认 A，agent 使用 B 的工具链/网络/tmp；A 使用原有沙箱环境，不继承 B 凭证。每次命令独立，切换目录用 `command` 中的 `cd`，不提供 `cwd` 参数。前台默认 120 秒、最多 10 分钟；后台默认 30 分钟、最多 2 小时，用同一 MCP 的 TaskOutput/TaskStop 查询和取消，无自动完成通知。两端均保留沙箱限制，`dangerouslyDisableSandbox=true` 报错；退出 MCP 会取消任务。stdout/stderr 各最多 128 KiB，超限标记 truncated。

项目仍是同一份投影：缓存未命中的读和所有写入都经过 A，远端 Bash 写文件也会修改本机工作区。`waldo.md` 会按实际模式说明两种工具的用法；关闭 `[context] enabled` 时不注入该指南。B 的 Bash 使用 B 的网络和 localhost，A 的网络白名单、宿主机服务与开发端口转发适用于 `Bash`。实际加速取决于缓存命中、文件访问量及依赖是否适合在 B 运行。

在 A 执行的 `Bash` 调用写入 A 的 `exec` / `exec-end`，额外标记 `tool = "Bash"`，可按会话统计调用次数、平均/最大耗时、退出码和中断状态；`waldo log -f | rg 'tool=Bash'` 可单独查看。`exec-end.duration_ms` 仍是 A 接收请求到观察到执行结束的耗时，不包含完整往返。开启 `--observe` 后，B 的 `tool-timing.jsonl` 同时按 `tool = "mcp__waldo__Bash"` 记录原生工具耗时与 hook 间隔（范围见下文）；B 的普通 Bash 不写入 A 的 exec 日志。MCP 另写 `kind=tool-execution`，包含 `location` 和 B 观察到的执行耗时，不记录参数或内容；它和原生 Hook 事件是不同统计口径，不要累加。

## 轻量运行日志

无需额外服务，仍使用本机的 `audit.log` 和原有查看命令：

```sh
waldo claude --observe     # 或 waldo codex --observe；也可通过配置默认开启
waldo log -f               # 在另一个终端查看；-s <会话ID> 筛选单次会话
```

在本机 `~/.config/waldo/config.toml` 中设置后，普通的 `waldo claude` / `waldo codex` 就会启用模型统计：

```toml
[observe]
enabled = true

# 可选：按项目覆盖全局设置
[project."/home/me/work/shop".observe]
enabled = false
```

优先级是命令行 > 当前项目配置 > 全局配置，未配置时默认关闭。`--observe` 或 `--observe=true` 临时开启，`--observe=false` 临时关闭，新会话生效。命令和文件读写日志始终记录，不受这个开关影响。

| 日志类型 | 内容与耗时范围 |
|---|---|
| `exec` / `exec-end` | 关联 ID、程序名、退出状态、耗时；从 A 可信客户端收到命令请求，到观察到退出结果，包含沙箱执行和输出收尾，不是纯程序运行时间，也不包含完整的 B 端工具调用时间 |
| `file-read` / `file-write` | 每 5 秒汇总已完成的 SFTP 读写请求次数和累计耗时，流结束时补一次汇总；包含 OPEN 和属性、目录等请求，次数不等于文件数，失败回复也算完成；`scope=sftp_rpc` |
| `model-call` | 通过配置或 `--observe` 开启后，原生客户端报告的模型请求次数、错误数和累计 `duration_ms`；Codex 的流事件处理时间另列 `stream_event_ms`，不能将它或 HTTP 请求耗时当作完整生成时长 |

文件统计位于 A 的转发边界，只覆盖实际到达 A 的 RPC；B 的缓存命中，以及 A 上 shell 子进程直接进行的本地文件访问不在其中。各项耗时可重叠，尤其多个文件请求并发时，累计值可能超过会话时长；不能将这些值直接相加或当作会话占比。未观测到操作的时间不标记为“模型思考”。断流的命令记为 `disconnected`，没有退出帧时不推测退出码；未完成或超出统计容量的文件请求另记 `file-stats-dropped`。

模型统计复用 [Claude Code](https://code.claude.com/docs/en/monitoring-usage) 和 [Codex](https://learn.chatgpt.com/docs/config-file/config-advanced) 自带的异步 OpenTelemetry 日志，B 上临时启动仅监听回环地址、使用随机会话路径的 JSON 接收器，只提取固定数值字段，再复用现有 SSH 连接批量传到 A。不代理模型 API、不增加模型请求、不注入 MCP，也不修改持久配置。启用模型统计会为本次进程配置本地日志 exporter；需要保留原有日志 exporter 时不要开启。模型输入、响应和工具内容不写入新增统计日志；原有命令与写操作审计保持原样。

未配置时默认不开启客户端原生遥测；命令和文件统计始终保留。遥测导出、转发是尽力而为的，断线、接收器不可用或 Agent 版本不支持相关事件时可能缺少模型统计，不会回退为估算模型时间，也不会阻止命令和文件操作。自定义 Agent 可配置 `telemetry` 适配器，见 [profiles.md](profiles.md)。普通遥测事件经过批处理，日志出现时间会晚于实际操作；本实现没有逐请求文件明细、全量进程跟踪或模型调用的关键路径分析。

### B 侧工具耗时

Claude profile 在启用 `--observe` 时额外注入公开的 HTTP `PreToolUse`、`PostToolUse`、`PostToolUseFailure` hooks，接收器只监听 B 的回环地址。日志只写 B 的 `~/.local/share/waldo/log/tool-timing.jsonl`（自定义 `remote_dir` 时为其 `log/tool-timing.jsonl`），不转发到 A，不修改 Claude、SDK 或持久 settings。

按 `s`（Waldo 会话）、`agent_session_id` 和 `tool_use_id` 配对 `tool-start` / `tool-end`。`duration_ms` 是 Claude 可选提供的工具执行耗时，不含权限等待和 PreToolUse hooks；`hook_elapsed_ms` 是 B 上两次 hook 到达之间的间隔，包含期间的等待，使用同一进程的单调时钟。字段缺失时不估算，缺少结束事件的调用标为 `tool-incomplete`，不是已完成耗时。可按工具类别汇总次数、平均和最大值。这仍不覆盖 PreToolUse 前的调度或 PostToolUse 后的处理，也不是 UI 端到端耗时；后台工具的 hook 完成可能仅代表已提交后台任务。

统计保留工具名、关联 ID、状态和耗时，不保存 hook 中的命令、输入输出、路径或错误正文。原生 HTTP hook 会将完整负载发到本地接收器；超过 4 MiB 的事件跳过。请求仅入有界队列，写盘异步进行，队列满或负载无效时丢统计并记录 `dropped`；hook 超时为 1 秒，响应不包含权限决策或上下文。

临时 settings 会保留已有内联 settings 和 hooks；遇到 `--settings <文件>` 或显式 `disableAllHooks` 时跳过工具观测并在 B 的 `host.log` 说明，不覆盖原配置。原生版本不支持 HTTP hooks、已有 `allowedHttpHookUrls` 或组织策略禁止时也可能没有事件；Waldo 不绕过这些限制。设置 profile 的 `tool_hooks = "none"` 可只保留模型统计。

## 文件投射性能

项目默认使用 B 上的会话内 SFTP 内容缓存。每次只读打开项目文件时，A 在沙箱内实际打开文件，取得该句柄的纳秒级修改时间、大小、设备及 inode，并在一次 SFTP 扩展请求中返回句柄和版本。B 逐文件比较版本：一致则复用缓存内容，不一致才在后续实际 READ 请求中取新内容。打开文件本身不预读正文，也不主动补全部分读取的文件。到达代理的属性查询和目录列表始终回源，删除、重命名和权限变化由 A 的实际文件操作处理。

A 不再安装项目目录监听器，链路没有缓存通知、心跳或命令完成失效屏障；其他进程持续修改日志等无关文件不会清空源码缓存。所有写入仍由 A 原有 SFTP 服务执行；每次后续打开独立校验，无需先推送或全局清缓存。校验与打开使用同一个文件句柄，已有只读句柄可以保持本次打开时的缓存快照，重新打开才检查路径当前版本。

这是时间戳校验的弱一致性缓存，不计算内容哈希：保留相同精确修改时间、大小和文件身份的改写可能漏检，底层文件系统时间分辨率较粗时也有此限制。读取过程中发生并发改写不保证原子快照。填充缓存时只收集实际读取过的数据，在关闭时再次检查同一文件句柄版本，仅将完整且版本未变的小文件收入缓存；关闭校验与 CLOSE 合并为一个请求。热读仍有打开校验和关闭往返，节省的是正文传输，不能保证所有工作负载加速。断线中的新打开等待原传输恢复，不能绕过校验使用旧内容。

缓存只用会话内存，总数据预算 32 MiB，单文件最多 1 MiB，最多 256 项，通过容量上限和 LRU 淘汰；所有打开快照与进行中的填充也计入数据预算。文件关闭后不为缓存保留 A 的句柄。`content_cache_ttl` 默认 `10s`，改为闲置保留时间，成功复用后重新计时；它不再决定内容是否最新。特殊文件、大文件及不支持校验扩展的服务直通原 SFTP。`/tmp`、Agent 共享运行目录和会话指南目录不使用项目代理。

`host`/`remote` 的代理模式使用 `direct_io`，关闭 SSHFS 的属性、目录和内容页缓存，以确保真正打开时会执行校验。`metadata_cache_ttl` 在该模式下不缓存代理内路径属性；关闭内容代理后才控制 SSHFS 内核属性/路径缓存。冷读、大文件可能失去页缓存收益；需要直接挂载时可配置：

```toml
[project."/home/me/work/shop".fs]
metadata_cache_ttl = "0s"
content_cache_ttl = "0s"
```

两个 TTL 均支持 `0s` 到 `1m` 的整数秒，全局 `[fs]` 配置可被项目配置覆盖，新会话生效。为兼容旧配置：如果有效的 `metadata_cache_ttl` 是显式设置的 `0s`，且全局和当前项目都未显式设置 `content_cache_ttl`，内容缓存也保持关闭。显式的内容 TTL 优先。单独设置 `content_cache_ttl = "0s"` 会恢复旧的直接 SSHFS 模式，此时 `metadata_cache_ttl` 继续控制内核属性/路径缓存，A 上的变化可能在 TTL 内不可见。两个值都设为零才同时关闭这些缓存。`waldo log` 的会话事件及 B 的 `host.log` 记录配置，方便排查。

`host-mcp` 对项目及显式只读 skill 挂载启用短缓存：属性/目录项最多 2 秒，不存在的路径最多 1 秒；均受 `metadata_cache_ttl` 限制，设为 `0s` 可关闭。内容代理开启时也生效，仍保留 `direct_io` 和逐文件打开校验；不缓存目录列表，不增加文件监听或推送。自动加载的配置/skills 可能延迟这段时间才看到变化，MCP 对 A 的文件读写直接执行，不经过挂载缓存。共享运行目录和会话指南保持无元数据缓存。B 的 `host.log` 同时记录实际属性缓存和负缓存 TTL。

大型仓库、目录遍历和 Git 仍可能产生大量元数据往返，按文件校验主要减少正文重传，并不保证它们变快。普通命令建议保持默认 `host` 模式；实际收益应以真实双机和 FUSE 工作负载衡量，不能把缓存命中率直接当作整个 Agent 的提速比例。

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
