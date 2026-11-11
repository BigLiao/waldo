// Command waldo is one static binary with four roles, chosen by argv[0] and
// the first argument:
//
//	waldo ...              A: the trusted client and its user commands
//	waldo sandbox-serve    A: inside the sandbox, serves files and exec
//	waldo host             B: per-session capsule and stub hub
//	<anything else>        B: inside the capsule, a stub that forwards to A
package main

import (
	"os"
	"path/filepath"
	"strings"

	"waldo/internal/cli"
	"waldo/internal/host"
	"waldo/internal/serve"
	"waldo/internal/stub"
)

func main() {
	base := filepath.Base(os.Args[0])
	if !strings.HasPrefix(base, "waldo") {
		os.Exit(stub.Main())
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "host":
			os.Exit(host.Main(os.Args[2:]))
		case "sandbox-serve":
			os.Exit(serve.Main())
		}
	}
	os.Exit(cli.Main(os.Args[1:]))
}
