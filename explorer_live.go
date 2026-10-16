package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const explorerEventKeepalive = 30 * time.Second

// handleEvents streams chain events to explorer pages so they update in place
// instead of waiting for a reload.
//
// The explorer runs its own listener, so this deliberately does not reuse the
// API server's stream: no second port to expose, and no cross-origin setup.
// GET /events
func (e *Explorer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// The explorer server sets a 30s WriteTimeout, which would cut the stream.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		http.Error(w, "failed to initialize event stream", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	blockCh := e.daemon.SubscribeBlocks()
	defer e.daemon.UnsubscribeBlocks(blockCh)

	chain := e.daemon.chain
	if err := writeExplorerEvent(w, flusher, "connected", map[string]any{
		"height":          chain.HeightFast(),
		"next_difficulty": chain.NextDifficultyFast(),
	}); err != nil {
		return
	}

	keepalive := time.NewTicker(explorerEventKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case block := <-blockCh:
			if block == nil {
				continue
			}
			hash := block.Hash()
			if err := writeExplorerEvent(w, flusher, "new_block", map[string]any{
				"height":    block.Header.Height,
				"hash":      fmt.Sprintf("%x", hash[:]),
				"timestamp": block.Header.Timestamp,
				"tx_count":  len(block.Transactions),
				// The chain map colors the new cell by the block's own values.
				// The key is not "difficulty": that tile shows the next target.
				"block_difficulty": block.Header.Difficulty,
				"fees":             blockFees(block),
				// The pages show the next target, not the mined block's own
				// difficulty. Sending the block's value would write a
				// different number into the same tile.
				"next_difficulty": chain.NextDifficultyFast(),
			}); err != nil {
				return
			}

		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeExplorerEvent(w http.ResponseWriter, flusher http.Flusher, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
