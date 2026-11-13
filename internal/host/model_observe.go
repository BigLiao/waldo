package host

import (
	"log"
	"strings"
	"sync"
	"time"

	"waldo/internal/modelstats"
	"waldo/internal/proto"
)

type modelObservation struct {
	collector     *modelstats.Collector
	done, stopped chan struct{}
	once          sync.Once
}

func (h *host) startModelObservation() {
	if !h.hello.Observe {
		return
	}
	provider := h.hello.Profile.Telemetry
	c, err := modelstats.Start(provider)
	if err != nil {
		log.Printf("model observation unavailable: %v", err)
		return
	}
	args, env, err := modelstats.Config(provider, c.URL, h.hello.Profile.Args, nil)
	if err != nil {
		c.Close()
		log.Printf("model observation unavailable: %v", err)
		return
	}
	h.hello.Profile.Args, h.observeEnv = args, env
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		h.scrub[key] = true // private B collector settings must not enter A exec
	}
	o := &modelObservation{collector: c, done: make(chan struct{}), stopped: make(chan struct{})}
	h.modelObservation = o
	go func() {
		defer close(o.stopped)
		st, err := h.sess.OpenStream()
		if err != nil {
			return
		}
		defer func() { st.SetDeadline(time.Now()); go st.Close() }()
		st.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if proto.WriteMsg(st, proto.Header{Kind: proto.KindObserve}) != nil {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var prev modelstats.Stats
		send := func() bool {
			now := c.Snapshot()
			if now == prev {
				return true
			}
			// Best effort, never wait through a reconnect or a congested link.
			st.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
			if proto.WriteMsg(st, now) != nil {
				return false
			}
			prev = now
			return true
		}
		for {
			select {
			case <-ticker.C:
				if !send() {
					return
				}
			case <-o.done:
				send()
				return
			}
		}
	}()
}

func (h *host) stopModelObservation() {
	if o := h.modelObservation; o != nil {
		o.once.Do(func() {
			o.collector.Close()
			close(o.done)
			select {
			case <-o.stopped:
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
