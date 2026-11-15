package session

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"waldo/internal/config"
	"waldo/internal/proto"
)

// contextGuide describes the actual session policy. It contains no environment
// values, credentials, or contents read from the user's home directory.
func contextGuide(p *config.Policy, host, agent, cwd string, mounts []string) []byte {
	var b strings.Builder
	fmt.Fprint(&b, "# waldo session environment\n\n")
	fmt.Fprintf(&b, "Agent: %q. Server B: %q. Host A: %s/%s.\n\n", agent, host, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Project root: %q. Initial working directory: %q.\n\n", p.Project, cwd)
	fmt.Fprint(&b, "## Where operations happen\n\n")
	if p.ExecMode == proto.ExecHostMCP {
		fmt.Fprint(&b, `当前使用 host-mcp 命令模式：

- 内置 Bash、Read、Grep、Glob、Edit、Write、NotebookEdit 工具已禁用。使用同名 MCP 工具，完整名称如 mcp__waldo__Read、mcp__waldo__Bash；参数沿用原工具，额外支持 location: auto（默认）、host（A）、agent（B）。
- 文件工具 auto 按挂载来源分发：项目/共享路径走 A，B 的 HOME、Claude memory、会话记录和 /tmp 留在 B。项目本身位于 /tmp 下时，该项目子目录仍走 A。文件不存在即报错，不会换到另一端查找。
- Read/Edit/Write 使用绝对 file_path；Grep/Glob 使用 pattern 和可选 path（默认项目工作目录）。搜索应限定在单一来源的目录，跨 A/B 的搜索需拆开。
- 例如：Read {"file_path":"/tmp/result.txt"} 读取 B；加 "location":"host" 读取 A 的同名文件。location=agent 不能绕过 A 的投影访问 B 上同名项目目录。
- 编辑/覆盖前先 Read，版本由 MCP 内部记录；冲突后重新读取。新文件直接 Write。NotebookEdit 使用 notebook_path。图片通过 Read 返回；PDF pages 目前返回文字，需要所选端安装 pdfinfo/pdftotext。
- Bash 默认在 A 沙箱运行，需要 B 环境时明确 location=agent。依赖、localhost、临时文件属于所选端。每次调用独立，cd 和环境变量不会跨调用保留；在 command 中用 cd 切换工作目录。
- Bash 使用 timeout（毫秒），支持 run_in_background；后台任务用同一 MCP 的 TaskOutput/TaskStop 查询和停止。无自动完成通知，会话结束即取消。dangerouslyDisableSandbox=true 不受支持。
- 项目挂载仍供项目说明和 skills 自动加载使用；A 上的写入修改真实工作区。B 保留原生 shell，仅启动时 PATH 中的 Git 探测转发 A；普通任务请使用 MCP。两端执行都受各自现有沙箱权限约束。
`)
	} else if p.ExecMode == proto.ExecRemote {
		fmt.Fprint(&b, `当前使用远端命令模式：

- Agent 和内置 Bash 均在远端 B 的沙箱中运行。普通搜索、文本处理和不依赖本机环境的脚本，优先使用内置 Bash。
- 项目目录投影自本机 A。缓存命中的读取在 B 完成，未命中的读取仍需访问 A；项目文件的写入也会同步到 A，修改真实工作区。
- 命令需要本机依赖、虚拟环境、系统工具、构建测试环境或本机服务时，使用 mcp__waldo__Bash。例如：{"command":"cd /absolute/project/path && python -m pytest"}。
- A、B 的工具、依赖、HOME 和进程环境不同。不要假定本机的虚拟环境或二进制能在 B 运行；B 缺少依赖时，可以改用 Bash。
- 内置 Bash 中的 localhost 指 B；访问 A 的沙箱服务应使用 Bash。
- 内置 Bash 使用 B 的网络；A 的网络白名单和开发端口转发适用于 Bash。Bash 始终受现有本机沙箱约束。
`)
	} else {
		fmt.Fprint(&b, `- The agent process, login state and model API connections are on B.
- Project file reads and writes through the projection operate on A's sandbox.
- Commands launched through PATH tools (shell, git, rg, language tools) are forwarded to A's sandbox. Use these tools for builds, tests, dependency inspection and Git operations.
- A and B use the same path strings but separate HOME contents. Do not infer A's OS, architecture or installed tools from the agent process on B.
- A native executable started directly from a projected path or an agent package can run on B or fail there. Launch project programs through the forwarded command tools.
`)
	}
	fmt.Fprint(&b, `
## Files and permissions

- The project is writable. Changes affect the user's working tree on A.
- System and exposed toolchain paths are read-only. The sandbox HOME is separate from the host HOME.
- Existing protected paths (Git hooks/config, .envrc, editor configuration) are mounted read-only. Respect permission errors; do not recreate paths to bypass them.
- This version does not hide .git, node_modules or other dependency directories from the file projection. This guide does not grant permissions or enforce policy.
- Startup references in instruction files are read-only session views. Their original project files are preserved. Project instruction edits made through command tools take effect in the next session.
`)
	if p.ContentCacheTTL > 0 {
		fmt.Fprintf(&b, "- A bounded, session-local SFTP content cache is configured on B (idle retention %s). Every read-only open validates that file on A using nanosecond modification time, size and file identity before reusing cached bytes. Only files actually read are cached; no change notifications, background refresh or whole-project invalidations are used. Uncached metadata and directory listings are fetched from A. Writes still go to A.\n", p.ContentCacheTTL)
		fmt.Fprintln(&b, "- This is timestamp-based, weak consistency, not content-hash verification. Preserving the exact timestamp and size can hide a change. Reopen after changes; an already open cached handle is a snapshot. A file changed while being read is not promised to be an atomic snapshot. A connection failure does not authorize stale opens: validation waits for recovery or fails.")
	} else if p.MetadataCacheTTL > 0 && p.ExecMode != proto.ExecHostMCP {
		fmt.Fprintf(&b, "- Project path lookups and file attributes on B use a %s cache. Changes made on A may take this long, plus an in-flight request, to become visible. Directory listings are not cached. Reopen files after external changes; do not assume an already open handle is refreshed.\n", p.MetadataCacheTTL)
	} else if p.MetadataCacheTTL == 0 {
		fmt.Fprintln(&b, "- Project metadata caching is disabled for this session.")
	}
	if p.ExecMode == proto.ExecHostMCP {
		if p.MetadataCacheTTL > 0 {
			ttl := min(p.MetadataCacheTTL, 2*time.Second)
			fmt.Fprintf(&b, "- Automatic project/readonly-skill discovery uses a short metadata cache (%s for attributes/path entries, %s for missing paths). Changes may be visible after that delay plus an in-flight request. Directory listings are not cached. MCP file tools on A bypass these caches and operate on current host files.\n", ttl, min(ttl, time.Second))
		}
		fmt.Fprintln(&b, "- /tmp is private to B, except explicitly projected subdirectories. location=host accesses A tmp. Shared runtime paths and this guide bypass the project cache.")
	} else {
		fmt.Fprintln(&b, "- Shared runtime paths, including /tmp and this guide, bypass the project cache. Writes remain synchronous.")
	}
	fmt.Fprintln(&b, "Projected paths:")
	for _, m := range mounts {
		fmt.Fprintf(&b, "- %q\n", m)
	}
	fmt.Fprintln(&b, "\nAdditional read-only paths:")
	if len(p.ExposeRO) == 0 {
		fmt.Fprintln(&b, "- None configured beyond automatically detected toolchains.")
	}
	for _, m := range p.ExposeRO {
		fmt.Fprintf(&b, "- %q\n", m)
	}
	fmt.Fprint(&b, "\n## Network\n\n")
	fmt.Fprintln(&b, "Commands on A use a domain allow-list proxy. Direct external connections are isolated. The agent's model API traffic stays on B.")
	allow := append([]string{}, p.Allow...)
	if extra, err := config.LoadAllowed(); err == nil {
		allow = append(allow, extra["*"]...)
		allow = append(allow, extra[p.Project]...)
	}
	fmt.Fprintf(&b, "\nAllowed destinations at startup: %q.\n", allow)
	fmt.Fprintf(&b, "Host services available on sandbox localhost: %v.\n", p.HostPorts)
	fmt.Fprintf(&b, "Forwarded development ports: mode=%q, explicit ports=%v. Auto mode excludes reserved service ports.\n", p.Ports.Mode, p.Ports.List)
	fmt.Fprintf(&b, "Proxy connections to private/local addresses allowed: %t.\n", p.AllowPrivate)
	fmt.Fprintln(&b, "\nThe user can update network permissions on A with `waldo allow <domain>`. Ask the user when a task needs a policy change; do not edit policy from inside the sandbox.")
	fmt.Fprintf(&b, "\n## Reading this guide\n\nRead %s with either a file tool or Bash such as `cat %s`. This path is shared by A and B and exists for this session.\n", proto.ContextFile, proto.ContextFile)
	return []byte(b.String())
}
