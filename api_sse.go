package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// handleEvents streams real-time events via Server-Sent Events.
// Event types: new_block, mined_block, sync_status
// GET /api/events
func (s *APIServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Disable write timeout for this long-lived connection.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, http.StatusInternalServerError, "failed to initialize SSE stream")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe to block events
	blockCh := s.daemon.SubscribeBlocks()
	minedCh := s.daemon.SubscribeMinedBlocks()
	defer s.daemon.UnsubscribeBlocks(blockCh)
	defer s.daemon.UnsubscribeMinedBlocks(minedCh)

	// Send initial status so the client knows the connection is live
	syncing := false
	if s.daemon != nil && s.daemon.syncMgr != nil {
		syncing = s.daemon.syncMgr.IsSyncing()
	}
	if err := s.sendSSE(w, flusher, "connected", map[string]any{
		"chain_height": s.daemon.Chain().Height(),
		"syncing":      syncing,
	}); err != nil {
		log.Printf("SSE connected event write failed: %v", err)
		return
	}

	// Keepalive ticker (SSE comment line to prevent proxies from killing idle connections)
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case block := <-blockCh:
			if block == nil {
				continue
			}
			if err := s.sendSSE(w, flusher, "new_block", map[string]any{
				"height":    block.Header.Height,
				"hash":      fmt.Sprintf("%x", block.Hash()),
				"timestamp": block.Header.Timestamp,
				"tx_count":  len(block.Transactions),
			}); err != nil {
				return
			}

		case block := <-minedCh:
			if block == nil {
				continue
			}
			if err := s.sendSSE(w, flusher, "mined_block", map[string]any{
				"height": block.Header.Height,
				"hash":   fmt.Sprintf("%x", block.Hash()),
				"reward": coinbaseReward(block),
			}); err != nil {
				return
			}

		case <-keepalive.C:
			// SSE comment line — keeps the connection alive through proxies/load balancers
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// sendSSE writes a single SSE event.
func (s *APIServer) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) error {
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
