package transport

import (
	"strings"
	"testing"
)

// B must not be able to use the connection to reach back into A.
func TestHardening(t *testing.T) {
	s := &SSH{Host: "me@b", ExtraArgs: []string{"-F", "cfg"}}
	args := strings.Join(s.Args("cmd"), " ")
	for _, want := range []string{"-T", "ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "Tunnel=no", "-e none"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args lack %q: %s", want, args)
		}
	}
	a := s.Args("cmd")
	if a[len(a)-2] != "me@b" || a[len(a)-3] != "--" {
		t.Errorf("host must follow --: %v", a)
	}
	// User options come first so hardening options win (ssh uses the first value).
	if a[0] != "-F" {
		t.Errorf("extra args first: %v", a)
	}
}

func TestRemoteDirExpr(t *testing.T) {
	for in, want := range map[string]string{
		"":                    `"$HOME"/.local/share/waldo`,
		"~/x y":               `"$HOME"/'x y'`,
		"/srv/w":              "/srv/w",
		"/tmp/$(touch pwned)": `'/tmp/$(touch pwned)'`,
	} {
		if got := (&SSH{RemoteDir: in}).remoteDirExpr(); got != want {
			t.Errorf("%q -> %s want %s", in, got, want)
		}
	}
}
