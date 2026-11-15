package cli

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"waldo/internal/config"
)

func TestRemotePrefixAndArgumentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		in, out []string
		remote  string
	}{
		{[]string{"--remote", "b1", "--paseo", "-p", "hi"}, []string{"-p", "hi"}, "b1"},
		{[]string{"--remote=b2", "claude"}, []string{"claude"}, "b2"},
		{[]string{"--remote=b1", "--remote", "b2"}, []string{}, "b2"},
		{[]string{"-p", "--remote=b2"}, []string{"-p", "--remote=b2"}, ""},
		{[]string{"--", "--remote", "b2"}, []string{"--", "--remote", "b2"}, ""},
	} {
		out, opts, err := wrapperPrefix(tc.in)
		if err != nil || opts.Remote != tc.remote || !reflect.DeepEqual(out, tc.out) {
			t.Fatalf("%v -> %v %+v %v", tc.in, out, opts, err)
		}
	}
	for _, args := range [][]string{{"--remote"}, {"--remote="}, {"--remote", "--paseo"}, {"--remote=../x"}} {
		if _, _, err := wrapperPrefix(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	opts := wrapperOptions{Remote: "b2"}
	opts.inherit(wrapperOptions{Remote: "b1", Paseo: true, ExecMode: "host-mcp"})
	if opts.Remote != "b2" || !opts.Paseo || opts.ExecMode != "host-mcp" {
		t.Fatalf("prefix inheritance: %+v", opts)
	}
}

func TestSetupAndDoctorChooseNamedMachine(t *testing.T) {
	for _, command := range []string{"setup", "doctor"} {
		for _, args := range [][]string{{"--remote", "b2"}, {"--remote=b2"}, nil} {
			t.Run(command+strings.Join(args, " "), func(t *testing.T) {
				setupTestEnv(t, true)
				cfg := &config.Config{Host: "legacy", DefaultRemote: "b1",
					SSHArgs: []string{"-i", "global-key"}, RemoteDir: "~/global",
					Remote: map[string]config.RemoteCfg{
						"b1": {Host: "one"},
						"b2": {Host: "two", SSHArgs: []string{"-i", "key-two"}, RemoteDir: "~/two"},
					}}
				var code int
				if command == "setup" {
					code = setup(cfg, append(append([]string(nil), args...), "--skip-deps"))
				} else {
					code = doctorCommand(cfg, args)
				}
				if code != 0 {
					t.Fatalf("exit %d", code)
				}
				b, err := os.ReadFile(os.Getenv("SETUP_TEST_SSH_LOG"))
				if err != nil {
					t.Fatal(err)
				}
				wantHost, wantKey, wantDir := "one", "global-key", "global"
				if len(args) > 0 {
					wantHost, wantKey, wantDir = "two", "key-two", "two"
				}
				log := string(b)
				if !strings.Contains(log, "\n"+wantHost+"\n") || !strings.Contains(log, "\n"+wantKey+"\n") || !strings.Contains(log, `D="$HOME"/`+wantDir+";") || strings.Contains(log, "\nlegacy\n") {
					t.Fatalf("wrong SSH destination/options/data directory: %s", log)
				}
				if cfg.Host != "legacy" || cfg.DefaultRemote != "b1" || cfg.SSHArgs[1] != "global-key" {
					t.Fatal("setup/doctor changed default configuration")
				}
				if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
					t.Fatal("named setup/doctor rewrote config")
				}
			})
		}
	}
}

func TestInvalidRemoteCommandsNeverConnect(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--remote", "missing"}, {"--remote=missing", "claude"},
		{"run", "--remote=missing", "--", "echo"},
		{"setup", "--remote=missing"}, {"doctor", "--remote=missing"},
		{"setup", "--remote="}, {"doctor", "--remote="},
		{"setup", "--remote=b1", "new-host"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			log := setupTestEnv(t, false)
			if code := Main(args); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("invalid selection ran setup or SSH")
			}
		})
	}
}

func TestMainAcceptsRemoteOnEitherSideOfCommand(t *testing.T) {
	for _, args := range [][]string{
		{"--remote", "b2", "doctor"}, {"doctor", "--remote", "b2"},
		{"--remote=b1", "doctor", "--remote=b2"},
		{"--remote=b2", "setup", "--skip-deps"}, {"setup", "--skip-deps", "--remote=b2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setupTestEnv(t, true)
			if err := os.MkdirAll(config.ConfigDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			data := []byte("default_remote = \"b1\"\n[remote.b1]\nhost = \"one\"\n[remote.b2]\nhost = \"two\"\n")
			if err := os.WriteFile(config.Path(), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if code := Main(args); code != 0 {
				t.Fatalf("exit %d", code)
			}
			b, _ := os.ReadFile(os.Getenv("SETUP_TEST_SSH_LOG"))
			if !strings.Contains(string(b), "\ntwo\n") || strings.Contains(string(b), "\none\n") {
				t.Fatal("wrapper remote selection was not passed to command")
			}
		})
	}
}
