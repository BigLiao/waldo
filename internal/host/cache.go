package host

import (
	"path/filepath"
	"strconv"
	"time"

	"waldo/internal/proto"
)

func projectMetadataCacheable(project, mount string) bool {
	root := filepath.Clean(mount)
	return root == filepath.Clean(project) && root != "/tmp" && root != proto.ContextDir
}

// Legacy modes only cache project metadata. Keeping sshfs's own
// directory cache off avoids stacking its integer-second TTL with the kernel
// TTL. Runtime paths must stay fresh for cwd markers and command output files.
// Content-cache and synchronous-write behavior are deliberately unchanged.
func sshfsCacheOptions(ttl time.Duration, cacheable bool) []string {
	if !cacheable {
		ttl = 0
	}
	return sshfsMetadataOptions(ttl, 0)
}

func sshfsMetadataOptions(ttl, negativeTTL time.Duration) []string {
	seconds := strconv.FormatInt(int64(ttl/time.Second), 10)
	negative := strconv.FormatInt(int64(negativeTTL/time.Second), 10)
	return []string{"dir_cache=no", "attr_timeout=" + seconds, "entry_timeout=" + seconds, "negative_timeout=" + negative}
}

// Native file-tool modes keep metadata fresh before the proxy's open check.
func sshfsProxyCacheOptions() []string {
	return append(sshfsCacheOptions(0, false), "direct_io")
}

// host-mcp file tools bypass SSHFS. Its project and read-only skill mounts
// remain for Claude's automatic discovery, where short metadata staleness is
// acceptable. Shared writable runtime paths must still be immediately visible.
func sshfsMountCacheOptions(hello proto.Hello, mount string, contentProxy bool) (options []string, ttl, negativeTTL time.Duration) {
	project := projectMetadataCacheable(hello.Project, mount)
	configured := time.Duration(hello.MetadataCacheTTLSeconds) * time.Second
	if project && !contentProxy {
		ttl = configured
	}
	if hello.ExecMode == proto.ExecHostMCP && configured > 0 {
		cacheable := project
		root := filepath.Clean(mount)
		if root != "/tmp" && root != proto.ContextDir && (hello.Facts.ContextFile == "" || root != filepath.Dir(hello.Facts.ContextFile)) {
			for _, path := range hello.ReadOnlyMounts {
				cacheable = cacheable || root == filepath.Clean(path)
			}
		}
		if cacheable {
			ttl = min(configured, 2*time.Second)
			negativeTTL = min(ttl, time.Second)
		}
	}
	options = sshfsMetadataOptions(ttl, negativeTTL)
	if contentProxy {
		if ttl == 0 {
			return sshfsProxyCacheOptions(), 0, 0
		}
		// Keep file bytes out of the kernel page cache; each actual open
		// still goes through the existing per-file version validation.
		options = append(options, "direct_io")
	}
	return
}
