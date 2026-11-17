# macOS 本机端

macOS 作为 A，Agent 继续运行在 Linux B。Mac 使用原生 Seatbelt 执行本机工具，不需要安装 macFUSE、SSHFS 或虚拟机。Linux A 与 Mac A 可以同时连接同一 B：每个连接运行独立 Agent 进程，B 的 Claude 安装、登录、配置和历史继续共享。

当前已在 Apple Silicon、macOS 15.6.1 上运行原生回归，并交叉构建 darwin/amd64。Intel Mac 的原生运行仍需验证。

## 安装

macOS 需要包含 Mac 产物的 Release（v0.4.1 起）；v0.3.1 仅有 Linux 产物。可使用与 Linux 相同的一键安装命令：

```sh
curl -fsSL https://raw.githubusercontent.com/BigLiao/waldo/master/install.sh | sh
waldo setup me@linux-server
waldo claude --exec-mode host-mcp
```

安装脚本自动下载本机 Mac 程序及两个 Linux B 配套程序，以 `sha256sum` 或系统 `shasum -a 256` 校验后安装到 `~/.local/bin`。若该目录尚未在 PATH 中，按脚本提示添加。

## 从源码使用

也可使用 Go 1.26.8+ 构建配套程序：

```sh
make build-pair BUILD="dev-$(date +%s)"
./waldo setup me@linux-server
./waldo claude --exec-mode host-mcp
```

将 `waldo`、`waldo-linux-amd64`、`waldo-linux-arm64` 放在同一目录。配套构建使用相同 Version/Build；仅运行 `make dev` 得到的本机文件哈希与 Linux 程序不同，不能作为跨系统配套版本。

`make release` 生成 Linux/macOS、amd64/arm64 四种程序和校验文件。

`setup` 使用已有 Homebrew 安装缺失的 git、bash、ripgrep，不自动安装 Homebrew。Seatbelt 不可用时启动失败，不回退为直接执行。`doctor` 会实际验证沙箱可执行、项目可写、秘密文件不可读及 Git 配置不可改。

## 本机执行环境

- 项目读写使用真实 Mac 工作区；HOME 为 Waldo 私有持久目录，TMPDIR/TMP/TEMP 为每会话私有目录。
- 系统和已发现的工具链只读；宿主登录、SSH、Keychain 和 Waldo 策略/日志不向沙箱开放。
- 文件工具和 shell 使用 Mac 工具链。B 的 `uname`、系统路径及原生程序仍属于 Linux，不能据此推断 A 的平台。
- Claude 支持 host、host-mcp、remote；host-mcp 的项目 Bash/Read/Write 经过现有 SSH 通道在 Mac 沙箱执行。remote 的原生 Bash 留在 B，注入的 Bash MCP 默认使用 A。
- B 的 Claude 可复用其登录 shell 中的凭证、服务地址及模型变量。这些变量只在 B 注入 Agent，并在转发到 A 时清除；不会复制到 Mac 登录环境。

Mac 没有挂载命名空间。硬编码 `/tmp/foo` 不会因 TMPDIR 设置而改写；B `/tmp` 与 A 私有 tmp 分开，跨端共享应使用会话说明公布的实际目录。Paseo skill 会复制为只读会话副本；文件工具可翻译 B 的 skill 路径，在 A 上执行 skill 配套命令时使用会话说明中的 A 路径。含符号链接或特殊文件的 skill 副本会被拒绝。

## 网络与开发服务

出网必须经过每会话认证的回环代理，由可信 Waldo 检查域名白名单和目标 IP。沙箱不能任意直连外网或使用系统 DNS 服务，代理认证不会发送到上游。

开发服务端口需显式配置：

```toml
[net]
ports = [3000, 5173]
host_ports = [5432]
```

`ports` 允许相应本机监听端口；请让开发服务绑定 `127.0.0.1`。`host_ports` 允许沙箱直接连接对应宿主回环服务；如果命令需要访问自身开发服务，也需将该端口列入 `host_ports`。Mac 与宿主共享网络，端口许可不能区分同一端口上的不同服务。

Mac 当前不自动发现/发布端口，`ports="auto"` 会提示配置，`waldo ports` 不提供 Mac listener 的自动清单；`ports="off"` 禁止开发监听。不同电脑可以各自使用 localhost:3000；同一 Mac 上多个会话使用同一端口仍会冲突。

## 原生兼容边界

- 不默认开放 `/var/folders` 中的用户共享 T/C 目录。已验证 Node、Go 和最小 C/Swift 程序可运行；Xcode 可能对系统缓存和文件监听发出警告，复杂 Xcode、模拟器和工具插件需要另行验证。
- 工具再次建立 Seatbelt 时可能失败。SwiftPM、macro/plugin、Chromium/Electron 的支持取决于版本和参数；不会自动关闭工具自带沙箱。
- 普通命令组和后台 job 随连接清理。setsid/double-fork 脱离监管的 daemon 不保证全部回收；它仍受 Seatbelt 约束，但可能继续使用已授权项目和端口。
- Claude 原生历史按项目路径组织，Mac 与 Linux 的 cwd 不同可产生不同 `--continue` 列表。Waldo 不按设备隔离历史，也不自动同步项目文件。

## 验证

```sh
go vet ./...
go test -p 2 ./internal/... ./test/install
make test-native
```

`test/native` 运行真实 Seatbelt、代理认证、文件保护、APFS 大小写/Unicode 别名、信号、端口和后台清理测试。执行环境若本身不允许嵌套 Seatbelt，需在原生终端运行；测试不会把无法启动沙箱视作通过。
