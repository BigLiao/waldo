package session

import (
	"fmt"
	"runtime"
	"strings"

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
	if p.ExecMode == proto.ExecRemote {
		fmt.Fprint(&b, `当前使用远端命令模式：

- Agent 和内置 Bash 均在远端 B 的沙箱中运行。普通搜索、文本处理和不依赖本机环境的脚本，优先使用内置 Bash。
- 项目目录投影自本机 A。缓存命中的读取在 B 完成，未命中的读取仍需访问 A；项目文件的写入也会同步到 A，修改真实工作区。
- 命令需要本机依赖、虚拟环境、系统工具、构建测试环境或本机服务时，使用 mcp__waldo_host__host_bash。例如：{"command":"python -m pytest", "cwd":"/absolute/project/path"}。
- A、B 的工具、依赖、HOME 和进程环境不同。不要假定本机的虚拟环境或二进制能在 B 运行；B 缺少依赖时，可以改用 host_bash。
- 内置 Bash 中的 localhost 指 B；访问 A 的沙箱服务应使用 host_bash。
- 内置 Bash 使用 B 的网络；A 的网络白名单和开发端口转发适用于 host_bash。host_bash 始终受现有本机沙箱约束。
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
		fmt.Fprintf(&b, "- A bounded, session-local SFTP cache is configured on B (content maximum age %s, metadata maximum age %s). It is used only when the proxy and project-change monitoring are working; otherwise reads pass through to A. Writes still go to A. Project filesystem changes invalidate the cache; external edits are observed asynchronously. Forwarded A command completion waits for invalidation when a cache subscriber is present. Reopen files after changes; an already open handle is not a live view.\n", p.ContentCacheTTL, p.MetadataCacheTTL)
		fmt.Fprintln(&b, "- Monitoring failure, notification loss or disconnection disables cache hits. Notifications cannot guarantee instant visibility of arbitrary external writes; mmap and remote changes on network filesystems may require bypassing the cache.")
	} else if p.MetadataCacheTTL > 0 {
		fmt.Fprintf(&b, "- Project path lookups and file attributes on B use a %s cache. Changes made on A may take this long, plus an in-flight request, to become visible. Directory listings are not cached. Reopen files after external changes; do not assume an already open handle is refreshed.\n", p.MetadataCacheTTL)
	} else {
		fmt.Fprintln(&b, "- Project metadata caching is disabled for this session.")
	}
	fmt.Fprintln(&b, "- Shared runtime paths, including /tmp and this guide, bypass the project cache. Writes remain synchronous.")
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
