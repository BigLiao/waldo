package host

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// sshfs translates the A user's IDs to the B user before the capsule's user
// namespace sees file attributes. File mappings also translate chown requests
// in the opposite direction; other owners remain unchanged (nomap=ignore).
type idMapFiles struct {
	uid string
	gid string
}

func prepareIDMaps(dir string, remoteUID, remoteGID int) (idMapFiles, error) {
	username, err := localIDName("用户", "-un", os.Getuid(), func(id string) (string, error) {
		u, err := user.LookupId(id)
		if err != nil {
			return "", err
		}
		return u.Username, nil
	})
	if err != nil {
		return idMapFiles{}, err
	}
	group, err := localIDName("主组", "-gn", os.Getgid(), func(id string) (string, error) {
		g, err := user.LookupGroupId(id)
		if err != nil {
			return "", err
		}
		return g.Name, nil
	})
	if err != nil {
		return idMapFiles{}, err
	}
	return writeIDMaps(dir, remoteUID, remoteGID, username, group)
}

// Pure-Go os/user may not see NSS users. id uses the system's NSS lookup,
// matching the username/groupname resolution performed by sshfs itself.
func localIDName(kind, idArg string, id int, lookup func(string) (string, error)) (string, error) {
	name, lookupErr := lookup(strconv.Itoa(id))
	if lookupErr == nil {
		if err := validateIDMapName(name); err != nil {
			return "", fmt.Errorf("B 上的%s名称无法用于 sshfs ID 映射: %w", kind, err)
		}
		return name, nil
	}
	out, err := exec.Command("id", idArg).Output()
	if err != nil {
		return "", fmt.Errorf("无法解析 B 上%s ID %d 的名称（sshfs ID 映射需要名称；os/user: %v；id: %w）", kind, id, lookupErr, err)
	}
	name = strings.TrimSuffix(string(out), "\n")
	if err := validateIDMapName(name); err != nil {
		return "", fmt.Errorf("B 上的%s名称无法用于 sshfs ID 映射: %w", kind, err)
	}
	// With -n, id prints the numeric ID if it cannot find a name. Such a
	// fallback is not a usable sshfs mapping, which resolves names via NSS.
	if name == strconv.Itoa(id) {
		return "", fmt.Errorf("B 上的%s ID %d 没有可解析的名称，无法创建 sshfs ID 映射", kind, id)
	}
	return name, nil
}

func validateIDMapName(name string) error {
	if name == "" || strings.ContainsRune(name, ':') || strings.IndexFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("无效的名称 %q", name)
	}
	return nil
}

func idMapLine(name string, id int) (string, error) {
	if err := validateIDMapName(name); err != nil {
		return "", err
	}
	if id < 0 || uint64(id) >= 1<<32-1 {
		return "", fmt.Errorf("无效的 A 端 UID/GID %d", id)
	}
	return name + ":" + strconv.Itoa(id) + "\n", nil
}

func writeIDMaps(dir string, remoteUID, remoteGID int, username, group string) (idMapFiles, error) {
	uidLine, err := idMapLine(username, remoteUID)
	if err != nil {
		return idMapFiles{}, fmt.Errorf("创建 sshfs UID 映射: %w", err)
	}
	gidLine, err := idMapLine(group, remoteGID)
	if err != nil {
		return idMapFiles{}, fmt.Errorf("创建 sshfs GID 映射: %w", err)
	}
	files := idMapFiles{uid: filepath.Join(dir, "uidmap"), gid: filepath.Join(dir, "gidmap")}
	for i, mapping := range []struct{ path, line string }{{files.uid, uidLine}, {files.gid, gidLine}} {
		if err := writeIDMap(mapping.path, mapping.line); err != nil {
			if i > 0 {
				os.Remove(files.uid)
			}
			return idMapFiles{}, fmt.Errorf("写入 sshfs ID 映射 %s: %w", mapping.path, err)
		}
	}
	return files, nil
}

func writeIDMap(path, line string) error {
	// The per-session directory is private. Exclusive creation additionally
	// prevents following an unexpected symlink or replacing an existing map.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func (m idMapFiles) options() []string {
	return []string{"idmap=file", "nomap=ignore", "uidfile=" + escapeFuseValue(m.uid), "gidfile=" + escapeFuseValue(m.gid)}
}

// Values are passed as argv (without a shell), but libfuse still parses each
// -o argument as a comma-separated list and interprets backslash escapes.
func escapeFuseValue(value string) string {
	return strings.NewReplacer("\\", "\\\\", ",", "\\,").Replace(value)
}
