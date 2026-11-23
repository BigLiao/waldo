package hostbash

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"waldo/internal/hosttools"
	"waldo/internal/proto"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "host-file-tool" {
		os.Exit(hosttools.Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestLocalFileRoutingVersionsAndNoFallback(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(&pathRouter{cwd: dir}, filepath.Join(dir, "absent-socket"), "/bin/bash", dir)
	defer r.Close()
	file := filepath.Join(dir, "nested", "test.txt")
	call := func(name string, args map[string]any) (Result, error) {
		t.Helper()
		b, _ := json.Marshal(args)
		return r.Run(context.Background(), Call{Tool: name, Arguments: b})
	}
	must := func(name string, args map[string]any) Result {
		t.Helper()
		res, e := call(name, args)
		if e != nil || res.ExitCode != 0 || res.Location != "agent" {
			t.Fatalf("%s: %+v %v", name, res, e)
		}
		return res
	}
	must("Write", map[string]any{"file_path": file, "content": "hello"})
	must("Read", map[string]any{"file_path": file})
	must("Edit", map[string]any{"file_path": file, "old_string": "hello", "new_string": ""})
	b, _ := os.ReadFile(file)
	if len(b) != 0 {
		t.Fatal(string(b))
	}
	os.WriteFile(file, []byte("external"), 0600)
	res, e := call("Write", map[string]any{"file_path": file, "content": "overwrite"})
	if e == nil && res.ExitCode == 0 {
		t.Fatal("overwrote external change")
	}
	must("Read", map[string]any{"file_path": file})
	must("Write", map[string]any{"file_path": file, "content": ""})
	res, e = call("Read", map[string]any{"file_path": file, "location": "host"})
	if e == nil || !strings.Contains(e.Error(), "A command channel unavailable") {
		t.Fatal("host read fell back", res, e)
	}
	res, e = call("Read", map[string]any{"file_path": file + "missing"})
	if e != nil || res.ExitCode == 0 || res.Location != "agent" {
		t.Fatal(res, e)
	}
}

func TestBackgroundTaskLifecycle(t *testing.T) {
	socket := execSocket(t, func(c net.Conn) { receiveExec(t, c); proto.ReadFrame(c) })
	r := newRunner(&pathRouter{cwd: "/"}, socket, "/bin/bash", "/")
	defer r.Close()
	res, e := r.Run(context.Background(), Call{Command: "sleep", Background: true})
	if e != nil || !res.Running || res.TaskID == "" {
		t.Fatal(res, e)
	}
	block := false
	status, e := r.Run(context.Background(), Call{Tool: "TaskOutput", TaskID: res.TaskID, Block: &block})
	if e != nil || !status.Running {
		t.Fatal(status, e)
	}
	if _, e = r.Run(context.Background(), Call{Tool: "TaskStop", TaskID: res.TaskID, Location: "agent"}); e == nil {
		t.Fatal("accepted wrong location")
	}
	stopped, e := r.Run(context.Background(), Call{Tool: "TaskStop", TaskID: res.TaskID})
	if e != nil || stopped.Running {
		t.Fatal(stopped, e)
	}
}

func TestAgentBashUsesNativeShellAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NVM_BIN", "/host/node")
	t.Setenv("PYTHONHOME", "/host/python")
	r := newRunner(&pathRouter{cwd: dir}, "/missing", "/host/bash", dir)
	defer r.Close()
	res, e := r.Run(context.Background(), Call{Command: `/usr/bin/env bash -c 'printf NATIVE_OK; test -z "$NVM_BIN$PYTHONHOME"'`, Location: "agent"})
	if e != nil || res.ExitCode != 0 || res.Stdout != "NATIVE_OK" || res.Location != "agent" {
		t.Fatal(res, e)
	}
	res, e = r.Run(context.Background(), Call{Command: "sleep 20", Location: "agent", TimeoutMS: 20})
	if e == nil || res.ExitCode != 255 {
		t.Fatal("local timeout did not cancel", res, e)
	}
}
