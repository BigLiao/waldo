package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"waldo/internal/config"
	"waldo/internal/hosttools"
	"waldo/internal/proto"
	"waldo/internal/serve"
	"waldo/internal/util"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sandbox-serve":
			os.Exit(serve.Main())
		case "host-file-tool":
			os.Exit(hosttools.Main(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

func TestDirectSessionUsesLocalFilesystemAndPrivateRuntime(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")
	for _, dir := range []string{home, project} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "marker"), []byte("local-home"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DIRECT_TEST_SECRET", "must-stay-local")
	pol, err := (&config.Config{}).PolicyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	if pol.SandboxEnabled {
		t.Fatal("local sandbox must default to off")
	}
	sb, err := StartSandbox(SandboxOptions{
		Unsandboxed: !pol.SandboxEnabled, Home: home, Project: project, Policy: pol,
		DataDir: filepath.Join(root, "data"), ContextGuide: []byte("guide at " + proto.ContextFile),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	tmp := sb.ExportPath("/tmp")
	if sb.Facts.Home != home || tmp == "/tmp" || sb.Fwd != nil {
		t.Fatalf("unexpected runtime: HOME=%q tmp=%q forwarder=%v", sb.Facts.Home, tmp, sb.Fwd != nil)
	}
	if _, ok := util.EnvGet(sb.Facts.Env, "DIRECT_TEST_SECRET"); ok {
		t.Fatal("the host's unlisted environment reached B")
	}

	out := directExec(t, sb, "/bin/sh", []string{"sh", "-c", `cat "$HOME/marker"; printf '\n%s' "$TMPDIR"; test -z "$HTTP_PROXY"`}, "")
	if out != "local-home\n"+tmp {
		t.Fatalf("unexpected shell output: %q", out)
	}

	helper := []string{"/run/waldo/waldo", "host-file-tool", "Read"}
	out = directExec(t, sb, helper[0], helper, fmt.Sprintf(`{"file_path":%q}`, filepath.Join(home, "marker")))
	if !strings.Contains(out, "local-home") {
		t.Fatalf("unexpected file tool output: %q", out)
	}

	st, err := sb.Open(proto.Header{Kind: proto.KindFS, Path: project})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(5 * time.Second))
	client, err := sftp.NewClientPipe(st, st)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.Open(filepath.Join(project, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := io.ReadAll(f)
	f.Close()
	client.Close()
	if err != nil || !strings.Contains(string(guide), sb.Facts.ContextFile) || sb.Facts.ContextFile == proto.ContextFile {
		t.Fatalf("invalid context projection: %q, %v", guide, err)
	}

	directExec(t, sb, "/bin/sh", []string{"sh", "-c", `
		(trap 'printf stopped > stopped; exit' TERM; : > ready; while :; do sleep 1; done) >/dev/null 2>&1 &
		while [ ! -f ready ]; do sleep 0.1; done
	`}, "")
	sb.Close()
	if stopped, err := os.ReadFile(filepath.Join(project, "stopped")); err != nil || string(stopped) != "stopped" {
		t.Fatalf("the background job did not receive SIGTERM: %q, %v", stopped, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temporary directory survived the session: %v", err)
	}
}

func directExec(t *testing.T, sb *Sandbox, path string, argv []string, input string) string {
	t.Helper()
	st, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: &proto.ExecReq{
		Path: path, Argv: argv, Cwd: sb.Project, Env: sb.Facts.Env,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(5 * time.Second))
	fw := proto.NewFrameWriter(st)
	if input != "" {
		if err := fw.Write(proto.FStdin, []byte(input)); err != nil {
			t.Fatal(err)
		}
	}
	if err := fw.Write(proto.FStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for {
		kind, p, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case proto.FStdout, proto.FStderr, proto.FError:
			out.Write(p)
		case proto.FExit:
			var status proto.Exit
			if err := proto.DecodeJSON(p, &status); err != nil || status.Code != 0 {
				t.Fatalf("exec failed (%+v, %v): %s", status, err, out.String())
			}
			return out.String()
		}
	}
}
