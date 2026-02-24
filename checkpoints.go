package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	checkpointsFilename = "checkpoints.dat"

	// DefaultCheckpointsURL is where nodes fetch checkpoints when not in --full-sync mode.
	// This file is intended to be committed to the repo (append-only).
	DefaultCheckpointsURL = "https://raw.githubusercontent.com/blocknetprivacy/checkpoints/mainnet/checkpoints.dat"

	// Keep downloads bounded in case of a misconfigured URL.
	maxCheckpointsDownloadBytes = 32 << 20 // 32 MiB
)

func checkpointsPath(dataDir string) string {
	return filepath.Join(dataDir, checkpointsFilename)
}

func checkpointsURL() string {
	if v := strings.TrimSpace(os.Getenv("BLOCKNET_CHECKPOINTS_URL")); v != "" {
		return v
	}
	return DefaultCheckpointsURL
}

func ensureCheckpointsFile(path string) (downloaded bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}

	url := strings.TrimSpace(checkpointsURL())
	if url == "" {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("failed to create checkpoints dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("failed to build checkpoints request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to download checkpoints: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("checkpoints download HTTP %d", resp.StatusCode)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("failed to open temp checkpoints file: %w", err)
	}

	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxCheckpointsDownloadBytes))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("failed to write checkpoints: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("failed to close checkpoints file: %w", closeErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("failed to finalize checkpoints file: %w", err)
	}

	return true, nil
}

func loadCheckpointsFile(path string) (checkpoints map[uint64][32]byte, heights []uint64, maxHeight uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()

	checkpoints = make(map[uint64][32]byte)

	sc := bufio.NewScanner(f)
	// Allow long lines defensively.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		h, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil || h == 0 {
			continue
		}
		hashHex := strings.TrimSpace(parts[1])
		hashHex = strings.TrimPrefix(hashHex, "0x")
		if len(hashHex) != 64 {
			continue
		}
		b, err := hex.DecodeString(hashHex)
		if err != nil || len(b) != 32 {
			continue
		}
		var hash [32]byte
		copy(hash[:], b)

		if _, exists := checkpoints[h]; !exists {
			heights = append(heights, h)
		}
		checkpoints[h] = hash
		if h > maxHeight {
			maxHeight = h
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, 0, err
	}

	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	return checkpoints, heights, maxHeight, nil
}
