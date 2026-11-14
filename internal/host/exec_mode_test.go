package host

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/hostbash"
	"waldo/internal/proto"
	"waldo/internal/util"
)

func TestHostMCPReusesSessionAndKeepsAEnvironment(t *testing.T) {
	a, b := net.Pipe()
	client, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h := &host{sess: remote, hello: remoteHello(), scrub: map[string]bool{"ANTHROPIC_API_KEY": true}}
	h.hello.Facts.Env = append(h.hello.Facts.Env, "ANTHROPIC_API_KEY=must-stay-hidden")
	socket := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go h.serveStubs(ln)
	done := make(chan error, 1)
	go func() {
		stream, err := client.AcceptStream()
		if err != nil {
			done <- err
			return
		}
		defer stream.Close()
		stream.SetDeadline(time.Now().Add(3 * time.Second))
		var header proto.Header
		if err := proto.ReadMsg(stream, &header); err != nil {
			done <- err
			return
		}
		if header.Kind != proto.KindExec || header.Exec == nil {
			t.Error("did not reuse exec protocol")
			done <- io.ErrUnexpectedEOF
			return
		}
		path, _ := util.EnvGet(header.Exec.Env, "PATH")
		if path != "/A/bin" {
			t.Errorf("B PATH reached A: %v", header.Exec.Env)
		}
		if _, ok := util.EnvGet(header.Exec.Env, "ANTHROPIC_API_KEY"); ok {
			t.Error("credential reached A")
		}
		kind, _, err := proto.ReadFrame(stream)
		if err != nil || kind != proto.FStdinEOF {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- proto.NewFrameWriter(stream).WriteJSON(proto.FExit, proto.Exit{})
	}()
	result, err := hostbash.Execute(context.Background(), socket, "/A/bin/bash", "/project", hostbash.Call{Command: "id"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("MCP over existing session: %+v %v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func remoteHello() proto.Hello {
	return proto.Hello{ExecMode: proto.ExecRemote, Session: "mode-test", Project: "/project", Cwd: "/project/sub", Profile: proto.Profile{MCPConfigFlag: "--mcp-config", Args: []string{"--verbose"}}, Argv: []string{"-p", "hello"}, Facts: proto.Facts{Home: "/home/agent", Env: []string{"HOME=/home/agent", "PATH=/A/bin", "SHELL=/A/bin/bash", "NVM_BIN=/A/node", "GOROOT=/A/go"}, PathDirs: []proto.PathDir{{Dir: "/A/bin", Names: []string{"bash"}, OnPath: true}}}}
}

func TestRemoteExecConfigurationAndEnvironment(t *testing.T) {
	h := &host{hello: remoteHello()}
	if err := h.configureExecMode(); err != nil {
		t.Fatal(err)
	}
	if h.hello.Profile.Args != nil || !strings.Contains(strings.Join(h.hello.Argv, " "), "host-bash-mcp") || !strings.Contains(strings.Join(h.hello.Argv, " "), "/A/bin/bash") {
		t.Fatal("missing native MCP injection")
	}
	env := h.agentEnv()
	path, _ := util.EnvGet(env, "PATH")
	shell, _ := util.EnvGet(env, "SHELL")
	if !strings.Contains(path, "/usr/bin") || strings.Contains(path, "/A/") || shell != "/bin/bash" {
		t.Fatalf("remote environment: %v", env)
	}
	for _, key := range []string{"NVM_BIN", "GOROOT"} {
		if _, ok := util.EnvGet(env, key); ok {
			t.Fatalf("forwarded A toolchain %s to B", key)
		}
	}
	if session, _ := util.EnvGet(env, "WALDO_SESSION"); session != "mode-test" {
		t.Fatal("lost session environment")
	}
}

func TestExecModeDefaultsAndUnsupportedProfiles(t *testing.T) {
	args := []string{"-p", "unchanged"}
	h := &host{hello: proto.Hello{Argv: args}}
	if err := h.configureExecMode(); err != nil || h.hello.ExecMode != proto.ExecHost || !reflect.DeepEqual(h.hello.Argv, args) {
		t.Fatalf("changed default: %+v %v", h.hello, err)
	}
	for _, hello := range []proto.Hello{{ExecMode: "invalid"}, {ExecMode: proto.ExecRemote}, {ExecMode: proto.ExecRemote, Profile: proto.Profile{MCPConfigFlag: "--mcp-config"}}} {
		if err := (&host{hello: hello}).configureExecMode(); err == nil {
			t.Fatal("accepted unsupported remote configuration")
		}
	}
}

// No FUSE is needed to verify the native capsule layout. Project mounts here
// are direct binds; the full SSHFS + A sandbox path is covered by the E2E test.
func TestRemoteCapsuleExecutesNativeBash(t *testing.T) {
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap unavailable")
	}
	if out, err := exec.Command(bw, "--unshare-user", "--ro-bind", "/", "/", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bwrap namespace unavailable: %v %s", err, out)
	}
	dir := t.TempDir()
	for _, sub := range []string{"home", "s", "project"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	h := &host{runDir: dir, dataDir: dir, hello: remoteHello()}
	h.hello.Facts.Home = "/home/agent"
	h.hello.Project = filepath.Join(dir, "project")
	h.hello.Cwd = h.hello.Project
	h.hello.Mounts = []string{h.hello.Project}
	h.hello.Profile.Args = nil
	h.hello.Argv = []string{"--noprofile", "--norc", "-c", `readlink /proc/self/ns/net; printf remote > written; command -v python3`}
	ag, err := resolveAgent("/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	args, err := h.capsuleArgs(filepath.Join(dir, "home"), []string{h.hello.Project}, nil, ag)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bw, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("native capsule: %v\n%s", err, out)
	}
	ns, _ := os.Readlink("/proc/self/ns/net")
	if !strings.Contains(string(out), ns) {
		t.Fatalf("Bash did not use B's network namespace: %s", out)
	}
	data, err := os.ReadFile(filepath.Join(h.hello.Project, "written"))
	if err != nil || string(data) != "remote" {
		t.Fatalf("project write: %q %v", data, err)
	}
}
