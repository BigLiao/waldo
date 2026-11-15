package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"waldo/internal/config"
	"waldo/internal/session"
	"waldo/internal/termfilter"
)

func sessionsCmd(args []string, out io.Writer, portsOnly bool) int {
	command := "sessions"
	if portsOnly {
		command = "ports"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	remote := fs.String("remote", "", "只看指定远端的本机活跃会话")
	jsonOutput := fs.Bool("json", false, "输出 JSON")
	if code := managementFlags(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(out, "用法: waldo %s [--remote <名称>] [--json]\n", command)
		return 2
	}
	if flagSet(fs, "remote") {
		if err := config.ValidateRemoteName(*remote); err != nil {
			fmt.Fprintf(out, "waldo: %v\n", err)
			return 2
		}
	}
	states, err := session.RunningSessions()
	if err != nil {
		fmt.Fprintf(out, "waldo: %v\n", err)
		return 1
	}
	filtered := []session.RunState{}
	for _, state := range states {
		if *remote == "" || state.Remote == *remote {
			filtered = append(filtered, state)
		}
	}
	if *jsonOutput {
		return writeJSON(out, filtered)
	}
	if len(filtered) == 0 {
		fmt.Fprintln(out, "没有运行中的会话")
		return 0
	}
	if portsOnly {
		// Keep the original plain-text ports format for existing single-host
		// users; structured consumers can opt into --json.
		for _, state := range filtered {
			ports := []string{}
			for _, port := range state.Ports {
				ports = append(ports, fmt.Sprintf("127.0.0.1:%d", port))
			}
			if len(ports) == 0 {
				ports = append(ports, "（无）")
			}
			machine := ""
			if state.Remote != "" {
				machine = "  [remote=" + tableText(state.Remote) + " host=" + tableText(state.Host) + "]"
			}
			fmt.Fprintf(out, "%s  %-8s %s%s\n    %s\n", termfilter.Sanitize(state.Session), termfilter.Sanitize(state.Agent), termfilter.Sanitize(state.Project), machine, strings.Join(ports, "  "))
		}
		return 0
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tREMOTE\tHOST\tSSH\tAGENT\tPID\tPROJECT")
	for _, state := range filtered {
		remote := state.Remote
		if remote == "" {
			remote = "(host)"
		}
		connection := state.Connection
		if connection == "" {
			connection = "unknown"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", tableText(state.Session), tableText(remote), tableText(state.Host), tableText(connection), tableText(state.Agent), state.PID, tableText(state.Project))
	}
	w.Flush()
	return 0
}
