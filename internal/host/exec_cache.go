package host

import (
	"io"
	"sync"
	"time"

	"waldo/internal/proto"
)

// invalidateCachesBeforeExit is a local causal barrier: another yamux stream
// closing on A does not prove that B has consumed its final invalidation.
// Clear all project generations before the stub can observe command completion.
// Keep the same host -> watch -> cache lock order as connection-state updates;
// no transport operation is performed while any of these locks are held.
func (h *host) invalidateCachesBeforeExit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, watch := range h.cacheWatches {
		watch.mu.Lock()
		watch.cache.Invalidate()
		watch.mu.Unlock()
	}
}

// relayExec preserves the native exec protocol while placing a local cache
// barrier immediately before FExit. Stdin, EOF, signals and resize frames are
// copied unchanged. A socket EOF means the stub died, unlike an FStdinEOF frame.
func relayExec(remote, stub io.ReadWriteCloser, beforeExit func()) {
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			stub.Close()
			// yamux.Close can block while sending FIN. Interrupt local reads
			// and writes before closing, then let FIN finish independently.
			// This keeps command completion and stub death out of its timeout.
			if d, ok := remote.(interface{ SetDeadline(time.Time) error }); ok {
				d.SetDeadline(time.Now())
			} else if d, ok := remote.(interface{ SetReadDeadline(time.Time) error }); ok {
				d.SetReadDeadline(time.Now())
			}
			go remote.Close()
		})
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		io.Copy(remote, stub)
		stop()
	}()
	go func() {
		defer workers.Done()
		defer stop()
		writer := proto.NewFrameWriter(stub)
		for {
			kind, payload, err := proto.ReadFrame(remote)
			if err != nil {
				return
			}
			if kind == proto.FExit && beforeExit != nil {
				beforeExit()
			}
			if err := writer.Write(kind, payload); err != nil {
				return
			}
			if kind == proto.FExit {
				return
			}
		}
	}()
	workers.Wait()
}
