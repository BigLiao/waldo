# waldo 交接文档

> 状态截至 2026-11-11。设计依据是仓库根目录的 [DESIGN.md](../DESIGN.md)，用法见 [README.md](../README.md)，构建、测试与实现细节见 [DEVELOPMENT.md](DEVELOPMENT.md)，安全模型见 [SECURITY.md](SECURITY.md)，profile 格式见 [profiles.md](profiles.md)。本文只讲接手需要知道的事：现在到哪了、怎么跑、代码怎么读、哪里有坑、下一步做什么。

## 1. 一句话现状

DESIGN.md 的 **M0–M4 已实现**，单机回环（A 和 B 是同一台机器）下全部测试通过，真实 Claude Code 2.1.322 端到端可用。**M5 未开始**。两台真实 Linux 机器间已完成 setup/doctor、不同 UID/GID 的文件投射探针、Claude 版本查询与首次启动界面验证；完整开发任务尚未验证。

新增的会话说明功能：A 在启动时生成只读 `/run/waldo/context/waldo.md`，额外投射到 B；`internal/projection` 为项目根目录指令文件提供带引用的只读启动快照，普通 SFTP 操作仍由 `pkg/sftp.Server` 处理。配置见 [configuration.md](configuration.md) 的 `[context]`。该功能有协议层单元测试，尚未重新做真实 Agent 或两机端到端验证；目录排除策略尚未实现。

## 2. 快速上手

`waldo setup <host>` 现在默认通过 `internal/bootstrap/install.sh` 自动补齐 A/B 缺失的系统依赖（脚本内嵌进二进制），支持 apt-get、dnf/yum、pacman、apk。交互式 SSH 初始化单独申请 TTY 供 sudo 输入密码，协议连接仍禁用 TTY；非交互运行使用 `sudo -n`，失败即停止并提示手工命令。`--skip-deps` 仅跳过依赖安装，仍安装 B 端 waldo、复用状态并运行 doctor。安装器、setup 顺序与失败处理、SSH TTY 和输出过滤均有不调用真实包管理器的测试；已实际验证 TencentOS 上自动安装 bubblewrap 和 Ubuntu 端依赖齐全时跳过安装。

`setup` 同时默认复用 B 上各 profile 的现有配置与登录：`state` 目录直接绑定，单文件首次复制且保留已有非空副本。显式 `--adopt-login`、`--adopt-login=true` 仍兼容；`--adopt-login=false` 清除目录绑定名单，停止后续会话共享，但不删除原文件、已有副本和胶囊独立状态。重新开启也不覆盖已有非空单文件副本。

```sh
make dev          # 编译 ./waldo（版本号 = 二进制哈希，重编后会自动重装到 B）
make test         # vet + 单元 + 攻击 + e2e，约 2 分钟
make test-e2e     # 只跑全链路
WALDO_KEEP_TEST=1 go test -p 1 -run TestProbe -v ./test/e2e/   # 保留测试目录便于排查
```

测试依赖：`bwrap`、`sshfs`/`fusermount3`、`/usr/sbin/sshd`、`ssh-keygen`、`git`、`curl`、`python3`。攻击测试里有一个"白名单域名可访问"的正向对照需要能连 github.com，离线时设 `WALDO_TEST_OFFLINE=1`。

本机内存只有约 1GB：测试必须 `-p 1` 串行（Makefile 已经这样写），不要并行跑多个包。

**测试环境如何搭的**（`test/harness/harness.go`）：每个测试新建 `~/.cache/waldo-test/run-*`，里面放编译好的二进制、A 的数据/配置目录、B 的数据目录，以及一个只认临时密钥的用户态 sshd（随机端口，`UsePAM no`）。通过 `WALDO_DATA_DIR`、`WALDO_CONFIG_DIR`、config 里的 `ssh_args = ["-F", ...]` 和 `remote_dir` 把一切指向这个目录，结束后删除，不碰 `~/.ssh`、`~/.local/share/waldo`。

**回环下如何判断"在哪执行"**：A 和 B 主机名、路径都相同，所以断言一律用网络命名空间——A 沙箱有自己的 netns，B 胶囊共用宿主机的 netns。新写测试时沿用这个判别方法，不要用 `uname -n`。

## 3. 代码地图

一个二进制四个角色（`cmd/waldo/main.go`）：`argv[0]` 不以 `waldo` 开头 → stub；`waldo host` → B 端；`waldo sandbox-serve` → A 沙箱内；其余 → A 端 CLI。

| 想看什么 | 去哪 |
|---|---|
| A 端一次会话的完整流程 | `internal/session/session.go` 的 `Run` → `runRemote` → `relayAgentIO` |
| B 发来的流怎么被放行/拒绝 | `session.go` 的 `acceptFromB`（只收 `fs`、`exec`） |
| A 沙箱怎么启动、拿到哪些环境变量 | `internal/session/sandbox.go`（`StartSandbox`、`BaseEnv`、`ProxyEnv`） |
| bwrap 参数、受保护路径、会话后检查 | `internal/sandbox/sandbox.go`、`integrity.go` |
| 沙箱内执行命令（pty、进程组、后台进程） | `internal/serve/exec.go` 的 `runExec` |
| 沙箱内出网代理入口、端口探测 | `internal/serve/net.go` |
| 出网白名单、宿主机端口、端口转发（可信侧） | `internal/netpolicy/` |
| B 端：挂载、stub 目录、胶囊参数、Agent 进程 | `internal/host/host.go`（`prepare`、`mount`、`makeStubs`）、`capsule.go`（`resolveAgent`、`capsuleArgs`、`runAgent`、`adoptMain`） |
| stub 如何确定自己代表哪个程序 | `internal/stub/stub.go` 的 `identity`（`AT_EXECFN`） |
| SSH 参数、远端自动安装 | `internal/transport/ssh.go` |
| 线上协议（流头、帧、握手） | `internal/proto/proto.go` |
| 终端过滤 | `internal/termfilter/`（`Filter` 用于字节流，`Sanitize` 用于来自 B 的短字符串） |

### 线上协议速记

- A↔B：`ssh -T` 的 stdin/stdout 上跑 yamux。B 先输出一行 `WALDO-HOST <版本>` 再开始 yamux；版本不符或没装时远端脚本输出 `WALDO-NEED-INSTALL <uname -s> <uname -m>`，A 安装后重试。
- 每条 yamux 流先发一个"4 字节长度 + JSON"的 `Header` 说明用途（`ctl`/`io`/`fs`/`exec`/`sandbox`/`connect`/`hostport`/`dial`）。
- `io`、`exec` 流之后是帧：1 字节类型 + 4 字节长度 + 负载（stdin、stdout、stderr、信号、窗口大小、退出码、错误信息）。
- `fs` 流之后是裸 SFTP（B 端 `sshfs -o passive` ↔ A 沙箱内 `pkg/sftp`）。
- A 客户端 ↔ A 沙箱：沙箱进程的 stdin/stdout 上是第二个 yamux 会话。可信客户端在两个会话之间搬字节，只解析流头，并旁路嗅探 SFTP 写操作写审计日志。

### 版本与安装

`proto.FullVersion()` = `0.1.0+<Build>`。`make dev` 不设 `Build`，用可执行文件的 sha256 前 12 位，因此每次重编都会触发重装到 B。`make build`/`make release` 用 git 提交号，多个架构的二进制共享同一个 id——B 架构和 A 不同时，A 会在自己旁边找 `waldo-linux-<arch>`。

## 4. 验证情况

### 已自动化（`make test`）

| 测试 | 覆盖 |
|---|---|
| `test/attack` `TestAttacks` | 19 种攻击全部失败 + 5 个正向对照（见 SECURITY.md） |
| `TestPortForwardPolicy` | 普通开发端口转发到宿主机，5432 不转发 |
| `TestProtectedPathWarning` | 会话中新建 `.envrc` 会在退出时警告 |
| `TestSandboxSeesOnlyLoopback` | 沙箱只有 `lo` |
| `test/e2e` `TestProbe` | 假 Agent 全链路：stub、写死的 `/bin/sh`、`#!` 脚本、进程内文件读写/rename/mkdir 落到 A、`/tmp` 投射、环境变量过滤、信号转发、stub 被杀后进程组清理、后台服务存活并转发到宿主机、审计日志 |
| `TestDesignM2` | DESIGN M2 验收命令原样 |
| `TestTTY`、`TestInteractiveShell` | pty、按键、窗口大小同步、被转发命令也拿到匹配尺寸的 pty |
| `TestPipesAndExitCode` | 非交互管道、stderr、退出码 |
| `TestCleanupAfterBrokenLink` | 强杀 ssh 后 B 端挂载和运行目录被清理 |
| `TestHostStderrFiltered` | B 端二进制被换成往 stderr 写 OSC 52 的脚本，终端里看不到转义序列 |
| `TestDifferentHome` | A、B 的 HOME 路径不同时映射正确 |
| `TestSetupAdoptAndPorts` | `setup` 默认复用状态、目录绑定/单文件复制、显式关闭和重新开启时保留已有状态、会话中 `waldo ports` |
| 单元测试 | 终端过滤、域名匹配与内网地址、SFTP 嗅探、SSH 加固参数、拒绝 B 的非法流 |

### 只手工验证过（未自动化）

- 真实 Claude Code 2.1.322：`waldo claude --version`、两次 `waldo claude -p`（Bash 工具在 A 沙箱执行，Write 工具经 FUSE 写到 A，shell 快照和 `/tmp` cwd 文件机制成立），交互式 TUI 渲染到工作区信任对话框并能用方向键操作。用的是本机已有的 Claude 登录（当时显式传入 `--adopt-login`，当前已默认开启）。
- `waldo doctor`、`waldo link`、`waldo log`、`waldo allow` 对运行中会话立即生效。
- 2026-11-11：真实 A（TencentOS，UID/GID `51368:100`）→ B（Ubuntu，`1000:1000`）。原 `idmap=none` 导致 Claude 临时目录显示属主 `65534` 而拒绝启动，已改为每会话 `idmap=file,nomap=ignore` 精确双向映射。真实 `fakeagent ownership` 的项目与 `/tmp` 目录/文件属主、显式 chmod、写入/rename/读取全部通过，探针文件已清理；Claude `--version` 返回 2.1.322，交互启动到主题选择界面，未发送模型请求。
- 项目元数据缓存：`[fs] metadata_cache_ttl` 当前默认 `10s`，支持项目覆盖及 `0s` 关闭，范围为 `0s` 到 `1m` 的整数秒，新会话生效；仅启用内核 attr/entry TTL，SSHFS 用户态目录缓存和 negative 缓存保持关闭，运行路径仍零缓存，写入仍同步。使用约定是人工避免 A/B 同时修改项目；A 上编辑或执行命令（包括 Agent 转发的命令）后，B 仍可能在 TTL 内看到旧元数据。
- **当时1s实测**：真实两机的隔离临时项目上，同一文件预热后 20 次 stat 从关闭缓存的 7769.765ms 降到 1s 缓存下的 0.088ms；20 次重新打开读取从 23664.955ms 到 15238.706ms。两种模式均通过 A 修改、新建、rename、删除后 B 收敛可见的探针。上述数字来自当时的 `1s` 配置，当前默认 `10s` 尚未做两机基准测试，不代表真实 Agent 整体提速相同比例。

### 从未验证

- **两台真实机器的完整开发任务**和跳板机：目前只验证到上述双机初始化、文件投射探针与 Claude 启动，见第 7 节。
- codex（本机未安装）；任何 `#!` 脚本形式安装的 Agent（如 npm 全局安装的 node 脚本）。
- 真实 Claude 的完整交互对话（含权限确认界面）。
- B 架构与 A 不同（`make release` 的跨架构安装路径）。
- 多项目、大目录及长时间运行时的缓存失效与完整任务体验；目前的两机缓存性能数据和文件变更探针结果来自当时 `1s` 配置。
- SSH 禁转发只检查了参数，没用恶意 sshd 实测。
- M0 验收里的 `npm install && npm start`：本机没有 node，用 `python3 -m http.server` 代替。

## 5. 实现中发现的坑（改代码前先读）

1. **单文件绑定挂载上 rename 会 EBUSY。** 写临时文件再 rename 覆盖一个被单独 bind 进来的文件会失败；Claude Code 正是这样写 `~/.claude.json` 的，第一次实测时它的写入悄悄失败了。因此默认状态复用只绑定目录，文件首次复制且已有非空副本不覆盖（`host/capsule.go` 的 `adoptMain` 和 `capsuleArgs`）；`--adopt-login=false` 只停止目录共享，不清理任何原状态或副本。今后任何"把 B 上某个文件给胶囊"的需求都要绑目录，或者复制。
2. **胶囊里几乎所有命令都是 stub。** 包括 `touch`、`cat`、`mkdir`——它们在 A 上执行。只有 Agent 进程内的文件操作和 shell 内建命令发生在 B。写测试时很容易犯错（`TestDifferentHome` 就因此写错过一次，改成了 `: > file`）。
3. **stub 身份靠 `AT_EXECFN`。** `argv[0]` 可以被调用方随意设置（`exec -a`、登录 shell 的 `-bash`），不可靠；`AT_EXECFN` 是内核实际执行的路径。`#!` 脚本被直接执行时 `AT_EXECFN` 是脚本本身，这时回退到 `argv[0]`（内核放进去的解释器路径）。
4. **进程组语义有意不对称。** 链路断或 stub 被杀 → A 上整个进程组 SIGKILL；命令正常结束 → 不杀，`bash -c 'server &'` 留下的后台服务继续活到会话结束。主进程退出后最多再转发 200ms 输出，之后把输出丢弃而不是关管道，否则后台进程写日志会 EPIPE 崩掉。
5. **stub 自己中继 stdio，不用 SCM_RIGHTS。** 把 Agent 的 tty fd 交给 host 后，无法安全中断阻塞读，会吞掉用户的下一次按键。stub 是 tty 时把本地 tty 置 raw，行规程交给 A 上的 pty，和 ssh 客户端一样。
6. **`waldo host` 必须处理 SIGPIPE/SIGHUP。** ssh 断开后 Go 程序写 stdout 默认会被 SIGPIPE 直接杀死，来不及卸载挂载和删运行目录。现在用 `signal.Notify` 接住（`host.go` 的 `Main`）。上次运行崩溃留下的运行目录会在下次启动时由 `cleanStale` 清理。
7. **环境变量是白名单。** 宿主机环境（可能有各种云凭证）不进沙箱；用户设的变量要进 Agent 必须配 `[sandbox] pass_env`。代理变量只注入 A 上执行的命令，绝不能进 B 上的 Agent，否则 Agent 访问模型 API 会走到 B 自己的 localhost。
8. **`.git` 绑定到自身。** 让 `.git` 成为挂载点，`mv .git .git-old` 会 EBUSY，防止换一个带新 hooks 的 `.git`。
9. **Ubuntu 的 `/etc/resolv.conf` 是符号链接。** 胶囊里绑定的是它的真实目标（`capsuleArgs` 里对 `/etc` 文件统一 `EvalSymlinks`）。
10. **go 模块代理。** 本机默认 `GOPROXY` 是腾讯镜像，部分模块拉取会失败；用 `GOPROXY=https://proxy.golang.org,direct`。

## 6. 已知限制与技术债

按影响大致排序：

| 项 | 说明 | 位置 |
|---|---|---|
| 新建的受保护路径只告警不阻止 | 会话开始时不存在的 `.envrc`、`.git/commondir`、非 git 目录里新 `git init` 的 `.git` 无法预先只读。现在只在退出时对比快照并警告 | `sandbox/integrity.go` |
| 时钟偏差只告警 | 未实现 DESIGN §11 说的 mtime 校正 | `session.go` 握手处 |
| 断线即会话结束 | 无脱离/重连（M5） | — |
| FUSE 缓存尚无主动失效 | 项目默认 10s 内核属性/路径缓存；A 修改（含 Agent 转发执行的命令）需等待 TTL 加在途请求，目录列表与运行路径零缓存，冷查询和文件读取仍有跨机开销 | `host/cache.go`、`[fs] metadata_cache_ttl` |
| 只有遵守 `HTTP(S)_PROXY` 的程序能出网 | 已额外设 `NODE_USE_ENV_PROXY=1`；SSH 方式的 git remote 不可用 | `session/sandbox.go` `ProxyEnv` |
| stdin 是终端而 stdout 被重定向时 | Agent 拿到的是立即 EOF 的 stdin（不是 tty） | `relayAgentIO` |
| 只有 stdout 是 tty 的转发命令 | 走管道模式，`ls --color=auto` 之类不会着色 | `stub.Relay` 只在 0 和 1 都是 tty 时要 pty |
| `.git/info` 只读 | 可能妨碍 `git sparse-checkout` 等写 `info/` 的操作 | `sandbox.ProtectedNames` |
| 审计日志不轮转 | `audit.log` 无限增长 | `internal/audit` |
| `doctor` 的"沙箱可见"判断是启发式 | 根据路径前缀推断，不是真的起沙箱查 | `cli.go` `doctor` |
| `waldo link` 需要用户手动把目录放到 PATH 最前 | 只打印提示 | `cli.go` `link` |
| Agent 包内自带的原生二进制在 B 上执行 | 不在 PATH 目录里的程序不会被 stub 替换（DESIGN §11 已列为限制） | — |
| `--isolated`、macOS 作为 A、自研 FUSE 客户端 | 未做（M5） | — |

## 7. 建议的下一步

1. **补全两台真机的真实任务验证**（最高优先级）：初始化、不同 UID/GID 的文件投射与 Claude 启动已通过，接下来完成一次读代码、改文件、跑测试、起服务并在 A 的浏览器里打开的任务，重点看真实网络延迟下的 TUI 和文件操作体验。
2. **codex 实测**：确认 `--sandbox danger-full-access` 是否需要、首次登录流程能否在无浏览器的 B 上完成（DESIGN §11 最后一项）。
3. **npm/node 项目实测** M0 验收原文：`npm install && npm start`，宿主机浏览器访问。
4. 把真实 Claude 的冒烟测试做成可选的自动化测试（需要登录态，默认跳过）。
5. 然后按 DESIGN M5：脱离/重连 → 带失效推送的 FUSE 缓存 → `--isolated` → macOS。

## 8. 排查手册

| 现象 | 先看 |
|---|---|
| 会话起不来 | A：`~/.local/share/waldo/log/sandbox.log`；B：`~/.local/share/waldo/log/host.log` |
| 想知道 Agent 执行了什么、写了什么 | `waldo log -n 200`，或 `waldo log -f -s <会话id>` |
| 怀疑是沙箱策略问题 | `waldo dev-sandbox -- bash`：不连 B，直接进同样策略的沙箱 |
| 端口没转发 | `WALDO_DEBUG_PORTS=1 waldo dev-sandbox -- ...`；`waldo ports`；审计日志里的 `port-busy` |
| 出网被拒 | 403 响应正文里有 `waldo allow <域名>` 提示；审计日志 `net-deny` |
| B 上残留挂载 | `grep ' waldo:' /proc/self/mountinfo`，`fusermount3 -u -z <挂载点>`；运行目录在 `$XDG_RUNTIME_DIR/waldo/` 或 `/tmp/waldo-<uid>/` |
| B 上版本不对 | 删 `~/.local/share/waldo/bin/waldo`，下次运行会自动重装 |

## 9. 本机环境备注

开发机：Ubuntu 26.04、内核 7.0、Go 1.26.8、bubblewrap 0.11.1、sshfs 3.7.3 / fuse 3.18.2，`kernel.apparmor_restrict_unprivileged_userns=1` 但 bwrap 有 AppArmor profile，可以正常用。开发期间创建的临时 sshd、测试目录都已清理；本机真实的 `~/.local/share/waldo`、`~/.config/waldo` 从未创建过。
