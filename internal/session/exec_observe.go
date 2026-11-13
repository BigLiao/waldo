package session

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"waldo/internal/audit"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// execObserver only retains the small exit frame. Output and control bytes are
// copied unchanged; malformed input disables observation, never forwarding.
type execObserver struct {
	header    [5]byte
	headerN   int
	remaining uint32
	kind      byte
	exitBody  [1024]byte
	exitN     int
	disabled  bool
	exit      *proto.Exit
	endedAt   time.Time
}

func (o *execObserver) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !o.disabled && o.exit == nil {
		if o.headerN < len(o.header) {
			k := copy(o.header[o.headerN:], p)
			o.headerN += k
			p = p[k:]
			if o.headerN < len(o.header) {
				break
			}
			o.kind = o.header[0]
			o.remaining = binary.BigEndian.Uint32(o.header[1:])
			if o.remaining > 1<<20 || (o.kind == proto.FExit && o.remaining > uint32(len(o.exitBody))) {
				o.disabled = true
				break
			}
			o.exitN = 0
		}
		k := min(len(p), int(o.remaining))
		if o.kind == proto.FExit {
			o.exitN += copy(o.exitBody[o.exitN:], p[:k])
		}
		p = p[k:]
		o.remaining -= uint32(k)
		if o.remaining == 0 {
			if o.kind == proto.FExit {
				var exit proto.Exit
				if json.Unmarshal(o.exitBody[:o.exitN], &exit) == nil {
					o.exit, o.endedAt = &exit, time.Now()
				} else {
					o.disabled = true
				}
			}
			o.headerN = 0
		}
	}
	return n, nil
}

type execObservedConn struct {
	io.ReadWriteCloser
	observer *execObserver
}

func (c execObservedConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.observer.Write(p[:n])
	return n, err
}

// Preserve the half-close behavior util.Splice would use without observation.
func (c execObservedConn) CloseWrite() error {
	if closer, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return c.ReadWriteCloser.Close()
}

// spliceExec measures the trusted A forwarding boundary, not B's full tool
// invocation or just process runtime. It includes sandbox execution and its
// cache barrier. Disk logging happens after forwarding has finished.
func spliceExec(b, a io.ReadWriteCloser, al *audit.Logger, id uint64, program string, start time.Time) {
	observer := &execObserver{}
	util.Splice(b, execObservedConn{ReadWriteCloser: a, observer: observer})
	end, status := observer.endedAt, "completed"
	if observer.exit == nil {
		end, status = time.Now(), "disconnected"
	}
	logExecEnd(al, id, program, start, end, status, observer.exit)
}

func logExecEnd(al *audit.Logger, id uint64, program string, start, end time.Time, status string, exit *proto.Exit) {
	fields := map[string]any{
		"id": id, "program": filepath.Base(program), "status": status,
		"scope":       "a_forward",
		"duration_ms": float64(end.Sub(start)) / float64(time.Millisecond),
	}
	if exit != nil {
		fields["exit_code"] = exit.Code
		if exit.Signal != 0 {
			fields["signal"] = exit.Signal
		}
	}
	al.Log("exec-end", fields)
}
