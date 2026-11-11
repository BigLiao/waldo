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

[net]
allow = ["api.staging.example.com"]   # 在内置白名单（常见包仓库、GitHub）之上追加
ports = "auto"                        # "auto" | [3000, 5173] | "off"
host_ports = [5432]                   # 沙箱可访问的宿主机服务

[project."/home/me/work/shop"]
net.allow = ["api.example.com"]
net.host_ports = [5432, 6379]
fs.metadata_cache_ttl = "30s"      # 当前项目覆盖全局 TTL

[agent.claude]                        # 覆盖/新增 Agent profile，见 docs/profiles.md
env = { CLAUDE_CODE_MAX_OUTPUT_TOKENS = "16000" }
```

`waldo allow` 写的是 `~/.config/waldo/allowed.json`，与 `config.toml` 合并。

## 文件投射性能

项目目录默认启用 10 秒的内核文件属性和路径查询缓存，减少重复 `stat` 的跨机往返。目录列表和不存在文件的查询不缓存，`/tmp`、Agent 共享运行目录、会话说明目录保持零元数据缓存。保留同步写入，不新增跨次打开的内容缓存。

使用时按约定人工避免 A/B 同时修改项目。A 上编辑器或命令（包括 Agent 转发到 A 执行的命令）修改文件后，B 的属性、大小或路径替换仍可能延迟一个 TTL 加上在途请求时间才可见；当前没有实时失效推送。更重视即时一致性的项目可关闭缓存：

```toml
[project."/home/me/work/shop".fs]
metadata_cache_ttl = "0s"
```

TTL 支持 `0s` 到 `1m` 的整数秒，全局 `[fs]` 配置可被项目配置覆盖。配置在新会话生效；正在运行的会话不会重新挂载。`waldo log` 的会话事件及 B 的 `host.log` 会记录实际 TTL，方便对照性能。

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
