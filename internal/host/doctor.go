package host

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// DoctorReq is read from stdin by `waldo host --doctor`.
type DoctorReq struct {
	Profiles []proto.Profile `json:"profiles"`
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // not fatal
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type DoctorReport struct {
	Version    string  `json:"version"`
	OS         string  `json:"os"`
	Arch       string  `json:"arch"`
	Distro     string  `json:"distro"`
	TimeUnixMs int64   `json:"time_unix_ms"`
	Checks     []Check `json:"checks"`
}

// Distro returns the os-release ID (and ID_LIKE) for install hints.
func Distro() string {
	b, _ := os.ReadFile("/etc/os-release")
	var id, like string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "ID="); ok {
			id = strings.Trim(v, `"`)
		}
		if v, ok := strings.CutPrefix(l, "ID_LIKE="); ok {
			like = strings.Trim(v, `"`)
		}
	}
	return strings.TrimSpace(id + " " + like)
}

// InstallHint returns an install command for a package on the given distro.
func InstallHint(distro, pkg string) string {
	names := map[string]map[string]string{
		"apt":    {"bwrap": "bubblewrap", "sshfs": "sshfs", "rg": "ripgrep", "git": "git", "ssh": "openssh-client", "bash": "bash"},
		"dnf":    {"bwrap": "bubblewrap", "sshfs": "fuse-sshfs", "rg": "ripgrep", "git": "git", "ssh": "openssh-clients", "bash": "bash"},
		"pacman": {"bwrap": "bubblewrap", "sshfs": "sshfs", "rg": "ripgrep", "git": "git", "ssh": "openssh", "bash": "bash"},
		"apk":    {"bwrap": "bubblewrap", "sshfs": "sshfs", "rg": "ripgrep", "git": "git", "ssh": "openssh-client", "bash": "bash"},
	}
	pm := ""
	switch {
	case strings.Contains(distro, "debian") || strings.Contains(distro, "ubuntu"):
		pm = "apt"
	case strings.Contains(distro, "fedora") || strings.Contains(distro, "rhel") || strings.Contains(distro, "centos"):
		pm = "dnf"
	case strings.Contains(distro, "arch"):
		pm = "pacman"
	case strings.Contains(distro, "alpine"):
		pm = "apk"
	}
	n, ok := names[pm][pkg]
	if !ok {
		return ""
	}
	switch pm {
	case "apt":
		return "sudo apt install " + n
	case "dnf":
		return "sudo dnf install " + n
	case "pacman":
		return "sudo pacman -S " + n
	case "apk":
		return "sudo apk add " + n
	}
	return ""
}

func doctorMain(dataDir string) int {
	var req DoctorReq
	json.NewDecoder(os.Stdin).Decode(&req)
	d := Distro()
	r := DoctorReport{Version: proto.FullVersion(), OS: runtime.GOOS, Arch: runtime.GOARCH, Distro: d}
	add := func(c Check) { r.Checks = append(r.Checks, c) }
	for _, bin := range []string{"bwrap", "sshfs", "fusermount3"} {
		p, err := exec.LookPath(bin)
		c := Check{Name: bin, OK: err == nil, Detail: p}
		if err != nil {
			pkg := bin
			if bin == "fusermount3" {
				pkg = "sshfs"
			}
			c.Fix = InstallHint(d, pkg)
		}
		add(c)
	}
	if out, err := exec.Command("bwrap", "--unshare-user", "--unshare-pid", "--ro-bind", "/", "/", "--proc", "/proc", "true").CombinedOutput(); err != nil {
		add(Check{Name: "bwrap 可以创建用户命名空间", OK: false, Detail: strings.TrimSpace(string(out)),
			Fix: "检查 kernel.unprivileged_userns_clone / AppArmor 的 userns 限制"})
	} else {
		add(Check{Name: "bwrap 可以创建用户命名空间", OK: true})
	}
	_, err := os.Stat("/dev/fuse")
	add(Check{Name: "/dev/fuse", OK: err == nil})
	for _, p := range req.Profiles {
		bin, err := lookBin(p.Bin)
		if err != nil {
			add(Check{Name: "agent " + p.Name, Warn: true, Detail: "未安装（需要时在 B 上安装即可）"})
			continue
		}
		add(Check{Name: "agent " + p.Name, OK: true, Detail: bin})
		home, _ := os.UserHomeDir()
		logged := false
		for _, s := range p.State {
			rel, ok := strings.CutPrefix(s, "~/")
			if !ok {
				continue
			}
			if util.Exists(filepath.Join(dataDir, "home", rel)) {
				logged = true
			}
			for _, a := range adopted(dataDir) {
				if a == s && util.Exists(filepath.Join(home, rel)) {
					logged = true
				}
			}
		}
		c := Check{Name: "agent " + p.Name + " 已有状态（登录）", OK: logged, Warn: !logged}
		if !logged {
			c.Detail = "首次运行时会提示登录；或用 waldo setup --adopt-login 复用 B 上已有的登录"
		}
		add(c)
	}
	r.TimeUnixMs = time.Now().UnixMilli()
	json.NewEncoder(os.Stdout).Encode(r)
	return 0
}
