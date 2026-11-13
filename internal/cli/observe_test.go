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
		out, paseo, observe, err := wrapperPrefix(tc.in)
		if err != nil || !reflect.DeepEqual(out, tc.out) || paseo != tc.paseo || (observe != nil && *observe) != tc.observe {
			t.Fatalf("%v => %v, %t, %v, %v", tc.in, out, paseo, observe, err)
		}
	}
}

func TestObserveExplicitOverrideAndInheritance(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want *bool
	}{
		{[]string{"-p", "--observe=false"}, nil},
		{[]string{"--", "--observe=false"}, nil},
		{[]string{"--observe=false", "-p", "hi"}, new(false)},
		{[]string{"--observe=true", "-p", "hi"}, new(true)},
		{[]string{"--observe", "--observe=false"}, new(false)},
		{[]string{"--observe=false", "--observe"}, new(true)},
	} {
		_, _, got, err := wrapperPrefix(tc.args)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%v: got %v, want %v, err %v", tc.args, got, tc.want, err)
		}
	}
	for _, value := range []string{"", "yes", "FALSE", "1"} {
		if _, _, _, err := wrapperPrefix([]string{"--observe=" + value}); err == nil {
			t.Fatalf("accepted invalid --observe=%s", value)
		}
	}
}
