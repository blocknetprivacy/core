package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// handleSaveCheckpoints appends checkpoints for known blocks (every 100 blocks)
// to the on-disk checkpoints file, resuming after the last one already written.
// POST /api/checkpoints/save
func (s *APIServer) handleSaveCheckpoints(w http.ResponseWriter, r *http.Request) {
	chain := s.daemon.Chain()
	height := chain.Height()
	cpPath := checkpointsPath(s.dataDir)

	if height == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"written":         0,
			"chain_height":    0,
			"last_checkpoint": 0,
			"path":            cpPath,
			"message":         "chain is empty, nothing to save",
		})
		return
	}

	var lastWritten uint64
	if _, _, maxH, err := loadCheckpointsFile(cpPath); err == nil && maxH > 0 {
		lastWritten = maxH
	}

	// Next checkpoint height is the first multiple of 100 above the last written.
	start := lastWritten + 100 - (lastWritten % 100)
	if lastWritten == 0 {
		start = 100
	}

	if start > height {
		writeJSON(w, http.StatusOK, map[string]any{
			"written":         0,
			"chain_height":    height,
			"last_checkpoint": lastWritten,
			"path":            cpPath,
			"message":         "already up to date",
		})
		return
	}

	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to create data dir", err)
		return
	}
	f, err := os.OpenFile(cpPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		writeInternal(w, r, http.StatusInternalServerError, "failed to open checkpoints file", err)
		return
	}
	defer f.Close()

	written := 0
	lastHeight := lastWritten
	for h := start; h <= height; h += 100 {
		block := chain.GetBlockByHeight(h)
		if block == nil {
			break
		}
		hash := block.Hash()
		line := fmt.Sprintf("%d:%s\n", h, strings.ToUpper(hex.EncodeToString(hash[:])))
		if _, err := io.WriteString(f, line); err != nil {
			writeInternal(w, r, http.StatusInternalServerError, "failed to write checkpoint", err)
			return
		}
		written++
		lastHeight = h
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
