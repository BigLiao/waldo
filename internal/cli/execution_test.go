package cli

import (
	"reflect"
	"testing"
)

func TestExecutionPrefix(t *testing.T) {
	for _, tc := range []struct {
		in, out []string
		want    string
	}{
		{[]string{"--exec-mode", "remote", "--paseo", "--observe", "-p", "hello"}, []string{"-p", "hello"}, "remote"},
		{[]string{"--paseo", "--exec-mode=host-mcp", "-p", "hi"}, []string{"-p", "hi"}, "host-mcp"},
		{[]string{"--paseo", "--exec-mode=remote", "claude"}, []string{"claude"}, "remote"},
		{[]string{"--exec-mode=remote", "--exec-mode", "host", "-p", "hi"}, []string{"-p", "hi"}, "host"},
		{[]string{"-p", "--exec-mode=remote"}, []string{"-p", "--exec-mode=remote"}, ""},
		{[]string{"--", "--exec-mode", "remote"}, []string{"--", "--exec-mode", "remote"}, ""},
	} {
		out, opts, err := wrapperPrefix(tc.in)
		mode := opts.ExecMode
		if err != nil || !reflect.DeepEqual(out, tc.out) || mode != tc.want {
			t.Fatalf("%v: %v %q %v", tc.in, out, mode, err)
		}
	}
	for _, args := range [][]string{{"--exec-mode"}, {"--exec-mode="}, {"--exec-mode", "bad"}, {"--exec-mode=REMOTE"}} {
		if _, _, err := wrapperPrefix(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
