package wsl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"waldo/internal/util"
)

// CheckProject rejects Windows drive mounts in a writable WSL2 project.
// The Linux read-only binds used by Waldo cannot enforce the same file
// protection on DrvFs, where different names can resolve to the same file.
func CheckProject(project string) error {
	if !IsWSL2() {
		return nil
	}
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		return fmt.Errorf("检查 WSL2 项目目录: %w", err)
	}
	mounts, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("检查 WSL2 项目挂载: %w", err)
	}
	defer mounts.Close()
	mount, err := windowsProjectMount(mounts, real)
	if err != nil {
		return fmt.Errorf("检查 WSL2 项目挂载: %w", err)
	}
	if mount != "" {
		return fmt.Errorf("WSL2 项目目录 %q 涉及 Windows 挂载 %q，无法保证受保护文件只读。请把项目放到 WSL 文件系统（如 ~/projects），Windows 可通过 \\wsl.localhost 打开", project, mount)
	}
	return nil
}

func windowsProjectMount(r io.Reader, project string) (string, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 1<<20)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var backing, windows, nested string
	for s.Scan() {
		mount, fs, ok := strings.Cut(s.Text(), " - ")
		if !ok {
			continue
		}
		left, right := strings.Fields(mount), strings.Fields(fs)
		if len(left) < 6 || len(right) < 3 {
			continue
		}
		at := unescape.Replace(left[4])
		// On WSL2, virtiofs can also be the transport for Windows drives.
		fromWindows := right[0] == "drvfs" || right[0] == "virtiofs" ||
			(right[0] == "9p" && strings.Contains(","+right[2], ",aname=drvfs;"))
		if at != project && fromWindows && util.Within(at, project) {
			nested = at
		}
		if util.Within(project, at) && len(at) >= len(backing) {
			backing, windows = at, ""
			if fromWindows {
				windows = at
			}
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if backing == "" {
		return "", fmt.Errorf("没有找到 %q 所在的挂载", project)
	}
	if nested != "" {
		return nested, nil
	}
	return windows, nil
}
