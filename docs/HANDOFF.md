# waldo 交接文档

> 状态截至 2026-11-10。设计依据是仓库根目录的 [DESIGN.md](../DESIGN.md)，用法见 [README.md](../README.md)，安全模型见 [SECURITY.md](SECURITY.md)，profile 格式见 [profiles.md](profiles.md)。本文只讲接手需要知道的事：现在到哪了、怎么跑、代码怎么读、哪里有坑、下一步做什么。

## 1. 一句话现状

DESIGN.md 的 **M0–M4 已实现**，单机回环（A 和 B 是同一台机器）下全部测试通过，真实 Claude Code 2.1.322 端到端可用。**M5 未开始**。从未在两台真实的不同机器之间运行过。

## 2. 快速上手

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
| `TestSetupAdoptAndPorts` | `setup` 写入 host、`--adopt-login` 的目录绑定/文件复制语义、会话中 `waldo ports` |
| 单元测试 | 终端过滤、域名匹配与内网地址、SFTP 嗅探、SSH 加固参数、拒绝 B 的非法流 |

### 只手工验证过（未自动化）

- 真实 Claude Code 2.1.322：`waldo claude --version`、两次 `waldo claude -p`（Bash 工具在 A 沙箱执行，Write 工具经 FUSE 写到 A，shell 快照和 `/tmp` cwd 文件机制成立），交互式 TUI 渲染到工作区信任对话框并能用方向键操作。用的是本机已有的 Claude 登录（`--adopt-login`）。
- `waldo doctor`、`waldo link`、`waldo log`、`waldo allow` 对运行中会话立即生效。

### 从未验证

- **两台真实的不同机器**（不同主机名、不同用户/uid、真实网络延迟、跳板机）。这是接手后最该先做的事，见第 7 节。
- codex（本机未安装）；任何 `#!` 脚本形式安装的 Agent（如 npm 全局安装的 node 脚本）。
- 真实 Claude 的完整交互对话（含权限确认界面）。
- B 架构与 A 不同（`make release` 的跨架构安装路径）。
- 关闭缓存的 FUSE 在真实网络延迟下的体验。
- SSH 禁转发只检查了参数，没用恶意 sshd 实测。
- M0 验收里的 `npm install && npm start`：本机没有 node，用 `python3 -m http.server` 代替。

## 5. 实现中发现的坑（改代码前先读）

1. **单文件绑定挂载上 rename 会 EBUSY。** 写临时文件再 rename 覆盖一个被单独 bind 进来的文件会失败；Claude Code 正是这样写 `~/.claude.json` 的，第一次实测时它的写入悄悄失败了。因此 `--adopt-login` 只绑定目录，文件复制一份（`host/capsule.go` 的 `adoptMain` 和 `capsuleArgs`）。今后任何"把 B 上某个文件给胶囊"的需求都要绑目录，或者复制。
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
| FUSE 无缓存 | 每次 stat 一个往返，真实网络下未测性能 | `host.go` 的 `mount` 选项 |
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

1. **两台真机验证**（最高优先级）：一台 A、一台 B，用户名或 uid 不同、家目录路径不同。跑 `waldo setup`、`waldo doctor`、`waldo claude` 做一次真实任务（读代码、改文件、跑测试、起服务并在 A 的浏览器里打开）。重点看：uid 不同时 FUSE 上的文件属主和权限、延迟下 TUI 和文件操作的手感、`doctor` 的时钟偏差。
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
