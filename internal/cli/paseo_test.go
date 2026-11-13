package cli

import (
	"reflect"
	"testing"
)

func TestPaseoPrefixPreservesPrompt(t *testing.T) {
	for _, tc := range []struct {
		in, out []string
		enabled bool
	}{
		{[]string{"--paseo", "-p", "hello"}, []string{"-p", "hello"}, true},
		{[]string{"-p", "--paseo"}, []string{"-p", "--paseo"}, false},
		{[]string{"--", "--paseo"}, []string{"--", "--paseo"}, false},
	} {
		out, enabled, _, err := wrapperPrefix(tc.in)
		if err != nil || enabled != tc.enabled || !reflect.DeepEqual(out, tc.out) {
			t.Fatalf("%v -> %v, %v", tc.in, out, enabled)
		}
	}
}
