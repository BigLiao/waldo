package session

import (
	"io"
	"sync"
	"time"

	"waldo/internal/audit"
)

// File metrics describe completed SFTP RPCs reaching A. Native commands on A
// and cache hits on B do not pass through this boundary. Concurrent request
// durations accumulate; they are not a fraction of session wall-clock time.
func spliceObservedFS(b, a io.ReadWriteCloser, mutations io.Writer, al *audit.Logger) {
	stats := audit.NewFSStats()
	defer stats.Close()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var prev audit.FSCounts
		for {
			select {
			case <-ticker.C:
				prev = logFileCounts(al, prev, stats.Snapshot())
			case <-stop:
				logFileCounts(al, prev, stats.Snapshot())
				return
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(a, io.TeeReader(b, io.MultiWriter(mutations, stats.Requests())))
		a.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(b, io.TeeReader(a, stats.Responses()))
		b.Close()
	}()
	wg.Wait()
	stats.Close()
	close(stop)
	<-stopped
}

func logFileCounts(al *audit.Logger, prev, now audit.FSCounts) audit.FSCounts {
	if n := now.ReadCount - prev.ReadCount; n > 0 {
		al.Log("file-read", map[string]any{"count": n, "duration_ms": float64(now.ReadDuration-prev.ReadDuration) / float64(time.Millisecond), "scope": "sftp_rpc"})
	}
	if n := now.WriteCount - prev.WriteCount; n > 0 {
		al.Log("file-write", map[string]any{"count": n, "duration_ms": float64(now.WriteDuration-prev.WriteDuration) / float64(time.Millisecond), "scope": "sftp_rpc"})
	}
	if n := now.Dropped - prev.Dropped; n > 0 {
		al.Log("file-stats-dropped", map[string]any{"count": n})
	}
	return now
}
