package termfilter

import (
	"bytes"
	"testing"
)

func run(chunks ...string) string {
	var out bytes.Buffer
	f := New(&out)
	for _, c := range chunks {
		f.Write([]byte(c))
	}
	return out.String()
}

func TestFilter(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"plain", []string{"hello\r\n"}, "hello\r\n"},
		{"utf8", []string{"你好 ✓"}, "你好 ✓"},
		{"csi color", []string{"\x1b[1;31mred\x1b[0m"}, "\x1b[1;31mred\x1b[0m"},
		{"csi split", []string{"\x1b", "[3", "1m", "x"}, "\x1b[31mx"},
		{"private csi", []string{"\x1b[?2026h\x1b[?1049h"}, "\x1b[?2026h\x1b[?1049h"},
		{"cursor report query kept", []string{"\x1b[6n"}, "\x1b[6n"},
		{"title report dropped", []string{"a\x1b[21tb"}, "ab"},
		{"osc52 bel", []string{"a\x1b]52;c;ZXZpbA==\x07b"}, "ab"},
		{"osc52 st split", []string{"a\x1b]5", "2;c;ZXZp", "bA==\x1b", "\\b"}, "ab"},
		{"osc title", []string{"\x1b]0;my title\x07"}, "\x1b]0;my title\x07"},
		{"osc8 link", []string{"\x1b]8;;https://x\x1b\\t\x1b]8;;\x1b\\"}, "\x1b]8;;https://x\x1b\\t\x1b]8;;\x1b\\"},
		{"osc unknown", []string{"\x1b]1337;File=x\x07z"}, "z"},
		{"dcs dropped", []string{"a\x1bPtmux;\x1b\x1b]52;c;x\x07\x1b\\b"}, "ab"},
		{"apc dropped", []string{"a\x1b_Gf=100;AAAA\x1b\\b"}, "ab"},
		{"charset", []string{"\x1b(B"}, "\x1b(B"},
		{"esc simple", []string{"\x1b7\x1b8\x1bM"}, "\x1b7\x1b8\x1bM"},
		{"can aborts", []string{"\x1b]52;c;\x18ok"}, "ok"},
	}
	for _, c := range cases {
		if got := run(c.in...); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	in := "ok\x1b]52;c;ZXZpbA==\x07 \x1b[31mred\x1b[0m\r\x08\u009b6n\nnext"
	if got := Sanitize(in); got != "ok red\nnext" {
		t.Errorf("got %q", got)
	}
}
