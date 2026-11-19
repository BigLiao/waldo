# waldo

> **Agent 在远端，代码在身边，安心交给它去做。**

waldo 让 Claude Code、Codex 等编程 Agent 运行在你自己的服务器上，项目读写、构建和测试则可以在你本机的隔离沙箱里完成。

账号与登录留在服务器，项目和开发环境留在本机，无需整仓复制或同步，Agent 按需获取所需内容。可以直接用终端，也可以接入 Paseo 聊天界面。

## 它带来什么

- **一处登录，处处可用**：Agent 只需在服务器上登录一次，你的每一台电脑都能直接使用，会话历史和个人配置自然共享。
- **复用本机环境**：项目无需整仓同步，`npm start`、测试和依赖安装跑在本机，浏览器、调试器照常联调。
- **减少工具等待**：Claude 的 `host-mcp` 模式在本机完成项目读写、搜索和命令执行，减少大量逐文件的跨机访问。
- **保留常用命令体验**：继续使用 Agent 的参数、管道和退出码，也能在 Paseo 中聊天和编排多个 Agent。
- **安全有边界**：本机工具在沙箱内访问项目和明确共享的路径，出网走白名单，操作可通过日志追踪。

## 开始之前

请先准备好：

- 一台你能用 SSH 登录的 Linux 服务器（`ssh me@my-server` 能直接连上）；
- 在这台服务器上安装好要使用的 Agent，例如 Claude Code；
- 你正在用的这台电脑是 Linux、macOS 或运行 Ubuntu 等 WSL2 发行版的 Windows。[macOS 使用说明](docs/macos.md) · [Windows 与 WSL2 使用说明](docs/configuration.md#windows-与-wsl2)

其余依赖交给 `waldo setup` 自动补齐，过程中可能需要输入 sudo 密码。

## 安装

一键安装最新稳定 Release（需要 `curl`，以及 `sha256sum` 或 Mac 自带的 `shasum`，无需 Go 或 sudo）：

```sh
curl -fsSL https://raw.githubusercontent.com/BigLiao/waldo/master/install.sh | sh
```

脚本自动识别 Linux/macOS amd64/arm64，固定 Release 版本后下载并校验本机程序和两个 Linux 配套产物，安装到 `~/.local/bin`，同时准备好服务器需要的文件。macOS 需使用包含 Mac 产物的 Release（v0.4.1 起），较早版本仅提供 Linux 产物。下载或校验失败时不会替换已有安装，主程序以原子替换方式更新；不修改配置、登录状态或 shell 配置。若安装目录尚未在 PATH 中，按脚本提示添加。

指定版本或安装目录：

```sh
curl -fsSL https://raw.githubusercontent.com/BigLiao/waldo/master/install.sh | sh -s -- --version v0.3.1 --bin-dir "$HOME/.local/bin"
```

安装后运行 `waldo setup me@my-server` 初始化依赖和服务器。脚本安装的是已发布版本；主分支尚未发布的功能可用下方的源码构建方式体验。

Windows 用户在 WSL2 发行版的终端里安装和运行 Waldo，项目放在 WSL 的 Linux 文件系统。Windows 工具可以打开同一份项目，并通过按项目配置的 `windows_ports` 提供本机调试服务。[安装、工作区与调试配置](docs/configuration.md#windows-与-wsl2)

Linux 也可从 [GitHub Releases](https://github.com/BigLiao/waldo/releases/latest) 手动下载适合本机架构的 `waldo-linux-amd64` 或 `waldo-linux-arm64`，以及 `SHA256SUMS`，放在同一目录后安装；Mac 推荐使用上方的一键安装：

```sh
sha256sum --ignore-missing --check SHA256SUMS
mkdir -p ~/.local/bin
install -m 755 waldo-linux-amd64 ~/.local/bin/waldo  # ARM64 改用 waldo-linux-arm64
waldo version
```

手动安装时，如果服务器与本机架构不同，再把同一 Release 中服务器架构的二进制以原文件名放到 `~/.local/bin/`，供 `waldo setup` 自动安装到服务器；一键安装已包含这一步。

也可以从源码构建（Go 1.26.8 及以上）：

```sh
git clone https://github.com/BigLiao/waldo.git
cd waldo
make build
```

macOS 从源码构建并连接 Linux B 时，使用 `make build-pair BUILD="dev-$(date +%s)"` 构建匹配的本机程序与两个 Linux 配套程序，并将它们放在同一目录。

得到的 `./waldo` 是一个独立的二进制文件，把它放进 `PATH` 中的任一目录即可，例如：

```sh
install -m 755 waldo ~/.local/bin/waldo
```

## 初始化：只需一次

```sh
waldo setup me@my-server
```

这一步会自动完成：

1. 检查并安装本机和服务器缺少的依赖；
2. 把 waldo 安装到服务器上；
3. 沿用服务器上已有的 Agent 配置与登录；
4. 做一次全面体检，并告诉你结果。

如果服务器上还没有登录过 Agent，也没关系——第一次启动时按提示登录即可，之后所有电脑都不必再登录。

## 日常使用

进入项目目录，像平常一样启动 Agent：

```sh
cd ~/work/shop
waldo claude                        # 交互式使用，沿用原有默认模式
waldo claude -p "跑一下测试" | tee out.txt   # 非交互模式，管道、退出码照常
waldo codex                         # 同样支持 Codex
waldo run -- aider                  # 其他任意命令行 Agent
```

想连 `waldo` 这个词也省掉？

```sh
waldo link claude
```

按提示把一行 `export PATH=...` 加进 `~/.bashrc` 或 `~/.zshrc`，此后直接敲 `claude`，背后就是 waldo。不想要了，`waldo link --remove claude` 即可还原。

## Claude 宿主机工具模式：host-mcp

**项目搜索、文件读写和命令执行较多时，推荐使用 `host-mcp`，能显著减少跨机文件访问带来的等待。** 搜索和脚本直接在本机沙箱中处理文件，再把需要的内容和执行结果返回给远端 Agent；大量文件访问时通常会更快，具体提升取决于网络延迟和任务。

```sh
waldo claude --exec-mode host-mcp
```

也可在本机 `~/.config/waldo/config.toml` 中默认开启：

```toml
[execution]
mode = "host-mcp"
```

Waldo 会禁用 Claude 内置的 Bash、Read、Grep、Glob、Edit、Write、NotebookEdit，并注入同名 MCP 工具。文件工具按路径分发：项目走本机，Claude memory、会话记录和 `/tmp` 默认走远端；Bash 默认在本机沙箱执行。工具支持用 `location=auto/host/agent` 指定位置。项目仍保留按需投影，供项目说明和 skills 加载使用。

| 模式 | 工具如何执行 |
|---|---|
| `host`（默认） | 沿用内置文件工具，通过投影访问项目；PATH 命令转发到本机沙箱 |
| `host-mcp` | 使用同名 MCP 工具，项目读写、搜索和命令在本机完成，减少跨机文件往返 |
| `remote` | 内置 Bash 在远端运行，需要本机环境时调用注入的 Bash MCP |

`host-mcp` 和 `remote` 当前适用于内置 Claude。若全局启用了这两种模式，使用 Codex 时加 `--exec-mode host`；也可仅在 Claude 启动命令中指定模式。详细工具参数见[命令执行模式](docs/configuration.md#命令执行模式)。

## 在 Paseo 聊天界面中使用

支持通过 [Paseo](https://paseo.sh) 聊天界面使用远端 Claude Code。完成 `waldo setup` 后，在**代码所在机器**运行 Paseo daemon，并将以下配置合入 `~/.paseo/config.json`（设置了 `PASEO_HOME` 时为 `$PASEO_HOME/config.json`）。将 waldo 路径替换为 `command -v waldo` 查到的绝对路径：

```json
{
  "agents": {
    "providers": {
      "waldo-claude": {
        "extends": "claude",
        "label": "Claude via Waldo",
        "command": ["/home/me/.local/bin/waldo", "claude", "--exec-mode", "host-mcp", "--paseo"]
      }
    }
  }
}
```

执行 `paseo reload`（daemon 未运行时先执行 `paseo daemon start`），然后在 Paseo 中选择项目，新建聊天并选择 **Claude via Waldo** 即可。上例启用 `host-mcp`，项目工具走本机沙箱，账号与 Claude 状态留在远端。详见 [Paseo provider 配置](https://paseo.sh/docs/custom-providers)。

`--paseo` 会通过现有 SSH 连接桥接 Paseo MCP，并只读投影本机已安装的 `paseo`、`paseo-*` skills。请在 Paseo 的主机设置中开启 **Enable Paseo tools**；此参数需要 Paseo 注入的 agent ID 和认证，不能在普通终端中单独开启。它授予远端 agent 使用 Paseo 编排工具的权限，详见 [安全说明](docs/SECURITY.md)。

首轮聊天流会显示启动状态和初始化耗时，初始化完成后再发送首条消息。重连超时导致进程退出时，本轮聊天会报错；网络恢复后可在原聊天中发消息尝试恢复对话，已被终止的命令需重新执行。

## 多台远端、独立账号

一台本机可以连接多台服务器，每台服务器保留自己的 Claude 账号。在本机 `~/.config/waldo/config.toml` 中配置，`default_remote` 放在文件顶部、其他表段之前：

```toml
default_remote = "b1"

[remote.b1]
host = "ubuntu@server-1"

[remote.b2]
host = "ubuntu@server-2"
```

```sh
waldo setup --remote b1
waldo setup --remote b2
waldo claude --remote b2 --exec-mode host-mcp
waldo remote ls                    # 配置列表、默认远端与活跃会话数
waldo remote status --all          # 限时检查连接和远端环境，不安装或启动 Agent
waldo sessions --remote b2         # 会话、SSH 状态、PID 和项目
```

在 Paseo 中为每台机器配置一个 provider，启动命令增加 `--remote b2` 即可。会话固定使用所选机器，切模型、断线重连都不会自动换机。旧版单机 `host` 配置无需修改；未设置 `host` 时，唯一的命名远端会自动选中。更多选项见[多远端配置](docs/configuration.md#多远端机器)。

## 常用命令

| 命令 | 用途 |
|---|---|
| `waldo setup <user@host>` | 一次性初始化，连接你的服务器 |
| `waldo <agent> [参数...]` | 启动 Agent（内置 `claude`、`codex`） |
| `waldo <agent> --remote b2` | 选择命名远端；初始化和检查可用 `waldo setup --remote b2`、`waldo doctor --remote b2` |
| `waldo run -- <命令>` | 启动任意命令行 Agent |
| `waldo doctor` | 遇到问题时先跑它，检查两端环境是否就绪 |
| `waldo remote ls` / `waldo remote status --all` | 查看远端配置，或只读检查连接和远端环境；支持 `--json` |
| `waldo sessions [--remote b2]` | 查看活跃会话和 SSH 状态，支持 `--json` |
| `waldo allow <域名>` | 允许当前项目访问某个域名，加 `--global` 对所有项目生效 |
| `waldo ports [--remote b2]` | 查看转发端口，支持 `--json` |
| `waldo log [--remote b2] [-f]` | 查看或跟踪操作日志，可按远端筛选 |
| `waldo <agent> --observe` | 追加模型调用统计（Claude / Codex）；Claude 同时在 B 侧记录工具 hook 耗时 |
| `waldo claude --exec-mode host-mcp` | 项目工具通过 MCP 在本机执行，减少跨机文件访问 |
| `waldo claude --exec-mode remote` | 内置 Bash 在远端执行，需要本机环境时调用 Bash MCP |
| `waldo link <agent>` | 让 `claude` 等命令直接走 waldo |
| `waldo help [命令 [子命令]]` | 分级帮助，例如 `waldo help remote status`；配置写错时也可查看 |

## 按需调整

大多数时候无需任何配置。最常遇到的情况是 Agent 需要访问某个接口或私有仓库，这时放行对应域名即可，正在运行的会话立刻生效：

```sh
waldo allow api.example.com
```

如果 Agent 启动的开发服务器（比如 `localhost:3000`）需要在本机浏览器里打开，waldo 默认会自动转发，用 `waldo ports` 就能看到。

更细的设置，例如传入额外的环境变量、让沙箱访问本机数据库、为不同项目分别配置，都写在 `~/.config/waldo/config.toml` 里，详见 [配置参考](docs/configuration.md)。

排查卡顿时，在另一个终端运行 `waldo log -f`：`exec-end` 显示命令耗时和退出状态，`file-read` / `file-write` 汇总文件 RPC。用 `--observe` 或配置 `[observe] enabled = true` 追加模型请求统计；Claude 还会在远端 `~/.local/share/waldo/log/tool-timing.jsonl` 记录工具耗时，MCP 统计包含本机/远端执行位置。`--observe=false` 可临时关闭。统计不保存文件内容或命令输出，各项耗时的范围见[轻量运行日志](docs/configuration.md#轻量运行日志)。

## 安全一览

- **本机访问受沙箱约束**：工具可访问项目、明确共享的路径和只读工具链；远端保留自己的登录状态、memory 和临时目录。
- **本机工具出网有白名单**：默认放行常见包仓库和 GitHub，其他域名用 `waldo allow`；远端模型及远端工具使用服务器网络。
- **敏感位置受保护**：`.git/hooks`、`.git/config`、`.envrc`、编辑器配置等以只读方式提供，防止被悄悄篡改。
- **活动可追踪**：用 `waldo log` 查看操作记录，用 `--observe` 进一步定位模型和工具耗时。

更完整的安全模型见 [SECURITY.md](docs/SECURITY.md)。

## 小贴士

- Agent 工作时，尽量不要同时手动修改它正在改的文件。
- 文件投影只缓存实际读取过的内容，每次打开时逐文件校验高精度修改时间，无目录监听或主动更新推送；这是时间戳校验的弱一致性缓存，详见[配置参考](docs/configuration.md#文件投射性能)。
- 网络中断后会自动尝试重连，检测到断线后最多等待 **90 秒**，期间保留原 Agent、本地沙箱和文件挂载，跨机文件访问和工具结果可能暂时等待。恢复后继续原会话；超时则退出并清理进程。等待期间可按 `Ctrl+C` 取消。自动重连需要 SSH 密钥或 agent 等非交互认证方式。
- 超时退出后，已经写入项目的文件保留。回到原项目目录，用 `waldo claude -c` 继续最近的对话，或 `waldo claude --resume` 选择历史对话；这会启动新的 Agent 进程，不能恢复已被终止的命令。
- 碰到任何问题，先运行 `waldo doctor`，它通常会直接告诉你缺了什么、该怎么做。

## 更多文档

- [Windows 与 WSL2 使用说明](docs/configuration.md#windows-与-wsl2)
- [配置参考](docs/configuration.md)：全部配置项与初始化细节
- [安全说明](docs/SECURITY.md)：信任边界与沙箱策略
- [Agent profile](docs/profiles.md)：接入新的 Agent
- [开发指南](docs/DEVELOPMENT.md)：构建、测试与代码结构
- [设计方案](DESIGN.md)：项目的来龙去脉
