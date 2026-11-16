package host

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var envIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loginCredentials reads only the profile's credential variables on B. SSH
// command sessions may not source the login configuration used by native Claude.
// Values remain in B memory and are scrubbed from every exec forwarded to A.
func loginCredentials(names []string) map[string]string {
	return readLoginCredentials("/bin/bash", names)
}

func readLoginCredentials(shell string, names []string) map[string]string {
	var valid []string
	var script strings.Builder
	script.WriteString(`printf '\000WALDO_LOGIN_ENV\000'; `)
	for _, name := range names {
		if envIdentifier.MatchString(name) {
			valid = append(valid, name)
			script.WriteString(`printf '%s\000' "${` + name + `-}"; `)
		}
	}
	result := make(map[string]string)
	if len(valid) == 0 {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", script.String())
	cmd.WaitDelay = 100 * time.Millisecond
	var output credentialBuffer
	cmd.Stdout = &output
	if cmd.Run() != nil || output.overflow {
		return result
	}
	marker := []byte("\x00WALDO_LOGIN_ENV\x00")
	idx := bytes.LastIndex(output.Bytes(), marker)
	if idx < 0 {
		return result
	}
	values := bytes.Split(output.Bytes()[idx+len(marker):], []byte{0})
	if len(values) != len(valid)+1 {
		return result
	}
	for i, name := range valid {
		if len(values[i]) > 0 {
			result[name] = string(values[i])
		}
	}
	return result
}

type credentialBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *credentialBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (256 << 10) - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.Buffer.Write(p)
	return n, nil
}
