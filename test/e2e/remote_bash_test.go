package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteBashAndHostMCPUseDifferentMachinesAndSameFiles(t *testing.T) {
	e, agent := setup(t)
	e.WriteConfig("[execution]\nmode='remote'\n[sandbox]\npass_env=['E2E_HOST_ONLY']\n[agent.claude]\nbin=" + strconvQuote(agent) + "\nstate=[]\nshared=[]\n")
	cmd := e.Cmd("claude", "remote-bash") // mode is inherited, no launch flag
	cmd.Env = append(cmd.Env, "E2E_HOST_ONLY=host-environment")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remote mode: %v\n%s", err, out)
	}
	r := parseResults(string(out))
	if r["remote_bash_netns"] != hostNetns(t) || r["remote_bash_netns"] != r["remote_agent_netns"] || r["remote_bash_code"] != "0" {
		t.Fatalf("built-in Bash left B: %v", r)
	}
	if r["host_bash_netns"] == "" || r["host_bash_netns"] == r["remote_agent_netns"] || r["host_bash_exit"] != "7" || r["host_bash_stderr"] != "stderr" || r["host_bash_env"] != "true" || r["host_bash_iserror"] != "true" {
		t.Fatalf("host MCP did not use A sandbox: %v", r)
	}
	for name, want := range map[string]string{"remote-write": "remote", "host-write": "host"} {
		data, err := os.ReadFile(filepath.Join(e.Project, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s did not reach workspace: %q %v", name, data, err)
		}
	}
	log, _ := os.ReadFile(filepath.Join(e.AData, "audit.log"))
	if !strings.Contains(string(log), `"exec_mode":"remote"`) || !strings.Contains(string(log), `"tool":"Bash"`) || !strings.Contains(string(log), "host-write") || strings.Contains(string(log), "printf remote > remote-write") {
		t.Fatalf("unexpected command routing in audit: %s", log)
	}
	// A launch flag overrides the global mode, restoring the original stub path.
	out, err = e.Cmd("claude", "--exec-mode", "host", "bash-location").CombinedOutput()
	r = parseResults(string(out))
	if err != nil || r["bash_code"] != "0" || r["bash_netns"] == "" || r["bash_netns"] == hostNetns(t) {
		t.Fatalf("host flag did not override config: %v\n%s", err, out)
	}
	waitNoMounts(t)
}
