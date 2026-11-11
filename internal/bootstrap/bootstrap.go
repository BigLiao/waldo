// Package bootstrap installs waldo's missing system dependencies during setup.
package bootstrap

import (
	_ "embed"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"waldo/internal/util"
)

//go:embed install.sh
var installer string

func arguments(role string, needs []string) ([]string, error) {
	if role != "a" && role != "b" {
		return nil, fmt.Errorf("bootstrap: unknown role %q", role)
	}
	args := []string{"-c", installer, "waldo-install", role}
	return append(args, needs...), nil
}

// Command returns a complete, POSIX-shell-quoted installer command. It embeds
// the script in the command rather than stdin, leaving stdin available to sudo.
func Command(role string, needs []string) (string, error) {
	args, err := arguments(role, needs)
	if err != nil {
		return "", err
	}
	parts := []string{"sh"}
	for _, arg := range args {
		parts = append(parts, util.ShellQuote(arg))
	}
	return strings.Join(parts, " "), nil
}

// Install runs the same installer locally, with the supplied terminal streams.
func Install(role string, needs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	args, err := arguments(role, needs)
	if err != nil {
		return err
	}
	cmd := exec.Command("sh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("安装依赖: %w", err)
	}
	return nil
}
