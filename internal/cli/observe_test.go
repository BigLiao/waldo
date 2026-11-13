package cli

import (
	"reflect"
	"testing"
)

func TestObservePrefixPreservesPrompt(t *testing.T) {
	for _, tc := range []struct {
		in, out        []string
		paseo, observe bool
	}{
		{[]string{"--observe", "-p", "hi"}, []string{"-p", "hi"}, false, true},
		{[]string{"--paseo", "--observe", "-p", "hi"}, []string{"-p", "hi"}, true, true},
		{[]string{"--observe", "--paseo", "claude"}, []string{"claude"}, true, true},
		{[]string{"-p", "--observe"}, []string{"-p", "--observe"}, false, false},
		{[]string{"--", "--observe"}, []string{"--", "--observe"}, false, false},
	} {
		out, paseo, observe := wrapperPrefix(tc.in)
		if !reflect.DeepEqual(out, tc.out) || paseo != tc.paseo || observe != tc.observe {
			t.Fatalf("%v => %v, %t, %t", tc.in, out, paseo, observe)
		}
	}
}
