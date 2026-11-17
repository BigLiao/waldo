# macOS A 端支持：调研与实施方案

调研日期：2026-11-16。状态：源码适配已实现，已完成 Mac/Linux 真机验收；发布与 Intel Mac 原生验证待后续。

## 1. 结论与范围

建议采用 **macOS 原生 Seatbelt 后端 + 现有 Linux B 端**。继续使用 SSH、yamux、SFTP 和 B 上的 SSHFS/bubblewrap，不在 Mac 上安装 FUSE，也不默认引入虚拟机。这样构建、测试仍使用用户的 Mac 工具链，符合项目“复用本机环境”的定位。

这不是增加 `GOOS=darwin` 就能完成的移植。必须一起处理编译边界、物理目录布局、网络策略、进程生命周期、跨系统信号及交付测试。现有 `sandbox.Backend` 是起点，但只有 `Command`，尚不能表达这些差异。

目标范围：

- A：darwin/arm64、darwin/amd64；B：继续仅支持 linux/amd64、linux/arm64。
- **同一台 Linux B、同一 SSH 用户和同一 remote_dir，必须同时服务 macOS A 与 Linux A。** B 共用一套 Claude 安装、登录、Agent 状态和会话历史，每个连接启动独立的 Claude 进程；项目挂载与工具请求各自绑定对应 A。
- 保留 Linux A 的现有行为；B 允许为跨平台协议做改动，不新增 macOS B。
- 最终覆盖 Claude 的 host/host-mcp/remote，以及 Codex/run 现有 host 模式；先以 Claude host-mcp 完成贯通，不能用它代替其余模式的最终验收。
- 覆盖 setup、doctor、link、重连、会话管理、observe、Paseo 桥接。
- macOS 最低版本先不承诺：以项目 Go 版本支持范围和实际双架构 CI/真机结果定版。本次宿主机是 macOS 15.6.1，仅能作为一个验证样本。

**关键约束：Seatbelt 不提供 Linux mount/net/PID namespace 的等价物。** 原生实现必须明确目录与 localhost 行为差异；若要求完全保持 Linux 的隔离语义，应选择 VM 方案，且执行环境会变为 VM 内 Linux。不能同时承诺“原生 Mac 工具链”和“全部 Linux namespace 语义不变”。

## 2. 代码证据与改动面

| 领域 | 当前实现 | 所需改动 |
|---|---|---|
| A 沙箱 | `internal/sandbox/sandbox.go`：Default 固定 Bwrap；HOME、tmp、context、skills、exe 都依赖 bind mount | 拆公共模型与 Linux 后端；新增 Darwin policy、布局与能力声明 |
| A 生命周期 | `internal/session/sandbox.go`：固定代理 39339；关闭只杀 sandbox-serve，依赖 PID namespace 收尾 | 每会话资源、动态代理端口、平台清理与启动失败回收 |
| 编译边界 | `cmd/waldo/main.go` 无条件引入 host；`internal/host/host.go:368` 使用 Linux 专用 `Pdeathsig` | 用平台 helper 隔离 Pdeathsig；Darwin 明确拒绝 B 角色；完整角色拆包可选 |
| 公共类型 | `internal/fileversion/protocol.go:37` 把 Stat_t.Mode 直接赋给 uint32 | Darwin 的 Mode 为 uint16，显式转换；检查所有 Stat_t/平台常量 |
| CLI 耦合 | cli/remotes、cli/cli 直接引用 host.DoctorReq/Report、Distro、InstallHint | 本机依赖检查按平台实现；DTO 抽公共包是可选整理，不是编译前提 |
| 文件工具入口 | `internal/hostbash/exec.go` 固定发 `/run/waldo/waldo host-file-tool` | 使用协议中的 A helper 信息，或固定的 helper 操作枚举，在 A 沙箱内解析成本机 exe |
| 会话说明 | `proto.ContextDir` 固定 `/run/waldo/context`；projection.Reference 为常量 | context 路径随会话布局生成，贯通投影、WALDO_CONTEXT、说明引用、缓存排除 |
| HOME/共享目录 | `Spec.Home` 和 SandboxHome 依靠挂载保持同一路径、不同内容 | native HOME 指向实际沙箱目录；同步调整 Facts、profile.shared、skills 和 B HOME 绑定 |
| 临时文件 | session 默认把整个沙箱 `/tmp` 投影到 B | 不能把 Mac 宿主 `/tmp` 当作私有 tmp 导出；改成会话私有真实目录及显式导出 |
| 网络入口 | `internal/serve/net.go` 在沙箱监听代理/host_ports | macOS 共用宿主网络，不能照搬 host_ports 中继和固定监听端口 |
| 端口发现 | serve/net 读取 `/proc/net/tcp{,6}`；netpolicy.Forwarder 再绑定同端口 | Linux 保持原样；Darwin 采用直接监听及会话归属发现，不扫描并发布整机端口 |
| 信号协议 | proto.FSignal 是原始数字单字节；Exit.Signal 也是 OS 数字 | 保留帧结构，规定协议编号为 Linux 信号编号，Darwin 双向映射 |
| 工具链 | sandbox.Toolchains 主要针对 Linux HOME；BaseEnv 无 TMPDIR | 增加 Homebrew、CLT/Xcode、SDK、系统库、证书与私有缓存目录支持 |
| 分发 | Makefile、release.yml 只产 Linux；install.sh 要 sha256sum、使用 GNU mv -fT | 四种 OS/arch 产物，Mac 校验与 BSD 工具兼容，保留 Linux B 配套产物 |
| B 多连接 | host.prepare 使用共享 `dataDir/home` 和 adopted Agent 状态；runDir、挂载与进程按连接创建 | 保留共享状态，验证 Mac/Linux 并行进程的文件和命令不会串到另一 A |
| B 安装并发 | transport.bootstrap/Install/Run 共用 `bin/waldo`，按 FullVersion 精确匹配 | B 程序按版本并存；Open/Run/reattach 固定版本路径，原子安装并完整校验 |
| 自动化测试 | harness.New 要 bwrap；E2E 在同机启动 Linux B；多处断言 `/proc` | 拆 Mac A 单机攻击测试与 Mac A → Linux B 跨机 E2E |

Linux 审查者在同一提交 `b61a46c` 上使用 Go 1.26.8，对 darwin/arm64、darwin/amd64 执行 `CGO_ENABLED=0 go build ./...` 与 `go vet ./...`，报告编译错误只有 Stat_t.Mode 和 Pdeathsig 两处；在临时副本修正后，两架构 build/vet（含测试包）均通过。这是交叉构建结果，不代表 Mac 原生运行测试通过。DTO 抽取和完整角色拆包不列为编译必需项。

SSH 安装逻辑已有有利基础：`transport.binaryFor()` 按 OS/arch 寻找同目录 `waldo-<os>-<arch>`。但目前 Install 对配套二进制的返回版本检查比自身二进制弱，移植时应统一校验，避免跨系统版本不匹配后反复安装。

`stub.execFn()` 读 Linux `/proc/self/auxv` 和 `/proc/self/mem` 是 B 侧用途，不应为了 Mac A 重写；隔离平台 syscall 并阻止 Darwin 启动 B 角色即可。`hostbash/route.go` 的 mountinfo 解析同样属于 Linux B。不要把所有 `/proc` 命中都当成 A 端移植项。

## 3. 方案取舍

| 路线 | 优点 | 代价 | 建议 |
|---|---|---|---|
| 原生 Seatbelt | 复用 Mac 工具链；无 VM/FUSE；保留现有 Go 通道 | 无目录重映射和网络/PID namespace；需要新运行时布局和策略测试 | 主线，先完成可行性验证 |
| Linux VM（如 Lima） | 可最大程度复用 bwrap；隔离语义更接近现状 | 工具在 VM 运行；文件共享、依赖、端口和资源管理增加成本 | 严格隔离需求的备选，单独立项 |
| 仅设置代理变量/直接执行 | 初期改动少 | 命令可绕过代理、访问宿主文件和服务，破坏安全承诺 | 不采用 |

Seatbelt 的文件与网络规则可参考 Anthropic 的 macOS 实现；其中明确指出 SBPL 不能把读取重定向为另一份文件内容，也展示了 localhost 端口过滤和 local-binding 的区别。[上游源码](https://github.com/anthropics/sandbox-runtime/blob/main/src/sandbox/macos-sandbox-utils.ts)

本机 `man sandbox-exec` 明确标为 DEPRECATED，且 `/usr/bin/sandbox-exec` 仍存在。使用绝对路径并在 doctor 中执行能力探针；系统缺失或规则失效时停止启动，不退回无沙箱执行。当前可用不代表未来 macOS 的长期兼容承诺。

## 4. 推荐的实现设计

### 4.1 最小平台修复与后端扩展

- 公共层保留 Spec、受保护路径、工具链描述和 Backend 抽象；Bwrap 移入 Linux 文件。
- Linux 的 B 主流程仍使用现有 host；将 Pdeathsig 设置放入平台 helper，Darwin 不编译 Linux 专用字段。Darwin 入口对 host/stub 等 B 专属角色给出清晰错误。
- 注意 `stub.Relay` 也被 A 的 dev-sandbox 使用，不能直接将整个 stub 包排除在 Darwin 构建之外。
- DoctorReq/Report 抽公共包、完整 host 角色拆包可后续整理；首版可保留现有依赖，只隔离必要 syscall，不扩大编译修复范围。
- CI 实际构建 cmd，并检查公共包和测试的传递依赖，不以入口文件的 build tag 代替全包构建验证。
- 后端返回运行时布局/能力和清理句柄，而非只有 exec.Cmd。可采用 Prepare → Start → Close；名称与接口大小以实现时最小可用范围为准。

建议能力字段至少覆盖：路径重映射、私有网络、私有进程空间、端口发布方式。由实际后端提供能力，session/context/doctor 据此呈现真实行为。

### 4.2 原生文件系统布局

推荐 **A 使用实际物理路径，B 通过现有 bwrap 挂载到约定路径**，不要试图在 Seatbelt 内伪造 mount namespace。

```text
~/.local/share/waldo/
  sandbox-home/              A 工具的持久 HOME/缓存
  runtime/<session>/
    tmp/                     会话私有 TMPDIR
    context/waldo.md          只读说明
    policy.sb                策略文件，仅可信侧可写
    skills/                  必要的只读导出副本
  log/                       可信侧日志，沙箱无读取/修改权限
  run/                       可信侧状态，沙箱无读取/修改权限
```

- 保留当前配置/数据根目录约定，不顺带迁到 Library，避免引入配置迁移。
- `HOME` 在 Mac 指向 sandbox-home。B 暂沿用 Facts.Home 作为“路径字符串”，将共享 agentHome 绑定在那里；登录状态仍来自 B。P0/P3 必须验证同一共享状态挂在不同 HOME 字符串下的兼容性：插件 installLocation、installed_plugins/known_marketplaces、settings hooks 命令和 MCP 路径均可能是绝对路径。若失效，优先评估 B capsule 使用独立于 A 的固定 HOME，并贯通 A 工具环境、shell-snapshots/shared 投影和路径路由；不得以按设备复制状态规避问题。
- 共享路径按新 HOME 展开并显式生成导出清单。不能继续把 `~/.claude/shell-snapshots` 展开到宿主真实 HOME。
- 增加导出描述 `{source, target, read_only}` 或等价模型。B 的 target 可以保持 Linux 逻辑路径，SFTP 请求 source；不通过应用层路径检查替代 Seatbelt。
- context 在 A 使用真实路径；会话协议明确 A/B 路径。projection.Reference 改成按会话生成，B 的 `/run/waldo` 仍可保留。
- host-file-tool 使用明确的 helper 调用类型，在 A 的受限进程内解析实际 exe，避免要求 Mac 创建 `/run`。不能在可信 A 进程中执行远端任意 helper。
- Paseo skills 的 Source→Target 原来依靠绑定实现。Mac 初版将明确选中的 skill 复制到 runtime/skills 并只读授权；不复制整个 `.agents`/`.claude`。副本为会话快照，文档说明变化在下次会话生效。
- 将 HOME、TMPDIR/TMP/TEMP、上下文及导出路径作为统一布局传递，不能只改环境变量而遗漏 SFTP、MCP、B 挂载和缓存分类。

**tmp 的兼容边界必须明确：** 设置 TMPDIR 不会改写硬编码 `/tmp/foo` 的系统调用。Mac 默认只允许私有 tmp，不放开整个宿主 `/tmp`；B 的 `/tmp` 是 B 私有目录，跨端共享使用显式公布的 session tmp 路径。旧 host/remote 模式的 `/tmp` 共视行为不能原样承诺。必须测试 Claude shell snapshots 与 Codex 的临时文件访问；不兼容时给出具体错误/说明，不能静默导出宿主 tmp。

macOS 的 `confstr(_CS_DARWIN_USER_TEMP_DIR/_CS_DARWIN_USER_CACHE_DIR)` 还会返回用户级 `/var/folders/.../{T,C}`（真实路径需规范化），不因设置 TMPDIR 就变成 Waldo 私有目录；本机 `man confstr` 已确认两个接口。本方案默认不授权整棵 `/var/folders` 或整个用户 T/C，也不将其导出到 B。P0 用 clang module cache、swift、xcrun 分别测试私有缓存参数/环境变量能否满足运行；无法重定向且确需共享缓存的工具，在默认策略下明确列为受限。若后续提供兼容配置，只授权经验证的最小子目录并明确其与宿主进程共享读写的风险，不静默放开用户 T/C。Apple 工具链支持范围随实测结果发布。

### 4.3 Seatbelt 文件与系统能力策略

- 默认拒绝；按需授权项目读写、私有 HOME/tmp 读写、系统运行库/工具链只读、session context/skills 只读。
- 不采用全盘可读策略。宿主 `.ssh`、`.aws`、`.codex`、`.claude`、浏览器、Keychain、waldo 配置/凭证/日志保持不可读。
- 系统路径按探针确定，重点覆盖 `/System`、`/usr`、必要 `/Library` 子目录、`/dev` 与证书服务。避免为兼容工具而授权整个 `/Library` 或用户 `Library`。
- Homebrew：Apple Silicon `/opt/homebrew`，Intel `/usr/local`；用户工具链和实际 symlink 目标均需处理。编译器需覆盖 CLT/Xcode/SDK 的实际位置，不向沙箱授予自动安装系统组件的权限。
- 规范化 `/tmp`→`/private/tmp`、`/var`→`/private/var`、路径中的符号链接；支持空格、引号、Unicode。策略参数与 SBPL 转义独立实现，不能复用 shell quoting。
- 对 ProtectedNames 拒绝写入/删除/重命名/替换，同时考虑父目录重命名、缺失路径新建、`.git` 为文件的 worktree 和外部 commondir。P0 在大小写不敏感 APFS 上测试 `.GIT/hooks`、`.Git/config` 等路径别名，并在可适用路径测试 NFC/NFD 等价变体，确认实际文件对象不能绕过 SBPL 保护。不能只按字符串大小写匹配。
- snapshot 只能事后告警，不能当作保护生效的证据。应直接拒绝创建原本不存在的敏感目标，并验证父目录替换不能绕过。
- 所需 PTY、进程创建、同沙箱 signal、必要 Mach 服务逐项探测。禁止笼统放开 Mach、Unix socket、Apple Events；重点阻断 Docker/Paseo/SSH agent 等宿主控制接口。

嵌套 Seatbelt 是已知兼容性限制：工具在受限进程内再次调用 sandbox-exec/sandbox_init 的路径可能失败。P0 测试 SwiftPM manifest、含 macro/plugin 的 Swift 包、xcodebuild 脚本步骤，以及 Chromium/Electron/Playwright。不能把整个工具名称等同于必然失败：Playwright 的 `chromiumSandbox` 当前默认 false，具体版本和启动参数决定是否再建沙箱。[Playwright 文档](https://playwright.dev/docs/api/class-browsertype#browser-type-launch-option-chromium-sandbox)

针对实际确认的嵌套失败，文档可提供 `swift build --disable-sandbox`、Chromium/Electron 的 `--no-sandbox` 或 Playwright 的 `chromiumSandbox: false` 等适用开关，由用户显式选择，Waldo 不自动注入。它们仅关闭工具内层沙箱，外层 Seatbelt 仍必须生效，也不保证解决所有运行问题。SwiftPM 的 disable-sandbox 不能视作 macro/plugin 路径的通用修复，上游有对应问题记录；P0 按实际 Swift/Xcode 版本确认支持范围。[SwiftPM 问题记录](https://github.com/swiftlang/swift-package-manager/issues/7098)

### 4.4 网络策略：默认受限，开发端口显式授权

推荐首版采用精确端口策略，保留域名白名单；**不默认允许 localhost 任意连接**。

1. 可信 A 预绑定每会话随机 loopback 代理端口并保持所有权；Seatbelt 只允许连该端口，外网连接仍由 netpolicy 校验、解析并拨号。HTTP_PROXY 等仅负责工具适配，真正约束来自内核规则。
2. 可信 A 承载 loopback HTTP 代理，复用 Policy 的授权与审计；Linux 继续使用 namespace 内代理。Mac 代理必须使用每会话随机 Proxy-Authorization token：普通 HTTP 与 CONNECT 均先认证，无 token 或错误 token 均拒绝；转发前剥离该头，token 不写入日志/历史，也不传给上游或 B。会话结束时关闭 listener、已有连接并撤销 token。测试另一用户/进程无 token 请求被拒绝；这不构成对能读取同一用户进程环境的攻击者的安全边界。代理客户端不支持认证时明确报错，不退回无认证代理。
3. `host_ports` 在 Mac 上表示允许连接相应宿主 loopback 服务，直接授权，不再启动同端口中继，否则会与真实服务冲突。
4. Mac 开发服务与宿主共享端口，不再调用 Forwarder 绑定相同端口。初版要求 `ports=[3000,5173]` 显式列出可监听端口；默认 auto 显示“Mac 当前不支持自动发布，请配置端口”，不转换成全 localhost 放行。
5. 分别验证 bind、入站、出站、IPv4/IPv6 和通配监听。禁止把 bind 权限误用为任意出站权限，也不能假设一个 loopback 规则覆盖所有 IPv4-mapped IPv6 形式。
6. “允许浏览器访问开发服务”和“允许沙箱 curl 开发服务”是不同权限。初版允许指定端口的入站；沙箱连接该端口仍需列入明确的 loopback 连接授权，界面/文档说明。
7. DNS 默认由可信 A 代理解析，沙箱不获准直接访问 UDP/TCP 53、DoT 853、`/var/run/mDNSResponder`（含真实路径）或相关 DNS IPC/Mach 服务；拒绝经系统解析服务进行任意域名查询。只放行代理所需的最小非 DNS 系统服务。P0 测试系统 resolver、原始 DNS、DNS IPC 和通过白名单服务发起 DoH 的边界；白名单内服务仍可能承载任意应用数据，域名代理不是内容级防外带。确需本地解析的工具在默认策略下列为受限，不能静默放开 mDNSResponder。
8. Mac 下 `ports=off` 应禁止启动面向宿主的服务，而不只是停止显示端口；明确与 Linux“服务可在私网监听但不向宿主发布”的差异。

**原生模式剩余差异：** 端口许可不识别服务身份；被允许的端口可能由宿主其他服务占用，不能达到 network namespace 的隔离程度。开放开发服务互访即授予对应宿主端口访问权。不能保证多会话都使用 localhost:3000；端口冲突必须报错。若这些差异不能接受，采用 VM，不靠扫描 PID 或代理变量补出虚假的隔离承诺。

后续自动发现只能报告本会话子进程拥有的 listener，可用 libproc 或验证后的系统工具实现，需权衡 CGO。它不应自动扩大 Seatbelt 连接权限。初版不引入 CGO，仅为发现端口而新增系统集成不划算。

### 4.5 信号、终端与进程清理

- 保留 FSignal 单字节和 Exit.Signal 整数的帧结构，明确其含义均为 Linux 信号编号；Darwin 发送时映射为协议编号、接收时映射回 Darwin 编号。Linux 常用信号路径保持原义，用映射表和测试固定协议语义。
- Linux SIGUSR1=10、SIGUSR2=12、SIGWINCH=28；Darwin 分别为 30、31、28。覆盖 A→B Agent 控制、B stub→A 工具控制、A 工具退出→B stub 自发信号、MCP Result 和日志；不得把协议编号直接当成本机编号。
- 明确支持的信号集合；缺少跨平台对应的信号拒绝或记录为不支持，不按相同整数猜测。每个连接仍要求 A/B FullVersion 精确匹配，二进制按版本选择，因此本次不引入协议协商或旧 wire 格式兼容。
- 退出码保留执行端实际 code；stub 要复现 signal 时使用本地映射，日志同时标明执行端/协议信号名，避免误读 `128+signal` 在两系统上的差异。
- Mac 清理保证范围：登记由 sandbox-serve 启动的 job、进程组和可确认归属的子进程；正常退出/取消/超时先 TERM，等待最多 2 秒后 KILL，整体 5 秒内结束受管进程并清理 listener、代理连接和会话临时资源（初版验收阈值，P0 如需调整须给出测量依据）。P0 验收普通子进程、后台 job、PTY 子进程和进程组均在该期限内终止；不得因清理杀死另一连接进程。A 可信主进程异常退出时，supervisor 根据控制通道 EOF 尝试执行同一清理流程。
- macOS 无 PID namespace、Pdeathsig 和 subreaper，setsid/double-fork 后被 launchd 收养的进程，以及 supervisor 自身被强杀的情况，不承诺全部回收。对能确认归属的残留尽力清理并写日志/状态供 doctor 诊断，不凭 PID 或进程名扩大查杀范围；已有完整身份记录时校验启动时间以防 PID 复用。
- 脱离 supervisor 不解除继承的 Seatbelt 策略，不能据此宣称越出沙箱；但它除占用资源外，仍可能继续写入已授权项目、使用已许可的宿主端口。撤销代理 token 能阻止继续使用该代理，不能撤销全部既有文件/端口权限。这是有明确范围的原生生命周期限制，不因仅有 daemon 残留就阻断发布；若普通受管 job 也无法清理、会误伤另一会话或沙箱权限失效，仍阻断发布。
- 继续验证 creack/pty、x/term 的 Darwin 行为：PTY EOF、窗口缩放、管道退出码、Ctrl+C、SIGTERM、90 秒重连保留与超时退出。

### 4.6 初始化、安装和发行

- bootstrap 分 OS+角色：Mac A 检查系统 ssh/sandbox-exec，profile 依赖用已存在的 Homebrew 安装；不自动安装 Homebrew。不需要 A 上的 bwrap/sshfs/macFUSE。
- 缺 brew 时输出明确手动安装指导；系统能力检查失败时不能通过 `--skip-deps` 绕过沙箱。
- doctor 检查实际沙箱启动、项目可写/秘密不可读、代理可连/外网直连拒绝、工具链可运行、端口配置、B Linux 依赖。先复用检测器，避免 setup 与 doctor 判定不一致。
- release 产出 `waldo-{linux,darwin}-{amd64,arm64}`，共享 Version+Build。Mac 安装包下载本机 Darwin 产物和两个 Linux B 产物，不需要另一种 Darwin 架构。
- 源码跨平台开发提供统一 BUILD 的配套构建目标；仅本机 `make dev` 的内容 hash 无法与 Linux B 的不同文件 hash 相等。文档必须解释如何构建匹配的 A/B。
- install.sh 支持 sha256sum 或 `shasum -a 256`；逐个校验白名单文件，保留版本固定、全部验证后替换。去除 GNU 专属 `mv -T`，同时保留拒绝目录目标与原子更新测试，不能直接删掉选项了事。
- SHA256SUMS 覆盖全部最终产物；release.yml 上传四种二进制。签名若加入必须在生成最终 checksum 之前完成；Developer ID/公证可单列发行增强，需验证实际下载与 Gatekeeper 路径，不能用“可交叉编译”代替安装验证。

### 4.7 同一 B 的共享 Claude 与多连接

B 只有一套 Claude 安装和共享状态，macOS A、Linux A 可同时连接；每个连接启动自己的 host、capsule 和 Claude 进程。Claude 登录、配置、memory、会话历史与原生恢复列表继续共用，不按设备拆分 HOME，不新增 client_id 持久命名空间或认证材料拆分机制。

```text
macOS A ── SSH M ── host M → capsule M → Claude 进程 M → 工具请求回 Mac A
Linux A ── SSH L ── host L → capsule L → Claude 进程 L → 工具请求回 Linux A
                                          │
                               共用 B 上的 Claude 安装与状态
```

现有架构已经接近这个模型：host 按实例保存 hello、yamux、挂载和 children；runDir 随机生成，stub socket 在 runDir 内，缓存与 Paseo gateway 按连接创建。`dataDir/home` 与 adopted Agent 目录继续共享。两个 capsule 内的 HOME 路径字符串可随 A 不同，底层仍绑定同一套 B 状态。

实现与验收重点：

- **共享安装与状态：** 沿用 B 上已有的 Claude 和登录，保留现有 HOME/adopted 机制，不复制出每设备的 Claude 状态目录，也不迁移既有历史。并发读写遵循 Claude 本身的行为，Waldo 不新增会话历史数据库或凭证刷新代理。
- **工作区与工具路由：** 每个进程使用自己的 A 平台、工具链、导出路径、UID/GID、执行模式、网络策略和会话说明。每个 capsule 只挂载对应 A 的项目/skills/tmp；即使两台 A 使用相同项目路径，FS、exec、MCP、Paseo 和观测流也必须经本连接的 yamux 返回所属 A，不能按路径寻找另一个连接。
- **原生会话恢复：** 不按设备限制 Claude 的 --continue/resume 列表和选择，可在另一设备启动新连接后选择 B 上已有会话。原生历史恢复与 SSH 断线重连分开：前者恢复 Claude 对话，并使用新连接当前的工作区和工具环境；后者只重连原 host/Agent 进程及其原 A。历史中的旧路径按新 A 的实际文件系统处理，不自动连接回旧 A，也不自动同步项目文件。
- **运行时资源：** 挂载、stub socket、SFTP 内容缓存、MCP 后台 job、桥接 token、collector 和清理句柄保持连接级归属。关闭 Mac 连接只结束该连接的资源，不卸载 Linux 连接的挂载、不杀其 Agent；共享 Claude 状态不因连接退出被删除。
- **安装版本：** P1 必须把 B 的 Waldo 二进制放到 `<remote_dir>/bin/<version-key>/waldo`，version-key 使用经验证/编码的完整 Version+Build；同一 Release 的各架构共享 Build。候选程序在临时目录核对完整版本/摘要后原子发布，每个版本发布后不可变。Open/bootstrap、Run 和 reattach 都固定选择本连接所需版本，不经可被另一版本覆盖的 `bin/waldo`；重连只附着原 host，不启动替代 Agent。不同 A 版本各自使用对应的 Linux B 程序，仍共用同一 Claude 安装与 HOME，不涉及 Agent 状态隔离。
- **安装并发与保留：** 按版本发布避免全局安装锁；同版本并发发布仍需使用原子 no-replace/等价机制，已存在目标先验证再复用，失败不覆盖已发布文件。活跃和等待重连的版本必须保留，首版不自动 GC；后续以可靠的存活记录做 GC。开发构建仍应提供统一 BUILD 的 Mac/Linux 配套产物，否则候选 B 的 FullVersion 校验失败时直接报错，不能以放松匹配掩盖问题。旧 singleton 路径不自动删除；未实现版本目录的旧客户端不纳入新版混用保证，升级期间不覆盖其程序或中断旧进程。
- **网络与端口：** 两台 A 的 localhost:3000 属于各自本机，可同时使用，不在 B 上设置全局 3000 转发口。B 的 MCP/观测/Paseo listener 使用各连接的随机端口和认证。remote 模式命令共享 B 网络，若两个进程都绑定 B 的 3000，按正常端口冲突处理。
- **日志与清理：** 集中日志可继续保留，用 session/PID 区分连接；不为了多设备强制新增日志存储层。stale 清理只能回收已确认死亡连接的 runDir，避免 PID 复用和并发清理误伤。

当前失败模式已核对：普通 Open/Run 也会在版本不符时调用 Install 覆盖 `bin/waldo`，不是只有 setup。Mac Build X 覆盖 Linux Build Y 后，Y 的 reattach 经过同一 bootstrap 得到 NeedInstall 而非精确匹配 banner，90 秒内若无法恢复对应程序就会超时丢失原连接；两端后续连接可继续反复覆盖。跨平台 dev 二进制 hash 不同使该问题在开发期尤其常见。因此不能仅以“要求同 Build”替代版本目录设计。

不增加 Waldo 层按设备限制恢复或对同一 Claude native session 加锁的机制；同一历史被多个进程同时恢复的行为沿用 Claude 原生语义。本次必须保证每个进程的工具路由和退出清理仍然独立。

新增并发验收场景：

1. Mac A 和 Linux A 使用同一 SSH 用户、同一 remote_dir 同时 setup/连接，分别覆盖相同与不同 Version+Build；共用 B 上同一 Claude 安装与登录，分别运行独立 Claude PID，读改本机文件并启动各自本地 3000 服务。
2. 两台 A 使用相同逻辑项目路径和文件名，写入不同标记；SFTP/MCP/exec、skills、缓存、UID 映射与会话说明均返回本连接对应 A 的内容。
3. 一端创建对话，另一端验证 Claude 原生历史/resume；`/Users/...` 与 Linux 项目路径不同，按 cwd 分项目的 --continue 列表不同是预期结果，不要求自动合并。显式选择历史后，工具请求使用新连接的 A，旧连接文件不会自动同步或改写。同时验证共享状态中插件、hooks、MCP 的绝对路径在两种 HOME 字符串下能否运行；如改为固定 B HOME，重复验证 shared/shell-snapshots 与本机工具环境。
4. Mac 断网、重连、超时或退出时，Linux 的 Agent PID、挂载和正在运行的命令保持正常；反向也测。共享历史继续保留，触发 stale 清理不影响另一活跃连接。
5. 两端以不同 FullVersion 并发首次连接、Run/doctor 和重复 setup；随后交替断网重连，确认各自 B 版本路径/Agent PID 保持不变，不反复覆盖程序、不丢失原会话。再测同版本并发发布去重，以及发布失败、旧路径并存；全过程不重置共享 Claude 配置、登录或历史。
6. 两端同时使用 observe、Paseo、MCP 后台任务与取消命令，确认 hook/gateway/job 的路由和清理只作用于所属连接。

## 5. 分阶段交付与验收

以下为单人熟悉 Go 的粗估工程量，不是已验证工期；可行性阶段结果可能改变后续估算。

| 阶段 | 交付 | 验收条件 | 粗估 |
|---|---|---|---|
| P0：可行性验证 | Seatbelt 原型、工具兼容矩阵、清理保证范围、HOME/tmp 合同 | Node/Go/git/rg/PTY；clang/swift/xcrun 与 T/C 缓存；嵌套沙箱；共享 HOME 绝对路径；APFS 保护；认证代理/DNS；受管 job 清理及 daemon 残留边界 | 3–4 人日 |
| P1：平台与协议 | 两处编译修复、Darwin 角色入口保护、Linux 编号信号映射、helper/context/layout、B 版本目录 | 四种目标构建/vet；不同版本并发安装与原会话重连；跨 OS 信号；Linux 回归；DTO 抽取不作前置 | 3–4 人日 |
| P2：原生闭环 | Seatbelt、HOME/tmp、context/skills、代理与显式端口 | Mac A→Linux B Claude host-mcp 完成读改搜、构建测试和开发服务；不需 A FUSE | 4–6 人日 |
| P3：模式与生命周期 | host/remote、Codex/run、Paseo、observe、重连和清理、共享 Claude 多连接验证 | 三种模式按公开的 tmp/port 合同验收；Mac+Linux 同时连接同一 B 的六项并发场景通过 | 4–7 人日 |
| P4：发布 | setup/doctor、安装、CI、用户文档 | Mac 双架构安装/升级；两种 Linux B 架构安装；安全测试无静默 skip；Linux 全量回归通过 | 3–5 人日 |

计入共享 Claude 的多连接验证后，合计约 **17–26 人日**，其中 P0 是继续原生路线的决策关口；P1 在两处最小编译修复节省的范围内计入约 0.5–1 人日的版本目录改造，仍属粗估。若关键边界不成立，应在 P0 后调整范围或转 VM，避免先做完安装发布再发现隔离不成立。

测试组织：

- Ubuntu 保留当前 make test；加双 Darwin 交叉构建以尽早发现编译退化。
- macOS runner 执行单测和真实 Seatbelt 攻击测试。CI 缺少 sandbox-exec、必要依赖或拒绝规则失效时失败，不因 Skip 得到假绿。
- 跨机 E2E 不能沿用 Mac 本机启动 Linux B 的 harness。拆为 A runner 与可配置 Linux B fixture；使用受控 SSH 连接和私有测试状态。B 可来自单独 VM/测试服务器，不要求用户安装 VM。
- 在可用 CI 架构之外补真机记录；Apple Silicon/Intel A、amd64/arm64 B 的四种组合至少都有安装/握手验证，核心流程分别覆盖两种 A 架构。
- 文件语义覆盖 APFS 大小写、Unicode、symlink、原子替换、chmod、纳秒 mtime、UID/GID（Mac 常见 501/20 → B 用户）、worktree、跨平台依赖二进制不能在 B 直接执行。
- 安全测试覆盖 APFS 上 `.GIT/hooks`、`.Git/config` 及适用路径 NFC/NFD 等价别名、protected paths 新建/重命名绕过、HOME/Library/Keychain 读取、Unix socket、非白名单直连、DNS IPC/mDNSResponder、直接 DNS/IPv6 绕过、宿主端口、无认证/错 token 代理请求、跨会话代理/缓存泄漏；setsid/double-fork 和 A 异常退出按清理合同验证，不将全部 daemon 回收作为通过条件。
- 用户功能覆盖 Claude/Codex、非交互管道、退出码、link/zsh、Paseo skills/MCP、observe、热更新 allow、90 秒重连、端口冲突、setup --skip-deps、无 brew、下载损坏和旧版安装不被破坏。

## 6. 建议拆分的 PR

1. **最小平台修复**：Pdeathsig 平台 helper、Stat 转换、Darwin B 入口保护；DTO 抽取与完整角色拆包可选，保证 Linux 回归。
2. **跨系统信号与运行时布局**：helper/context/导出描述、HOME/tmp 合同、Linux 编号信号映射、B 版本目录和并发安装/重连；先在 Linux 测试新协议。
3. **Seatbelt 后端与安全回归**：文件、系统能力、进程生命周期；先提供 dev-sandbox 可检验入口。
4. **Mac 网络和真实跨端流程**：代理、端口模式、三种执行模式与 Paseo、共享 Claude 多连接验证；完成 Mac+Linux 并发接入同一 B 的验收。
5. **安装发布与文档**：bootstrap/doctor、四目标 release、Mac CI、README/SECURITY/configuration 更新。

不建议在第一步重写 SFTP、引入第三方完整 sandbox runtime、搬迁用户配置目录，或顺带实现 macOS B。上述改动对本次目标没有必要。

## 7. 证据与尚未验证项

- 已阅读仓库 A/B 启动链、沙箱、协议、文件投影、执行模式、代理/端口、安装、CI 和测试 harness。
- 已在本机确认 macOS 15.6.1、`/usr/bin/sandbox-exec` 存在、手册标记 DEPRECATED。
- 上游参考：[Anthropic macOS Seatbelt 实现](https://github.com/anthropics/sandbox-runtime/blob/main/src/sandbox/macos-sandbox-utils.ts)。这里只借鉴机制，不建议直接增加 Node 运行时依赖；引用 main 可能变化，实现前应固定参考 commit 并复核。
- 前期交叉构建证据：Linux 审查者报告在同一提交 b61a46c、Go 1.26.8、CGO_ENABLED=0 下，两种 Darwin 架构的 build/vet 仅遇到 Stat_t.Mode 与 Pdeathsig 两处错误；临时副本修复后均通过。此后在临时目录取得并校验 Go 1.26.8，实际运行结果见第 8 节。
- 方案阶段待验证清单（实际完成情况见第 8 节）：本项目 Seatbelt 策略、T/C 缓存取舍、嵌套工具兼容、共享状态绝对 HOME 路径、双架构 Mac→Linux E2E、CLT/Xcode 与各工具链、local-binding 的精确行为、完整后台清理、真实 Claude/Codex tmp 兼容性、签名/下载行为。以上都已列入阶段验收，不把开源方案存在等同于本项目已兼容。


## 8. 实现与验收结果

以下为方案实施后的实际结果，前文阶段表保留作为设计与工作量记录。

- 已实现 Darwin Seatbelt、私有 HOME/TMPDIR、上下文动态投影、只读 skill 导出与文件工具路径翻译；Linux 保留 bubblewrap。当前采用必要的平台 helper，未将完整 B 运行时或 doctor DTO 拆包。
- B 程序按完整 Version+Build 的 SHA256 目录并存；候选程序在执行版本检查后核对摘要，再通过 hard link 原子发布。所有连接/重连入口都固定版本，不改变共享 Claude HOME。
- 信号帧仍使用 Linux 编号，Darwin 双向映射；Mac 代理强制会话认证，文件/网络权限和普通后台进程清理均有原生集成测试。
- Mac 原生单元、安装、Seatbelt 回归已通过；Linux 开发机上的单元、攻击与完整回环 E2E 已通过。四种 OS/架构均可构建；macOS arm64 已运行，amd64 仅交叉构建。
- Node、Go、最小 C/Swift 程序在真实 Mac 沙箱内运行/编译通过。Xcode 的缓存与文件监听仍有警告，不据此承诺全部 Xcode、模拟器或嵌套沙箱支持。
- 真实 Mac A→Linux B 的 host/host-mcp/remote 已用有断言的探针验证；SFTP、UID/GID 映射、Bash、MCP Read/Write 正常。真实 Claude 通过 MCP 读取合成 README、识别 Darwin 并写入预期文件。
- Mac A 与 Linux A 的真实 Claude 连接同时使用同一 B 安装和登录，会话运行区间重叠，分别通过 MCP 得到 Darwin/Linux，均正常退出。另有共享状态探针确认一端可读另一端写入的 B HOME 状态；关闭 Mac 连接不会结束另一 Linux 进程。
- 真机验收发现 B 原生 Claude 的 API key、服务地址和模型配置来自登录 shell，现已在 B 端按 profile 白名单补齐加载并继续对 A 清除。未复制凭证到 Mac，也未按设备拆分 Claude 状态。
- `make build-pair` 可构建共享 BUILD 的本机程序和两个 Linux 配套程序；安装脚本支持 Mac SHA256 校验及 BSD mv；新增 macOS CI 和四目标发行构建。功能尚未发布到稳定 Release。

当前使用步骤和兼容边界以 [macOS 使用说明](macos.md) 为准。无法完全回收脱离监管 daemon、Mac 显式端口策略、B `/tmp` 与 A 私有 tmp 分开等差异仍保留，未用关闭沙箱方式绕过。
