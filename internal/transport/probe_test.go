package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"waldo/internal/proto"
)

func probeTestEnv(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$@" > "$PROBE_ARGS"
case "$PROBE_MODE" in
  missing) echo WALDO-STATUS-MISSING ;;
  mismatch) echo 'WALDO-STATUS-VERSION old-build' ;;
  ready)
    printf 'WALDO-STATUS-VERSION %s\n' "$PROBE_VERSION"
    printf '{"version":"test","checks":[]}\n'
    ;;
  failed) echo 'Permission denied' >&2; exit 255 ;;
  slow) exec /bin/sleep 5 ;;
  oversized) printf '%70000s' x ;;
  *) echo 'broken protocol' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PROBE_MODE", mode)
	t.Setenv("PROBE_VERSION", proto.FullVersion())
	t.Setenv("PROBE_ARGS", filepath.Join(dir, "args"))
	return filepath.Join(dir, "args")
}

func TestProbeDoesNotInstallOrPrompt(t *testing.T) {
	for _, mode := range []string{"missing", "mismatch", "ready", "failed", "oversized", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			path := probeTestEnv(t, mode)
			s := &SSH{Host: "two", ExtraArgs: []string{"-i", "key-two", "-o", "BatchMode=no"}, RemoteDir: "~/data-two"}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := s.Probe(ctx, []byte(`{"profiles":[]}`))
			switch mode {
			case "missing":
				if err != nil || result.Installed {
					t.Fatalf("%+v %v", result, err)
				}
			case "mismatch":
				if err != nil || !result.Installed || result.Compatible || result.Version != "old-build" || len(result.Report) != 0 {
					t.Fatalf("%+v %v", result, err)
				}
			case "ready":
				if err != nil || !result.Compatible || !strings.Contains(string(result.Report), `"checks"`) {
					t.Fatalf("%+v %v", result, err)
				}
			default:
				if err == nil {
					t.Fatal("bad probe accepted")
				}
			}
			b, _ := os.ReadFile(path)
			args := string(b)
			if strings.Index(args, "BatchMode=yes") > strings.Index(args, "BatchMode=no") || !strings.Contains(args, "StrictHostKeyChecking=yes") || !strings.Contains(args, "\ntwo\n") || !strings.Contains(args, "key-two") || strings.Contains(args, "WALDO-NEED-INSTALL") || strings.Contains(args, "waldo-install") {
				t.Fatalf("unsafe or wrong probe command: %s", args)
			}
		})
	}
}

func TestProbeDeadlineBoundsWholeCommand(t *testing.T) {
	probeTestEnv(t, "slow")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&SSH{Host: "slow"}).Probe(ctx, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced: %v after %s", err, time.Since(start))
	}
}
