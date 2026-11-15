package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"waldo/internal/config"
	"waldo/internal/host"
	"waldo/internal/profile"
	"waldo/internal/session"
	"waldo/internal/termfilter"
)

type remoteEntry struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	RemoteDir string `json:"remote_dir"`
	Default   bool   `json:"default"`
	Legacy    bool   `json:"legacy,omitempty"`
	Sessions  int    `json:"sessions"`
	Error     string `json:"error,omitempty"`
}

func remoteEntries(cfg *config.Config, states []session.RunState) []remoteEntry {
	defaultName := cfg.DefaultRemote
	if _, name, err := cfg.ResolveRemote(""); err == nil {
		defaultName = name
	}
	names := map[string]bool{}
	for name := range cfg.Remote {
		names[name] = true
	}
	if cfg.DefaultRemote != "" {
		names[cfg.DefaultRemote] = true
	}
	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	entries := []remoteEntry{}
	if cfg.Host != "" {
		entries = append(entries, remoteEntry{Host: cfg.Host, RemoteDir: cfg.RemoteDirOrDefault(), Default: defaultName == "", Legacy: true})
	}
	for _, name := range sorted {
		e := remoteEntry{Name: name, Host: cfg.Remote[name].Host, Default: name == defaultName}
		if err := config.ValidateRemoteName(name); err != nil {
			e.Error = err.Error()
		} else {
			resolved, _, err := cfg.ResolveRemote(name)
			if err != nil {
				e.Error = err.Error()
			} else {
				e.RemoteDir = resolved.RemoteDirOrDefault()
			}
		}
		entries = append(entries, e)
	}
	for i := range entries {
		for _, state := range states {
			if state.Remote == entries[i].Name && (state.Remote != "" || state.Host == entries[i].Host) {
				entries[i].Sessions++
			}
		}
	}
	return entries
}

func remoteLabel(e remoteEntry) string {
	if e.Legacy {
		return "(host)"
	}
	return e.Name
}

func tableText(s string) string {
	return strings.Join(strings.Fields(termfilter.Sanitize(s)), " ")
}

func remoteCmd(cfg *config.Config, args []string, out io.Writer) int {
	if len(args) == 0 {
		return remoteList(cfg, nil, out)
	}
	switch args[0] {
	case "ls", "list":
		return remoteList(cfg, args[1:], out)
	case "status":
		return remoteStatusCmd(cfg, args[1:], out)
	case "help", "--help", "-h":
		return showHelp(append([]string{"remote"}, args[1:]...), out)
	default:
		fmt.Fprintln(out, "用法: waldo remote ls [--json] | status [名称 | --all] [--timeout 10s] [--json]")
		return 2
	}
}

func managementFlags(fs *flag.FlagSet, args []string) int {
	fs.Usage = func() { showHelp(strings.Fields(fs.Name()), fs.Output()) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return -1
}

func remoteList(cfg *config.Config, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("remote ls", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "输出 JSON")
	if code := managementFlags(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(out, "用法: waldo remote ls [--json]")
		return 2
	}
	states, err := session.RunningSessions()
	if err != nil {
		fmt.Fprintf(out, "waldo: %v\n", err)
		return 1
	}
	entries := remoteEntries(cfg, states)
	if *jsonOutput {
		return writeJSON(out, entries)
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "未配置远端。运行 waldo setup <user@host>，或在 config.toml 中添加 [remote.<名称>]。")
		return 0
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDEFAULT\tHOST\tSESSIONS\tCONFIG")
	for _, e := range entries {
		def, status := "", "ok"
		if e.Default {
			def = "*"
		}
		if e.Error != "" {
			status = e.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", tableText(remoteLabel(e)), def, tableText(e.Host), e.Sessions, tableText(status))
	}
	w.Flush()
	return 0
}

type remoteStatus struct {
	remoteEntry
	Reachable bool               `json:"reachable"`
	State     string             `json:"state"`
	Version   string             `json:"waldo_version,omitempty"`
	Duration  int64              `json:"duration_ms"`
	Report    *host.DoctorReport `json:"report,omitempty"`
}

func probeRemote(cfg *config.Config, entry remoteEntry, timeout time.Duration) remoteStatus {
	r := remoteStatus{remoteEntry: entry, State: "invalid-config"}
	if entry.Error != "" {
		return r
	}
	source := *cfg
	if entry.Legacy {
		source.DefaultRemote = ""
	}
	selected, _, err := source.ResolveRemote(entry.Name)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	var req host.DoctorReq
	profiles := selected.Profiles()
	for _, name := range profile.Names(profiles) {
		req.Profiles = append(req.Profiles, profiles[name].Wire())
	}
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	result, err := session.SSHTransport(selected, selected.Host).Probe(ctx, b)
	r.Duration = time.Since(started).Milliseconds()
	if err != nil {
		r.State, r.Error = "unreachable", err.Error()
		return r
	}
	r.Reachable, r.Version = true, result.Version
	if !result.Installed {
		r.State = "not-installed"
		return r
	}
	if !result.Compatible {
		r.State = "version-mismatch"
		return r
	}
	var report host.DoctorReport
	if json.Unmarshal(result.Report, &report) != nil || report.Version == "" {
		r.State, r.Error = "invalid-report", "无法解析 B 的状态报告"
		return r
	}
	r.Report, r.State = &report, "ready"
	for _, check := range report.Checks {
		if !check.OK {
			if !check.Warn {
				r.State = "not-ready"
				break
			}
			r.State = "warning"
		}
	}
	return r
}

func remoteStatusCmd(cfg *config.Config, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("remote status", flag.ContinueOnError)
	all := fs.Bool("all", false, "检查所有配置的远端")
	jsonOutput := fs.Bool("json", false, "输出 JSON 数组")
	timeout := fs.Duration("timeout", 10*time.Second, "单台机器完整检查的超时（100ms–1m）")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if code := managementFlags(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() == 1 && name == "" {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(out, "用法: waldo remote status [名称 | --all] [--timeout 10s] [--json]")
		return 2
	}
	if *all && name != "" || *timeout < 100*time.Millisecond || *timeout > time.Minute {
		fmt.Fprintln(out, "waldo: 名称不能与 --all 同用，timeout 应在 100ms–1m 之间")
		return 2
	}
	states, err := session.RunningSessions()
	if err != nil {
		fmt.Fprintf(out, "waldo: %v\n", err)
		return 1
	}
	entries := remoteEntries(cfg, states)
	if !*all {
		selected, resolvedName, err := cfg.ResolveRemote(name)
		if err != nil {
			fmt.Fprintf(out, "waldo: %v\n", err)
			return 2
		}
		filtered := []remoteEntry{}
		for _, e := range entries {
			if e.Name == resolvedName && e.Host == selected.Host {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "waldo: 未配置远端")
		return 2
	}
	results := make([]remoteStatus, len(entries))
	// Bound concurrency so --all doesn't serialize every timeout or create an
	// unbounded number of SSH processes. Output remains in configuration order.
	workers := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() {
			workers <- struct{}{}
			defer func() { <-workers }()
			results[i] = probeRemote(cfg, e, *timeout)
		})
	}
	wg.Wait()
	code := 0
	for _, r := range results {
		if r.State != "ready" && r.State != "warning" {
			code = 1
		}
	}
	if *jsonOutput {
		if writeJSON(out, results) != 0 {
			return 1
		}
		return code
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tSSH\tWALDO\tSESSIONS\tSTATE\tCHECK_MS")
	for _, r := range results {
		ssh, version := "failed", r.Version
		if r.State == "invalid-config" {
			ssh = "-"
		}
		if r.Reachable {
			ssh = "ok"
		}
		if version == "" {
			version = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\n", tableText(remoteLabel(r.remoteEntry)), tableText(r.Host), ssh, tableText(version), r.Sessions, r.State, r.Duration)
	}
	w.Flush()
	for _, r := range results {
		if r.Error != "" {
			fmt.Fprintf(out, "  %s: %s\n", tableText(remoteLabel(r.remoteEntry)), tableText(r.Error))
		}
		if r.Report != nil {
			for _, check := range r.Report.Checks {
				if !check.OK {
					fmt.Fprintf(out, "  %s: %s %s\n", tableText(remoteLabel(r.remoteEntry)), tableText(check.Name), tableText(check.Detail))
				}
			}
		}
	}
	return code
}

func writeJSON(out io.Writer, value any) int {
	if err := json.NewEncoder(out).Encode(value); err != nil {
		return 1
	}
	return 0
}
