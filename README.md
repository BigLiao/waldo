# waldo

> **Claude 跑在远端服务器，本机用起来毫无感知——跟直接装在本地一样。**

waldo 把 Claude Code（以及 Codex 等编程 Agent）留在你自己的 Linux 服务器上，而你在手边的电脑里照常进入项目目录、照常敲 `claude`：读代码、搜仓库、改文件、跑构建和测试、开本地开发服务器与调试器，全部直接在本机完成。无需复制或同步代码，也无需操心文件在哪台机器上，从命令行到 [Paseo](https://paseo.sh) 聊天界面，用起来都像本地原生运行一样顺手。

- **本机无感体验，跟本地跑一样**：项目读写、搜索、依赖安装和 `npm start` 直接跑在本机，参数、交互界面、管道与退出码完全照常；执行一次 `waldo link claude` 后，连命令都还是直接敲 `claude`。
- **一处登录，多端共用**：Agent 的账号、登录态、配置与会话历史都留在服务器，你在 Linux、macOS 或 Windows（WSL2）电脑上装好 waldo 就能无缝续用。
- **快如本地，减少跨机等待**：文件读写、代码搜索与命令默认直接在本机完成，免去大量逐文件的跨机往返，日常操作响应跟本地一致。
- **按需开启本机沙箱**：默认以当前用户权限直接执行，开箱即用；需要更强隔离时，随时可为单次会话或特定项目开启本机沙箱，限制文件范围与出网白名单。

---

## 快速上手

只要三步，就能把远端服务器上的 Agent 接到本机项目里。

### 开始之前

请先确认具备以下条件：

- **远端服务器**：一台能通过 SSH 直接登录的 Linux 服务器（`ssh me@my-server` 可以连通），上面已经装好你要用的 Agent（例如 Claude Code 或 Codex）。
- **本机电脑**：Linux、macOS，或安装了 Ubuntu 等 WSL2 发行版的 Windows（在 WSL2 终端内运行）。

其余基础依赖（如服务器的 `bwrap`、`sshfs` 和本机的 `rg`、`git` 等）会由 `waldo setup` 自动检查并补齐。

### 第 1 步：在本机安装 waldo

运行一键安装脚本（仅需 `curl` 与 `sha256sum` 或 Mac 自带的 `shasum`，不需要 Go 或 `sudo`）：

```sh
curl -fsSL https://raw.githubusercontent.com/BigLiao/waldo/master/install.sh | sh
```

脚本会自动识别本机系统与架构（Linux / macOS 的 `amd64` / `arm64`），下载最新稳定版及配套的 Linux 服务端程序，校验 SHA256 后原子安装到 `~/.local/bin`（不会改动已有配置或 shell 启动文件）。

> **提示**：如果 `~/.local/bin` 还不在 `PATH` 中，按脚本输出把它加进 `~/.bashrc` 或 `~/.zshrc`：
>
> ```sh
> export PATH="$HOME/.local/bin:$PATH"
> ```
>
> - **macOS 用户**：需要 v0.4.1 及以上的 Release，详见 [macOS 使用说明](docs/macos.md)。
> - **Windows 用户**：请在 WSL2 发行版的终端里执行安装和后续命令，详见 [Windows 与 WSL2 使用说明](docs/configuration.md#windows-与-wsl2)。

<details>
<summary>其他安装方式：指定版本/目录、手动下载 Release、从源码构建</summary>

#### 指定版本或安装目录

```sh
curl -fsSL https://raw.githubusercontent.com/BigLiao/waldo/master/install.sh | sh -s -- --version v0.4.1 --bin-dir "$HOME/.local/bin"
```

#### 从 GitHub Releases 手动安装（Linux）

从 [GitHub Releases](https://github.com/BigLiao/waldo/releases/latest) 下载对应架构的 `waldo-linux-amd64`（或 `waldo-linux-arm64`）与 `SHA256SUMS` 放在同一目录后执行：

```sh
sha256sum --ignore-missing --check SHA256SUMS
mkdir -p ~/.local/bin
install -m 755 waldo-linux-amd64 ~/.local/bin/waldo   # ARM64 改用 waldo-linux-arm64
waldo version
```

如果服务器与本机架构不同，将同一 Release 中服务器架构的二进制按原文件名一并放入 `~/.local/bin/`，供 `waldo setup` 自动部署（一键安装脚本已自动包含此步骤；macOS 推荐直接使用一键安装）。

#### 从源码构建（需要 Go 1.26.8+）

适合体验主分支尚未发布的新功能：

```sh
git clone https://github.com/BigLiao/waldo.git
cd waldo
make build
install -m 755 waldo ~/.local/bin/waldo
```

macOS（或与服务器不同架构的本机）从源码构建时，使用 `make build-pair` 同时生成匹配版本的本机与双架构 Linux 程序，并放到同一目录：

```sh
make build-pair BUILD="dev-$(date +%s)"
install -m 755 waldo waldo-linux-amd64 waldo-linux-arm64 ~/.local/bin/
```

</details>

### 第 2 步：初始化服务器连接（只需一次）

```sh
waldo setup me@my-server
```

这一步会自动完成四件事（若需安装系统依赖，可能会提示输入 `sudo` 密码）：

1. 检查并补齐本机和服务器缺少的系统依赖；
2. 将匹配版本的 waldo 自动部署到服务器上；
3. 默认复用服务器上已有的 Agent 配置与登录态；
4. 运行一次全面体检（`waldo doctor`）并展示结果。

如果服务器上还没登录过 Agent，直接继续下一步，首次启动 Agent 时按提示登录即可；之后换其他电脑连接同一台服务器也不必重新登录。

### 第 3 步：像本地一样直接使用

切换到本机的任意项目目录，直接启动 Agent：

```sh
cd ~/work/my-app
waldo claude                                 # 交互式使用，读写、测试、起服务全在本机完成
waldo claude -p "跑一下测试" | tee out.txt    # 非交互模式，参数、管道与退出码跟本地一致
waldo codex                                  # 同样支持 Codex
waldo run -- aider                           # 运行其他任意命令行 Agent
```

**推荐顺手做一次无感接管**，连 `waldo` 前缀都省掉：

```sh
waldo link claude
```

按输出提示将 `export PATH=...` 加到 `~/.bashrc` 或 `~/.zshrc` 中。之后在任何项目目录里直接敲 `claude`，背后的远端连接、项目文件与本机命令全部自动衔接，日常使用跟在本机直接装了 Claude 完全一样。随时可以运行 `waldo link --remove claude` 还原。

---

## 扩展与可选功能

默认配置就能覆盖大多数日常开发。下面这些功能按需开启，互不干扰。

### 在 Paseo 聊天界面中使用

如果你习惯图形化的多 Agent 聊天和编排界面，可以把 waldo 接入 [Paseo](https://paseo.sh)。

1. 完成 `waldo setup` 后，在**代码所在的本机**确保 Paseo daemon 已运行（未运行时执行 `paseo daemon start`）。
2. 在 Paseo 主机设置中开启 **Enable Paseo tools**。
3. 将以下 provider 合入 `~/.paseo/config.json`（设置了 `PASEO_HOME` 时为 `$PASEO_HOME/config.json`），把 waldo 路径替换为 `command -v waldo` 查到的绝对路径：

```json
{
  "agents": {
    "providers": {
      "waldo-claude": {
        "extends": "claude",
        "label": "Claude via Waldo",
        "command": ["/home/me/.local/bin/waldo", "claude", "--paseo"]
      }
    }
  }
}
```

4. 执行 `paseo reload`，在 Paseo 中选择本机项目、新建聊天并选用 **Claude via Waldo** 即可。

`--paseo` 会复用当前 SSH 连接桥接 Paseo MCP，并将本机已安装的 `paseo`、`paseo-*` skills 只读投影给远端 Agent。首轮流式聊天会展示启动状态和初始化耗时，就绪后再发送首条消息。该参数依赖 Paseo 注入的调用者身份与凭证，不能在普通终端里单独开启；详细配置见 [Paseo custom providers](https://paseo.sh/docs/custom-providers)。

### 连接多台服务器、独立管理账号

一台本机可以配置多台远端服务器，各自保留独立的 Claude 账号和会话。编辑本机 `~/.config/waldo/config.toml`（注意 `default_remote` 必须放在文件顶部、其他表段之前）：

```toml
default_remote = "b1"

[remote.b1]
host = "ubuntu@server-1"

[remote.b2]
host = "ubuntu@server-2"           # 也可直接写 ~/.ssh/config 中的别名
ssh_args = ["-i", "/home/me/.ssh/server-2"]
```

分别初始化后即可按名称切换或统一巡检：

```sh
waldo setup --remote b1            # 初始化指定远端，不改动 default_remote
waldo setup --remote b2
waldo claude --remote b2           # 指定远端启动会话
waldo remote ls                    # 查看所有远端配置、默认项与活跃会话数
waldo remote status --all          # 限时只读检查各台机器连接与远端环境
waldo sessions --remote b2         # 查看活跃会话、SSH 状态、PID 和项目
```

- 在 Paseo 中，为每台服务器各自配置一个 provider，并在 `command` 里加上 `"--remote", "b2"` 即可。
- 每个会话启动后固定绑定所选服务器，切换模型或断线重连都不会自动换机。
- 原有单机 `host` 配置继续兼容；若未配置 `host` 且只定义了一个 `[remote.*]`，会自动选中它。更多细节见 [多远端机器](docs/configuration.md#多远端机器)。

### 按需开启本机沙箱

**默认情况下，本机沙箱是关闭的**，本机工具直接使用当前用户的权限、真实 HOME 和本机网络。

如果你希望把本机工具限制在当前项目和白名单网络内，可以随时开启沙箱：

- **单次会话开启**：

  ```sh
  waldo claude --local-sandbox
  waldo run --local-sandbox -- aider
  ```

- **全局或按项目持久开启**（写入 `~/.config/waldo/config.toml`）：

  ```toml
  [sandbox]
  enabled = true

  # 或者只对特定项目开启/关闭
  [project."/home/me/work/shop"]
  sandbox.enabled = true
  ```

优先级为：命令行 `--local-sandbox[=true|false]` > 项目配置 > 全局配置 > 默认关闭。

开启本机沙箱后，常用的配套能力：

- **动态放行出网域名**（对正在运行的会话立即生效）：

  ```sh
  waldo allow api.example.com          # 仅当前项目
  waldo allow --global api.example.com # 所有项目
  ```

- **查看开发端口与访问本机服务**：Linux 沙箱默认自动转发开发端口，可用 `waldo ports` 查看；访问宿主机数据库等服务可配置 `net.host_ports = [5432]`。macOS 需显式配置 `net.ports`（详见 [macOS 使用说明](docs/macos.md)）；Windows WSL2 需将项目放在 WSL Linux 文件系统，并可用 `net.windows_ports` 访问 Windows 本机调试端口（详见 [Windows 与 WSL2](docs/configuration.md#windows-与-wsl2)）。
- **追加环境变量或只读目录**：通过 `[sandbox]` 下的 `pass_env` 与 `expose_ro` 配置，详见 [配置参考](docs/configuration.md#本机沙箱)。

### 切换命令执行模式

平时无需配置，Claude 默认会自动把项目读写、搜索和命令落在本机直接完成，兼顾速度与体验。如果有特定需求，可按需切换为以下可选模式：

- **`host` 模式（更完整还原 Claude 原生环境）**：保留 Claude 全部内置文件工具，通过文件投影访问本机项目，PATH 中的命令转发到本机执行。相比默认方式，它能更完整地还原 Claude 原生工具行为与细节（同时也是 Codex 和 `waldo run` 的默认模式），但在大仓库搜索、高频文件读写或跨机网络延迟较高时，**需要考虑跨机往返带来的速度影响**。
- **`remote` 模式（远端优先执行命令）**：内置 Bash 留在远端服务器执行，仅在需要本机环境时调用注入的 `Bash` 工具，适合依赖主要装在服务器上、偶尔调用本机环境的场景。

按需临时切换或写入 `~/.config/waldo/config.toml`：

```sh
waldo claude --exec-mode host
waldo claude --exec-mode remote
```

```toml
[execution]
mode = "host"   # 全局切换；也可在 [project."..."] 下用 execution.mode 按项目覆盖
```

> **注意**：`remote`（及 Claude 默认模式）目前适用于内置 Claude 及声明了 `mcp_config_flag` 的自定义 profile。若在全局配置中显式设置了 `remote` 等模式，运行 Codex 时需加 `--exec-mode host`。完整选项与路由参数见 [命令执行模式](docs/configuration.md#命令执行模式)。

### 运行日志与耗时观测

waldo 始终在本机记录轻量操作日志（命令执行耗时、退出码、文件 RPC 汇总等），遇到卡顿或想确认 Agent 做了什么，可以在另一个终端实时查看：

```sh
waldo log -f                 # 持续跟踪最新事件
waldo log --remote b2 -n 30  # 按远端筛选最近 30 条，也可加 -s <会话ID>
```

想进一步分析模型响应耗时或 Claude 工具耗时，可以开启轻量观测：

```sh
waldo claude --observe       # 单次开启（Codex 同样支持）；--observe=false 可临时关闭
```

或在 `~/.config/waldo/config.toml` 中默认启用：

```toml
[observe]
enabled = true
```

开启后，本机日志会追加模型请求统计；Claude 还会在远端 `~/.local/share/waldo/log/tool-timing.jsonl` 记录工具 hook 耗时。所有统计均不保存文件内容、提示词或命令输出，口径说明见 [轻量运行日志](docs/configuration.md#轻量运行日志)。

### 接入其他 CLI Agent

除了内置的 `claude`、`codex` 和临时的 `waldo run -- <命令>`，你还可以在 `~/.config/waldo/config.toml` 中用 `[agent.<名称>]` 覆盖内置参数（例如注入额外环境变量）或声明新的 Agent profile，详见 [Agent profile](docs/profiles.md)。

---

## 常用命令速查

| 命令 | 说明 |
|---|---|
| `waldo setup <user@host>` | 一次性初始化：补齐依赖、部署远端、复用登录态并体检 |
| `waldo setup --remote <名称>` | 初始化 `config.toml` 中已配置的命名远端 |
| `waldo <agent> [参数...]` | 在当前项目启动 Agent（内置 `claude`、`codex`） |
| `waldo <agent> --remote <名称>` | 指定远端服务器启动（也可加 `--local-sandbox`、`--observe`、`--exec-mode`） |
| `waldo run -- <命令> [参数...]` | 以空 profile 运行任意命令行 Agent |
| `waldo link [--remove] <agent>` | 生成或移除 `<agent>` → `waldo <agent>` 的 PATH 包装脚本 |
| `waldo doctor [--remote <名称>]` | 检查两端依赖、沙箱状态、时钟偏差与 Agent 登录情况 |
| `waldo remote ls [--json]` | 列出远端配置、默认项和本机活跃会话数（不连接 SSH） |
| `waldo remote status [名称 \| --all]` | 限时只读检查 SSH、waldo 版本和远端依赖，不启动 Agent |
| `waldo sessions [--remote <名称>]` | 查看活跃会话、SSH 状态、PID 和项目（支持 `--json`） |
| `waldo allow [--global] <域名>` | 为开启沙箱的当前项目（或全局）放行出网域名，即时生效 |
| `waldo ports [--remote <名称>]` | 查看当前会话转发到 `127.0.0.1` 的端口（支持 `--json`） |
| `waldo log [--remote <名称>] [-f]` | 查看或实时跟踪本机操作日志，可用 `-s` 筛选会话 |
| `waldo help [命令 [子命令]]` | 分级帮助（例如 `waldo help remote status`）；配置有误时仍可用 |

> Waldo 自身的参数（如 `--remote`、`--local-sandbox`、`--observe`、`--exec-mode`、`--paseo`）需放在 Agent 参数之前；`waldo claude --help` 会透传给远端 Claude，查看 waldo 包装说明请用 `waldo help claude`。

---

## 核心机制

了解以下几点，能帮你更好地理解 waldo 的行为并避开常见误区：

- **单条 SSH 复用，无需常驻服务**：每个会话通过一条 SSH 连接承载终端交互、文件服务与命令转发，不需要在服务器或本机暴露额外端口。
- **远端胶囊 + 本机执行**：远端 Agent 运行在独立的轻量胶囊里，保留自己的账号状态、历史记录和模型凭证（凭证不会转发到本机）；本机负责提供项目文件并执行本机命令。
- **Claude 默认机制（`host-mcp`）为何快如本地**：Claude 启动时默认使用 `host-mcp` 模式——禁用内置的 Bash、Read、Grep、Glob、Edit、Write、NotebookEdit，改由同名 MCP 工具按路径自动分发：项目读写、搜索、编辑与命令直接在本机执行，绕开大量逐文件的跨机往返；Claude memory、会话记录和 `/tmp` 默认留在远端。项目仍保留按需投影，供 `CLAUDE.md`、`AGENTS.md` 和 skills 自动加载。
- **按需文件投影与 `host` 模式的权衡**：项目文件不做全量同步，只在需要时按需传输；远端会话内存会缓存已读取过的小文件，并在每次打开时向本机校验高精度修改时间、大小与文件身份（无目录监听或后台推送，属于时间戳校验的弱一致性缓存）。切换到 `host` 模式时，Claude 继续使用自己的全部内置文件工具来访问这份投影，环境还原度更完整，但目录遍历、大仓搜索和冷读会产生更多跨机元数据与文件往返，网络延迟较高时速度会明显慢于默认的 `host-mcp` 模式，详见 [文件投射性能](docs/configuration.md#文件投射性能)。
- **自动注入会话上下文**：启动时动态生成只读的 `/run/waldo/context/waldo.md` 并虚拟附加到项目根目录指令文件（如 `CLAUDE.md`、`AGENTS.md`），帮助 Agent 理解当前运行模式与路径分工，不会改动你本机的真实文件，也不会留下 Git diff。
- **90 秒断线自动重连**：网络中断被检测到后，waldo 最多等待 **90 秒** 自动重连（要求 SSH 密钥或 `ssh-agent` 等非交互认证）。期间保留远端 Agent、本机运行中的进程与文件挂载，跨机调用短暂等待，恢复后无缝继续；等待期间按 `Ctrl+C` 可取消。超时退出后，已写入本机的项目文件会完整保留，回到项目目录用 `waldo claude -c` 或 `waldo claude --resume` 即可继续对话（已终止的命令需重新执行；Paseo 聊天中可在网络恢复后继续发消息尝试恢复）。

---

## 安全与信任边界

### 默认模式（未开启本机沙箱）

**本机沙箱默认关闭（包括旧版本升级后未显式配置 `[sandbox] enabled` 的会话）。**

在此模式下，通过 waldo 连接的远端服务器可以经由本机工具使用你当前用户的文件权限和网络。**请仅连接你完全信任的服务器。**

即使未开启沙箱，waldo 仍会保持以下基础防护：
- SSH 连接禁用 Agent 转发、X11 转发、隧道与本地命令执行；
- 过滤远端发往终端的危险控制序列（如剪贴板写入 OSC 52、标题回显等）；
- 拒绝将 `/` 或整个家目录作为项目目录投影给远端；
- 清除远端模型 API Key 等凭证变量，不将其转发至本机命令环境；
- 将本机命令与文件操作记录到本地审计日志（可用 `waldo log` 查看）。

### 开启本机沙箱后

通过 `--local-sandbox` 或 `[sandbox] enabled = true` 开启后，waldo 在本机建立明确的安全边界：

- **文件与进程隔离**：Linux / WSL2 使用 bubblewrap 隔离命名空间与文件系统，macOS 使用 Seatbelt 实行默认拒绝策略。项目目录可写，已探测的工具链只读；自动保护 `.git/hooks`、`.git/config`、`.envrc`、`.vscode/` 等高风险路径，同时屏蔽 `~/.ssh` 与 waldo 自身的配置和审计日志。
- **网络与端口约束**：出网必须经由本机可信代理检查域名白名单（拒绝私网、回环及链路本地 IP）；未声明的宿主机端口不可访问。

### 扩展模式的权限提示

- **`--paseo` 桥接**：虽然真实 Bearer 凭据保留在本机内存中不发给远端，但开启 `--paseo` 即授权远端 Agent 使用 Paseo 编排工具（可创建或管理其他 Agent 与工作区），该权限**不受项目文件沙箱约束**。请仅在认可此权限范围时开启。
- **`remote` 执行模式**：普通 Bash 命令在远端服务器执行，使用服务器网络，不受本机出网白名单限制，也不写入本机命令日志。

完整的威胁模型、回归攻击测试和已知残余风险请阅读 [安全说明（SECURITY.md）](docs/SECURITY.md)。

---

## 小贴士与排障

- **遇到问题先跑体检**：运行 `waldo doctor`（多远端可用 `waldo remote status --all`），通常会直接指出缺少的依赖、版本差异或登录状态问题。
- **避免同时编辑同一文件**：Agent 正在修改某个文件时，尽量不要在本机编辑器里同时保存该文件。
- **多 Agent 并发开发**：如果同时开多个会话改同一个仓库，建议使用 `git worktree` 为不同任务分配独立目录。
- **随时查分级帮助**：即使还没初始化或配置文件写错了，`waldo help`、`waldo help remote status` 也能直接运行、不触发 SSH。

---

## 更多文档

- [配置参考](docs/configuration.md)：全部 `config.toml` 配置项、多远端、执行模式、缓存与运行日志详解
- [macOS 使用说明](docs/macos.md)：macOS 安装、Seatbelt 沙箱与端口配置边界
- [Windows 与 WSL2 使用说明](docs/configuration.md#windows-与-wsl2)：WSL2 安装、项目放置与 `windows_ports` 调试桥接
- [安全说明](docs/SECURITY.md)：完整信任边界、沙箱策略与残余风险
- [Agent profile](docs/profiles.md)：内置 profile 说明与接入自定义 Agent
- [开发指南](docs/DEVELOPMENT.md)：源码构建、测试矩阵与代码结构
- [设计方案](DESIGN.md)：架构背景与核心设计决策
