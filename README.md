# waldo

Agent（Claude Code / Codex / …）的进程和登录态放在你的服务器 B 上，它的"手"——文件读写、命令执行、端口——落在你当前这台机器 A 的沙箱里。设计见 [DESIGN.md](DESIGN.md)，接手开发请先读 [docs/HANDOFF.md](docs/HANDOFF.md)。

```sh
waldo setup me@my-server        # 一次性：补齐依赖、安装 B 端、默认复用 B 上的配置与登录、检查
cd ~/work/shop
waldo claude                    # 交互式，和原生 claude 一样
waldo claude -p "跑一下测试" | tee out.txt
waldo run -- aider              # 任意 CLI agent（空 profile）
```

| 命令 | 作用 |
|---|---|
| `waldo setup <host> [--adopt-login=false] [--skip-deps]` | 自动安装两端缺少的系统依赖，写入 `host`，安装/升级 B 端二进制，默认复用 B 上已有的 Agent 配置与登录，运行 doctor；`--adopt-login=false` 停止共享已有状态目录；`--skip-deps` 跳过系统依赖安装 |
| `waldo doctor` | 两端依赖、bwrap 能否建沙箱、Agent 是否安装/已有登录态、时钟偏差 |
| `waldo allow [--global] <域名>` | 给当前项目（或所有项目）放行出网域名，运行中的会话立即生效 |
| `waldo ports` | 当前会话转发到 `127.0.0.1` 的端口 |
| `waldo log [-n N] [-f] [-s 会话]` | 审计日志：执行过的命令、写过的文件、出网、端口 |
| `waldo link [--remove] <agent>` | 生成 `<agent>` → `waldo <agent>` 的包装脚本 |
| `waldo dev-sandbox -- <命令>` | 调试用：只起 A 端沙箱（同样的策略），在里面跑一条命令 |

## 构建

需要 Go 1.26.8+。产物是单个静态二进制（`CGO_ENABLED=0`），其中内嵌了依赖安装脚本。

```sh
make dev       # ./waldo，版本号带二进制哈希：每次重编译后会自动重装到 B
make build     # ./waldo，版本号用 git 提交号
make release   # dist/waldo-linux-{amd64,arm64}；B 的架构与 A 不同时，A 会找同目录下的这些文件
```

依赖：A 需要 `ssh`、`bubblewrap`（claude profile 还需要 `rg`、`git`、`bash`）；B 需要 `sshd`、`bubblewrap`、`sshfs`（fuse3）和已安装的 Agent。

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

## 配置

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

## 测试

```sh
make test-unit     # 纯单元测试
make test-attack   # M0：沙箱攻击回归测试，每个攻击都必须失败（另有正向对照）
make test-e2e      # M1–M3：用假 Agent 跑通 A → ssh → B 胶囊 → stub → A 全链路
```

e2e 测试自己起一个只接受临时密钥的用户态 `sshd`（随机端口），A/B 的数据和配置目录都放在 `~/.cache/waldo-test/run-*`，结束后删除，不碰 `~/.ssh` 和 `~/.local/share/waldo`。A、B 在同一台机器上时，主机名和路径都一样，所以测试用**网络命名空间**判断代码在哪执行：A 沙箱有自己的网络命名空间，B 胶囊用的是宿主机的。

## 代码结构

```
cmd/waldo/            单一多入口二进制（按 argv[0] / 子命令区分四个角色）
internal/
  cli/                setup / doctor / allow / ports / log / link / run
  bootstrap/          setup 内嵌的系统依赖安装脚本与调用入口
  session/            A 端编排：沙箱 + SSH + 各流接线（可信进程）
  transport/          SSH 传输、加固参数、远端自动安装
  proto/              流头、帧、握手消息、版本
  host/               B 端：胶囊（bwrap）、sshfs 挂载、stub 汇聚、pty、doctor
  stub/               B 胶囊内：把一次进程启动转发到 A
  serve/              A 沙箱内：SFTP 服务、exec 服务、出网代理入口、端口探测
  projection/         SFTP 会话指令文件视图，普通操作交给原 SFTP 服务
  sandbox/            沙箱后端接口与 bwrap 实现、受保护路径
  netpolicy/          出网白名单、宿主机端口、端口转发（可信进程）
  termfilter/         终端转义序列过滤
  profile/            内嵌 Agent profile
  audit/              审计日志、SFTP 写操作嗅探
  config/             配置与 allow 列表
test/harness/         测试环境（临时 sshd、独立数据目录）
test/attack/          沙箱回归测试
test/e2e/             全链路测试与假 Agent
docs/                 SECURITY.md、profiles.md
```

## 实现与设计文档的差异/补充

实现过程中实测或细化过的点（DESIGN.md §11 的待验证项也在这里）：

- **stub 自己中继 stdio**，没有用 SCM_RIGHTS 把 fd 交给 `waldo host`。原因：把 Agent 的 tty fd 交出去后，host 无法安全地中断对它的阻塞读，会"偷走"命令结束后用户的下一次按键。stub 是独立进程，退出即结束读，语义更干净。stub 是 tty 时会把本地 tty 置为 raw（像 ssh 客户端一样），行规程交给 A 上的 pty。
- **stub 的身份**用 `AT_EXECFN`（内核实际执行的路径）判断；`#!` 脚本被直接执行时回退到 `argv[0]`（解释器路径）。
- **进程组语义**：stub 被强杀或链路断开 → A 上的进程组被 SIGKILL；命令**正常结束**时不杀进程组，所以 `bash -c 'npm start &'` 起的后台服务会留在沙箱里（会话结束时随沙箱一起销毁）。主进程退出后最多再转发 200ms 输出，之后后台进程的输出被丢弃而不是让它收到 EPIPE。
- **受保护路径**：存在的 `.git/hooks`、`.git/config`、`.git/info`、`.envrc`、`.vscode/` 等以只读挂载；`.git` 自身额外绑定到原处，使它无法被改名替换。挂载需要挂载点，**不存在的路径无法预先设为只读**，因此会话前后对它们（以及 `.git/commondir`、`.git/config.worktree`）做快照对比，有变化就在退出时警告并写审计日志。
- **环境变量是白名单**：沙箱与 Agent 只拿到 `TERM`、`LANG`、`LC_*`、`TZ`、`USER`、`SHELL`、工具链相关变量等；其余（包括宿主机上的各种密钥）不会进入沙箱。需要的变量用 `[sandbox] pass_env` 追加。代理变量只注入到 A 上执行的命令，不进入 B 上的 Agent。
- **出网代理**额外拒绝解析到回环/内网/链路本地/CGNAT 地址的目标（防 DNS rebinding 绕过白名单访问宿主机），可用 `net.allow_private = true` 关闭。
- **项目目录**取 git 顶层目录，没有 git 时取当前目录；拒绝把 `/` 或整个 `$HOME` 当项目（`WALDO_ALLOW_ANY_DIR=1` 可强制）。
- **两端 UID/GID 不同时**：B 的 sshfs 使用会话内的显式 ID 映射，把 A 当前用户和主组映射到 B 当前用户和主组，并对 chown 请求做反向转换；其他属主保持原编号。映射只影响投射视图，不修改 A 文件的真实所有权，避免 B 胶囊把 A 的属主显示成 `65534`，导致 Claude 拒绝使用临时目录。
- **Claude Code 实测**（2.1.322）：shell 快照确实写在 `~/.claude/shell-snapshots`，cwd 记录文件在 `/tmp`，profile 的 `shared` 列表无需调整；`USE_BUILTIN_RIPGREP=0` 后它调用 PATH 上的 `rg`（被转发到 A）。
- **默认复用 B 的 Agent 状态**：B 上已有的状态**目录**（如 `~/.claude`，OAuth 凭证在其中的 `.credentials.json`）直接绑定进胶囊，B 上原生 claude 与 waldo 共用同一份凭证；状态**文件**（如 `~/.claude.json`，不含令牌）首次复制一份到胶囊 HOME，已有非空副本不覆盖。原因是实测：对单文件绑定挂载做"写临时文件再 rename"会返回 EBUSY（DESIGN §5.2 预见的问题），Claude Code 正是这样改写 `~/.claude.json` 的。两边的 `~/.claude.json`（项目信任、界面偏好等）此后各自独立。`setup --adopt-login=false` 只停止目录共享，保留原文件、已有副本和独立状态。
- **来自 B 的所有终端输出都过滤**：不仅是 Agent 的 io 流，ssh 转发的 B 端 stderr、握手错误信息、doctor 报告字段也都经过 `termfilter`（有回归测试：B 端二进制被换成往 stderr 写 OSC 52 的脚本）。
- 未实现（属于 M5 或之后）：`--isolated`（worktree）、macOS 作为 A、断线重连、自研 FUSE 客户端与缓存。
