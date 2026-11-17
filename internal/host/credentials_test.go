package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoginCredentialsReadOnlyRequestedIdentifiers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "login-shell")
	script := `#!/bin/sh
printf 'login startup chatter\n'
export TEST_CLAUDE_KEY='test-only-secret'
export TEST_OTHER_KEY='must-not-be-returned'
exec /bin/sh "$@"
`
	if err := os.WriteFile(file, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	values := readLoginCredentials(file, []string{"TEST_CLAUDE_KEY", "MISSING", "BAD;touch pwned"})
	if len(values) != 1 || values["TEST_CLAUDE_KEY"] != "test-only-secret" {
		t.Fatal("credential allow-list did not match")
	}
}
