package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const checkpointInterval uint64 = 100

func (d *Daemon) maybeSaveCheckpoints() {
	if !d.saveCheckpoints.Load() || d.checkpointsFile == "" {
		return
	}
	if _, _, _, err := d.saveCanonicalCheckpoints(d.checkpointsFile, false); err != nil {
		log.Printf("Warning: failed to publish checkpoints: %v", err)
	}
}

// saveCanonicalCheckpoints serializes automatic and API writers. It snapshots
// canonical hashes under the chain read lock, then publishes via atomic rename
// outside that lock so HTTP readers never observe a partial file. No daemon
// mutex may be held by callers. Writer progress advances only after success.
func (d *Daemon) saveCanonicalCheckpoints(path string, force bool) (written int, height, last uint64, err error) {
	d.checkpointMu.Lock()
	defer d.checkpointMu.Unlock()

	c := d.chain
	c.mu.RLock()
	height = c.height
	// A checkpoint producer still catching up must retain its downloaded anchors.
	if !force && height < c.fastSyncUntilHeight {
		c.mu.RUnlock()
		return 0, height, d.lastCheckpointWritten, nil
	}
	if height >= MaxReorgDepth {
		last = (height - MaxReorgDepth) / checkpointInterval * checkpointInterval
	}
	if !force && d.checkpointsInitialized && last == d.lastCheckpointWritten {
		c.mu.RUnlock()
		return 0, height, last, nil
	}
	var body bytes.Buffer
	for h := checkpointInterval; h <= last; h += checkpointInterval {
		block := c.getBlockByHeightLocked(h)
		if block == nil || block.Header.Height != h {
			c.mu.RUnlock()
			return 0, height, 0, fmt.Errorf("missing canonical block at checkpoint height %d", h)
		}
		fmt.Fprintf(&body, "%d:%X\n", h, block.Hash())
		written++
	}
	c.mu.RUnlock()

	if err := replaceCheckpointsFile(path, body.Bytes()); err != nil {
		return 0, height, 0, err
	}
	if path == d.checkpointsFile {
		d.lastCheckpointWritten = last
		d.checkpointsInitialized = true
	}
	return written, height, last, nil
}

func replaceCheckpointsFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".checkpoints-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
