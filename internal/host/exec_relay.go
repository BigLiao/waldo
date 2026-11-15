package host

import (
	"io"
	"sync"
	"time"

	"waldo/internal/proto"
)

// relayExec preserves frames and cancels the remote command when its stub dies.
// A socket EOF means the stub died, unlike an FStdinEOF frame.
func relayExec(remote, stub io.ReadWriteCloser) {
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
