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
	fmt.Fprintf(&b, "Agent: %q. Server B: %q. Commands run on A (%s/%s).\n\n", agent, host, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "Project root: %q. Initial working directory: %q.\n\n", p.Project, cwd)
	fmt.Fprint(&b, `## Where operations happen

- The agent process, login state and model API connections are on B.
- Project file reads and writes through the projection operate on A's sandbox.
- Commands launched through PATH tools (shell, git, rg, language tools) are forwarded to A's sandbox. Use these tools for builds, tests, dependency inspection and Git operations.
- A and B use the same path strings but separate HOME contents. Do not infer A's OS, architecture or installed tools from the agent process on B.
- A native executable started directly from a projected path or an agent package can run on B or fail there. Launch project programs through the forwarded command tools.

## Files and permissions

- The project is writable. Changes affect the user's working tree on A.
- System and exposed toolchain paths are read-only. The sandbox HOME is separate from the host HOME.
- Existing protected paths (Git hooks/config, .envrc, editor configuration) are mounted read-only. Respect permission errors; do not recreate paths to bypass them.
- This version does not hide .git, node_modules or other dependency directories from the file projection. This guide does not grant permissions or enforce policy.
- Startup references in instruction files are read-only session views. Their original project files are preserved. Project instruction edits made through command tools take effect in the next session.
`)
	if p.ContentCacheTTL > 0 {
		fmt.Fprintf(&b, "- A bounded, session-local SFTP cache is configured on B (content maximum age %s, metadata maximum age %s). It is used only when the proxy and project-change monitoring are working; otherwise reads pass through to A. Writes still go to A. Project filesystem changes invalidate the cache; external edits are observed asynchronously. Command completion waits for invalidation when a cache subscriber is present. Reopen files after changes; an already open handle is not a live view.\n", p.ContentCacheTTL, p.MetadataCacheTTL)
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
	fmt.Fprintf(&b, "\n## Reading this guide\n\nRead %s with either a file tool or a forwarded command such as `cat %s`. This path is shared by A and B and exists for this session.\n", proto.ContextFile, proto.ContextFile)
	return []byte(b.String())
}
