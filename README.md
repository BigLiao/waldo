# waldo

> **Agent 在远端，代码在身边，安心交给它去做。**

waldo 让 Claude Code、Codex 等编程 Agent 运行在你自己的服务器上，而它读写的每一个文件、执行的每一条命令，都发生在你本机一个隔离的沙箱里。

你照旧在本机敲 `claude`，体验与原生无异；不同的是，账号与登录留在服务器，代码不必上传或同步，Agent 也碰不到项目以外的东西。

## 它带来什么

- **一处登录，处处可用**：Agent 只需在服务器上登录一次，你的每一台电脑都能直接使用，会话历史和个人配置自然共享。
- **代码不离开本机**：无需同步、无需上传。`npm start` 跑在本机，浏览器、调试器照常联调。
- **用起来和原生一样**：参数、管道、退出码、权限确认界面都与原生一致，不必改变任何习惯。
- **安全有边界**：Agent 只能看到当前项目目录，出网要走白名单，所做的一切都留有记录。

## 开始之前

请先准备好：

- 一台你能用 SSH 登录的 Linux 服务器（`ssh me@my-server` 能直接连上）；
- 在这台服务器上安装好要使用的 Agent，例如 Claude Code；
- 你正在用的这台电脑同样是 Linux。

其余依赖交给 `waldo setup` 自动补齐，过程中可能需要输入 sudo 密码。

## 安装

从 [GitHub Releases](https://github.com/BigLiao/waldo/releases/latest) 下载适合本机架构的 `waldo-linux-amd64` 或 `waldo-linux-arm64`，以及 `SHA256SUMS`，放在同一目录后安装：

```sh
sha256sum --ignore-missing --check SHA256SUMS
mkdir -p ~/.local/bin
install -m 755 waldo-linux-amd64 ~/.local/bin/waldo  # ARM64 改用 waldo-linux-arm64
waldo version
```

如果服务器与本机架构不同，再把同一 Release 中服务器架构的二进制以原文件名放到 `~/.local/bin/`，供 `waldo setup` 自动安装到服务器。

也可以从源码构建（Go 1.26.8 及以上）：

```sh
git clone https://github.com/BigLiao/waldo.git
cd waldo
make build
```

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
waldo claude                        # 交互式使用，和原生 claude 一模一样
waldo claude -p "跑一下测试" | tee out.txt   # 非交互模式，管道、退出码照常
waldo codex                         # 同样支持 Codex
waldo run -- aider                  # 其他任意命令行 Agent
```

想连 `waldo` 这个词也省掉？

```sh
waldo link claude
```

按提示把一行 `export PATH=...` 加进 `~/.bashrc` 或 `~/.zshrc`，此后直接敲 `claude`，背后就是 waldo。不想要了，`waldo link --remove claude` 即可还原。

## 在 Paseo 聊天界面中使用

支持通过 [Paseo](https://paseo.sh) 聊天界面使用远端 Claude Code。完成 `waldo setup` 后，在**代码所在机器**运行 Paseo daemon，并将以下配置合入 `~/.paseo/config.json`（设置了 `PASEO_HOME` 时为 `$PASEO_HOME/config.json`）。将 waldo 路径替换为 `command -v waldo` 查到的绝对路径：

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

执行 `paseo reload`（daemon 未运行时先执行 `paseo daemon start`），然后在 Paseo 中选择项目，新建聊天并选择 **Claude via Waldo** 即可。账号留在远端，文件操作和命令执行仍在本机沙箱中。详见 [Paseo provider 配置](https://paseo.sh/docs/custom-providers)。

`--paseo` 会通过现有 SSH 连接桥接 Paseo MCP，并只读投影本机已安装的 `paseo`、`paseo-*` skills。请在 Paseo 的主机设置中开启 **Enable Paseo tools**；此参数需要 Paseo 注入的 agent ID 和认证，不能在普通终端中单独开启。它授予远端 agent 使用 Paseo 编排工具的权限，详见 [安全说明](docs/SECURITY.md)。

连接中断导致进程退出时，本轮聊天会报错；网络恢复后可在原聊天中发消息尝试恢复对话，执行到一半的命令不会自动恢复。

## 常用命令

| 命令 | 用途 |
|---|---|
| `waldo setup <user@host>` | 一次性初始化，连接你的服务器 |
| `waldo <agent> [参数...]` | 启动 Agent（内置 `claude`、`codex`） |
| `waldo run -- <命令>` | 启动任意命令行 Agent |
| `waldo doctor` | 遇到问题时先跑它，检查两端环境是否就绪 |
| `waldo allow <域名>` | 允许当前项目访问某个域名，加 `--global` 对所有项目生效 |
| `waldo ports` | 查看 Agent 启动的服务转发到了本机哪些端口 |
| `waldo log` | 回看 Agent 执行过的命令、改过的文件、访问过的网络 |
| `waldo link <agent>` | 让 `claude` 等命令直接走 waldo |

## 按需调整

大多数时候无需任何配置。最常遇到的情况是 Agent 需要访问某个接口或私有仓库，这时放行对应域名即可，正在运行的会话立刻生效：

```sh
waldo allow api.example.com
```

如果 Agent 启动的开发服务器（比如 `localhost:3000`）需要在本机浏览器里打开，waldo 默认会自动转发，用 `waldo ports` 就能看到。

更细的设置，例如传入额外的环境变量、让沙箱访问本机数据库、为不同项目分别配置，都写在 `~/.config/waldo/config.toml` 里，详见 [配置参考](docs/configuration.md)。

## 安全一览

- **只看得见项目**：Agent 的文件读写和命令执行都关在本机的沙箱里，除了当前项目目录，你的主目录、密钥和其他文件对它都不存在。
- **出网有白名单**：默认只放行常见的包仓库和 GitHub，其他域名需要你亲自 `waldo allow`。
- **敏感位置受保护**：`.git/hooks`、`.git/config`、`.envrc`、编辑器配置等以只读方式提供，防止被悄悄篡改。
- **事事有记录**：执行的命令、写入的文件、访问的网络都写进审计日志，随时用 `waldo log` 查看。

更完整的安全模型见 [SECURITY.md](docs/SECURITY.md)。

## 小贴士

- Agent 工作时，尽量不要同时手动修改它正在改的文件。
- 项目默认启用带变化通知的 SFTP 读缓存，本机普通文件修改会异步使远端缓存失效；通知传播期间仍可能短暂读到旧内容。特殊文件系统或写入方式可按[配置参考](docs/configuration.md#文件投射性能)关闭缓存。
- 网络中断后会自动尝试重连，检测到断线后最多等待 **90 秒**，期间保留原 Agent、本地沙箱和文件挂载，跨机文件访问和工具结果可能暂时等待。恢复后继续原会话；超时则退出并清理进程。等待期间可按 `Ctrl+C` 取消。自动重连需要 SSH 密钥或 agent 等非交互认证方式。
- 超时退出后，已经写入项目的文件保留。回到原项目目录，用 `waldo claude -c` 继续最近的对话，或 `waldo claude --resume` 选择历史对话；这会启动新的 Agent 进程，不能恢复已被终止的命令。
- 碰到任何问题，先运行 `waldo doctor`，它通常会直接告诉你缺了什么、该怎么做。

## 更多文档

- [配置参考](docs/configuration.md)：全部配置项与初始化细节
- [安全说明](docs/SECURITY.md)：信任边界与沙箱策略
- [Agent profile](docs/profiles.md)：接入新的 Agent
- [开发指南](docs/DEVELOPMENT.md)：构建、测试与代码结构
- [设计方案](DESIGN.md)：项目的来龙去脉
