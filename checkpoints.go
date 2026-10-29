package main

import (
	"bufio"
	"bytes"
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
	// The file is served from a web host and contains finalized canonical blocks.
	DefaultCheckpointsURL = "https://bntpool.com/checkpoints.dat"

	// Keep downloads bounded in case of a misconfigured URL.
	maxCheckpointsDownloadBytes = 32 << 20 // 32 MiB

	checkpointRefreshInterval = 24 * time.Hour
)

func checkpointsPath(dataDir string) string {
	return filepath.Join(dataDir, checkpointsFilename)
}

var checkpointsSource = DefaultCheckpointsURL

func checkpointsURL() string {
	return checkpointsSource
}

type checkpointsRefresh struct {
	checkpoints map[uint64][32]byte
	maxHeight   uint64
	replaced    bool
}

func refreshCheckpointsFile(ctx context.Context, path string) (checkpointsRefresh, error) {
	var res checkpointsRefresh

	url := strings.TrimSpace(checkpointsURL())
	if url == "" {
		return res, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return res, fmt.Errorf("failed to build checkpoints request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return res, fmt.Errorf("failed to download checkpoints: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("checkpoints download HTTP %d", resp.StatusCode)
	}
	remoteData, err := io.ReadAll(io.LimitReader(resp.Body, maxCheckpointsDownloadBytes))
	if err != nil {
		return res, fmt.Errorf("failed to read checkpoints: %w", err)
	}
	remote, _, remoteMax, err := parseCheckpoints(bytes.NewReader(remoteData))
	if err != nil || len(remote) == 0 {
		return res, err
	}

	localData, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, err
	}
	local, _, _, _ := parseCheckpoints(bytes.NewReader(localData))
	stale := false
	for h, hash := range remote {
		if local[h] != hash {
			stale = true
			break
		}
	}
	if !stale {
		return res, nil
	}

	if localData != nil {
		if err := os.WriteFile(path+".bak", localData, 0o644); err != nil {
			return res, fmt.Errorf("failed to back up checkpoints: %w", err)
		}
	}
	if err := replaceCheckpointsFile(path, remoteData); err != nil {
		return res, fmt.Errorf("failed to publish checkpoints: %w", err)
	}

	return checkpointsRefresh{checkpoints: remote, maxHeight: remoteMax, replaced: true}, nil
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

func finalizedCheckpointHeight(tip uint64) uint64 {
	if tip < MaxReorgDepth {
		return 0
	}
	final := tip - MaxReorgDepth
	return final - final%100
}

func loadCheckpointsFile(path string) (checkpoints map[uint64][32]byte, heights []uint64, maxHeight uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	return parseCheckpoints(f)
}

func parseCheckpoints(r io.Reader) (checkpoints map[uint64][32]byte, heights []uint64, maxHeight uint64, err error) {
	checkpoints = make(map[uint64][32]byte)

	sc := bufio.NewScanner(r)
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
