package host

import (
	"log"
	"path/filepath"

	"waldo/internal/toolhooks"
)

func (h *host) startToolObservation() {
	adapter := h.hello.Profile.ToolHooks
	if !h.hello.Observe || adapter == "" || adapter == "none" {
		return
	}
	c, err := toolhooks.Start(filepath.Join(h.dataDir, "log"), h.hello.Session)
	if err != nil {
		log.Printf("tool observation unavailable: %v", err)
		return
	}
	// Examine the complete argument list so a provider/SDK --settings value
	// cannot replace the added hooks, or lose its own hooks and permissions.
	args := append(append([]string(nil), h.hello.Profile.Args...), h.hello.Argv...)
	args, err = toolhooks.Config(adapter, c.URL, args)
	if err != nil {
		c.Close()
		log.Printf("tool observation unavailable: %v", err)
		return
	}
	h.hello.Profile.Args, h.hello.Argv = nil, args
	h.toolObservation = c
	log.Printf("session %s: B tool hooks configured; log=tool-timing.jsonl", h.hello.Session)
}

func (h *host) stopToolObservation() {
	if h.toolObservation != nil {
		h.toolObservation.Close()
	}
}
