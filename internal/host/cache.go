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

// Only the project uses a short kernel metadata cache. Keeping sshfs's own
// directory cache off avoids stacking its integer-second TTL with the kernel
// TTL. Runtime paths must stay fresh for cwd markers and command output files.
// Content-cache and synchronous-write behavior are deliberately unchanged.
func sshfsCacheOptions(ttl time.Duration, cacheable bool) []string {
	seconds := "0"
	if cacheable && ttl > 0 {
		seconds = strconv.FormatInt(int64(ttl/time.Second), 10)
	}
	return []string{"dir_cache=no", "attr_timeout=" + seconds, "entry_timeout=" + seconds, "negative_timeout=0"}
}
