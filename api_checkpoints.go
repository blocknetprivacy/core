package main

import "net/http"

// handleSaveCheckpoints atomically rebuilds checkpoints from finalized canonical
// blocks. Existing entries are revalidated rather than blindly appended.
// POST /api/checkpoints/save
func (s *APIServer) handleSaveCheckpoints(w http.ResponseWriter, r *http.Request) {
	cpPath := checkpointsPath(s.dataDir)
	written, height, lastHeight, err := s.daemon.saveCanonicalCheckpoints(cpPath, true)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to save checkpoints", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"written":         written,
		"chain_height":    height,
		"last_checkpoint": lastHeight,
		"path":            cpPath,
	})
}

// handleLoadCheckpoints downloads the checkpoints file if missing, loads it, and
// registers the checkpoints as trusted so blocks at or below the max checkpoint
// height can skip PoW re-verification during sync.
// POST /api/checkpoints/load
func (s *APIServer) handleLoadCheckpoints(w http.ResponseWriter, r *http.Request) {
	cpPath := checkpointsPath(s.dataDir)

	downloaded, err := ensureCheckpointsFile(cpPath)
	if err != nil {
		writeInternal(w, r, http.StatusBadGateway, "failed to fetch checkpoints", err)
		return
	}

	cps, _, maxH, err := loadCheckpointsFile(cpPath)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to load checkpoints", err)
		return
	}

	chainHeight := s.daemon.Chain().Height()

	if len(cps) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"downloaded":   downloaded,
			"loaded":       0,
			"max_height":   0,
			"chain_height": chainHeight,
			"fast_sync":    false,
			"message":      "no checkpoints found",
		})
		return
	}

	s.daemon.Chain().SetTrustedCheckpoints(cps, maxH)

	writeJSON(w, http.StatusOK, map[string]any{
		"downloaded":   downloaded,
		"loaded":       len(cps),
		"max_height":   maxH,
		"chain_height": chainHeight,
		"fast_sync":    chainHeight < maxH,
	})
}
