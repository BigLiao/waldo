package transport

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"waldo/internal/proto"
	"waldo/internal/termfilter"
	"waldo/internal/util"
)

type ProbeResult struct {
	Installed  bool
	Compatible bool
	Version    string
	Report     []byte
}

// Probe checks an existing installation without installing, upgrading or
// starting an agent. Authentication is noninteractive and the caller bounds
// the entire operation, including remote diagnostics, with its context.
func (s *SSH) Probe(ctx context.Context, request []byte) (ProbeResult, error) {
	var result ProbeResult
	script := fmt.Sprintf(`D=%s; B="$D/bin/waldo"; if [ ! -x "$B" ]; then echo WALDO-STATUS-MISSING; exit 0; fi; V=$("$B" version 2>/dev/null) || exit 1; printf 'WALDO-STATUS-VERSION %%s\n' "$V"; if [ "$V" = %s ]; then exec "$B" host --data-dir "$D" --doctor; fi`, s.remoteDirExpr(), util.ShellQuote(proto.FullVersion()))
	copySSH := *s
	copySSH.ExtraArgs = append([]string{
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1",
	}, s.ExtraArgs...)
	cmd := exec.CommandContext(ctx, "ssh", copySSH.Args(wrap(script))...)
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Stdin = bytes.NewReader(request)
	var stdout, stderr probeBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("SSH 检查失败: %w %s", err, termfilter.Sanitize(strings.TrimSpace(stderr.String())))
	}
	if stdout.truncated {
		return result, fmt.Errorf("远端状态报告超过 64 KiB")
	}
	line, rest, _ := strings.Cut(stdout.String(), "\n")
	if line == "WALDO-STATUS-MISSING" {
		return result, nil
	}
	version, ok := strings.CutPrefix(line, "WALDO-STATUS-VERSION ")
	if !ok || version == "" {
		return result, fmt.Errorf("无法解析远端状态协议")
	}
	result.Installed, result.Version = true, version
	result.Compatible = version == proto.FullVersion()
	if result.Compatible {
		result.Report = []byte(rest)
	}
	return result, nil
}

// Keep noisy SSH or remote output bounded without blocking pipe draining.
type probeBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *probeBuffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, (64<<10)-b.Len())
	if keep < n {
		b.truncated = true
	}
	b.Buffer.Write(p[:keep])
	return n, nil
}
