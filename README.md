# waldo

Agent（Claude Code / Codex / …）的进程和登录态放在你的服务器 B 上，它的"手"——文件读写、命令执行、端口——落在你当前这台机器 A 的沙箱里。设计见 [DESIGN.md](DESIGN.md)，接手开发请先读 [docs/HANDOFF.md](docs/HANDOFF.md)。

```sh
waldo setup me@my-server        # 一次性：写配置、把同版本二进制装到 B、检查两端依赖
cd ~/work/shop
waldo claude                    # 交互式，和原生 claude 一样
waldo claude -p "跑一下测试" | tee out.txt
waldo run -- aider              # 任意 CLI agent（空 profile）
```

| 命令 | 作用 |
|---|---|
| `waldo setup <host> [--adopt-login]` | 写入 `host`，安装/升级 B 端二进制，运行 doctor；`--adopt-login` 复用 B 上已有的 Agent 登录：状态**目录**直接绑定进胶囊（凭证只有一份），状态**文件**复制一份 |
| `waldo doctor` | 两端依赖、bwrap 能否建沙箱、Agent 是否安装/已有登录态、时钟偏差 |
| `waldo allow [--global] <域名>` | 给当前项目（或所有项目）放行出网域名，运行中的会话立即生效 |
| `waldo ports` | 当前会话转发到 `127.0.0.1` 的端口 |
| `waldo log [-n N] [-f] [-s 会话]` | 审计日志：执行过的命令、写过的文件、出网、端口 |
| `waldo link [--remove] <agent>` | 生成 `<agent>` → `waldo <agent>` 的包装脚本 |
| `waldo dev-sandbox -- <命令>` | 调试用：只起 A 端沙箱（同样的策略），在里面跑一条命令 |

## 构建

需要 Go 1.22+。产物是单个静态二进制（`CGO_ENABLED=0`）。

```sh
make dev       # ./waldo，版本号带二进制哈希：每次重编译后会自动重装到 B
make build     # ./waldo，版本号用 git 提交号
make release   # dist/waldo-linux-{amd64,arm64}；B 的架构与 A 不同时，A 会找同目录下的这些文件
```

依赖：A 需要 `ssh`、`bubblewrap`（claude profile 还需要 `rg`、`git`、`bash`）；B 需要 `sshd`、`bubblewrap`、`sshfs`（fuse3）和已安装的 Agent。

## 配置

全部在 A 的 `~/.config/waldo/config.toml`，B 上没有配置文件：

```toml
host = "me@my-server"
# ssh_args   = ["-F", "/path/to/ssh_config"]   # 追加给 ssh 的参数（也可用环境变量 WALDO_SSH_ARGS）
# remote_dir = "~/.local/share/waldo"           # B 上的数据目录

[sandbox]
expose_ro = ["~/.config/nvim"]     # 自动探测的工具链之外再只读挂载
pass_env  = ["ANTHROPIC_MODEL", "MY_APP_*"]   # 额外传入沙箱和 Agent 的环境变量（glob）

[net]
allow = ["api.staging.example.com"]   # 在内置白名单（常见包仓库、GitHub）之上追加
ports = "auto"                        # "auto" | [3000, 5173] | "off"
host_ports = [5432]                   # 沙箱可访问的宿主机服务

[project."/home/me/work/shop"]
net.allow = ["api.example.com"]
net.host_ports = [5432, 6379]

[agent.claude]                        # 覆盖/新增 Agent profile，见 docs/profiles.md
env = { CLAUDE_CODE_MAX_OUTPUT_TOKENS = "16000" }
```

`waldo allow` 写的是 `~/.config/waldo/allowed.json`，与 `config.toml` 合并。

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
  session/            A 端编排：沙箱 + SSH + 各流接线（可信进程）
  transport/          SSH 传输、加固参数、远端自动安装
  proto/              流头、帧、握手消息、版本
  host/               B 端：胶囊（bwrap）、sshfs 挂载、stub 汇聚、pty、doctor
  stub/               B 胶囊内：把一次进程启动转发到 A
  serve/              A 沙箱内：SFTP 服务、exec 服务、出网代理入口、端口探测
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
- **Claude Code 实测**（2.1.322）：shell 快照确实写在 `~/.claude/shell-snapshots`，cwd 记录文件在 `/tmp`，profile 的 `shared` 列表无需调整；`USE_BUILTIN_RIPGREP=0` 后它调用 PATH 上的 `rg`（被转发到 A）。
- **`--adopt-login`**：B 上已有的状态**目录**（如 `~/.claude`，OAuth 凭证在其中的 `.credentials.json`）直接绑定进胶囊，B 上原生 claude 与 waldo 共用同一份凭证；状态**文件**（如 `~/.claude.json`，不含令牌）复制一份到胶囊 HOME。原因是实测：对单文件绑定挂载做"写临时文件再 rename"会返回 EBUSY（DESIGN §5.2 预见的问题），Claude Code 正是这样改写 `~/.claude.json` 的。代价是两边的 `~/.claude.json`（项目信任、界面偏好等）此后各自独立。
- **来自 B 的所有终端输出都过滤**：不仅是 Agent 的 io 流，ssh 转发的 B 端 stderr、握手错误信息、doctor 报告字段也都经过 `termfilter`（有回归测试：B 端二进制被换成往 stderr 写 OSC 52 的脚本）。
- 未实现（属于 M5 或之后）：`--isolated`（worktree）、macOS 作为 A、断线重连、自研 FUSE 客户端与缓存。
