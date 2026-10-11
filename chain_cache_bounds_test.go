package main

import (
	"testing"

	"golang.org/x/crypto/sha3"
)

func TestChainCache_EnforcesCapAndRetainsHotNonPinnedEntries(t *testing.T) {
	// This is a "real storage, real chain" cache bound test. It does not use PoW.
	// Goal:
	// - exercise GetBlock() storage fallback
	// - exercise cumulativeWorkAtLocked() and getAncestorPath() (which also touch caches)
	// - ensure cacheTrimLocked() enforces a hard cap
	// - ensure "hot" non-pinned blocks remain cached under adversarial lookup pressure

	dataDir := t.TempDir()

	chain, err := NewChainWithCacheCap(dataDir, 164)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer func() {
		if err := chain.Close(); err != nil {
			t.Fatalf("failed to close chain: %v", err)
		}
	}()

	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	tipHash, tipHeight, tipWork, found := st.GetTip()
	if !found || tipHeight != 0 {
		t.Fatalf("expected genesis tip at height 0 (found=%v height=%d)", found, tipHeight)
	}

	// Build a long main chain in storage so we have a large lookup surface.
	const mainTipHeight = uint64(400)
	prevHash := tipHash
	prevWork := tipWork
	prevTimestamp := GenesisTimestamp

	for h := uint64(1); h <= mainTipHeight; h++ {
		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   prevHash,
				MerkleRoot: sha3.Sum256([]byte{0xA0, byte(h)}),
				Timestamp:  prevTimestamp + BlockIntervalSec,
				Difficulty: MinDifficulty,
				Nonce:      0,
			},
		}
		hash := b.Hash()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatalf("failed to compute cumulative work at height %d: %v", h, err)
		}
		if err := st.CommitBlock(&BlockCommit{
			Block:     b,
			Height:    h,
			Hash:      hash,
			Work:      work,
			IsMainTip: true,
		}); err != nil {
			t.Fatalf("failed to commit main-chain block at height %d: %v", h, err)
		}
		prevHash = hash
		prevWork = work
		prevTimestamp = b.Header.Timestamp
	}

	// Restart so we start from the real loadFromStorage() behavior (recent-window hydration).
	if err := chain.Close(); err != nil {
		t.Fatalf("failed to close chain before restart: %v", err)
	}
	chain = nil

	chain, err = NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to restart chain: %v", err)
	}

	if got := chain.Height(); got != mainTipHeight {
		t.Fatalf("unexpected restarted height: got %d, want %d", got, mainTipHeight)
	}
	cacheCap := chain.cacheCap
	protectedBack := chainProtectedBack()
	pinnedStart := mainTipHeight - protectedBack
	if pinnedStart == 0 {
		t.Fatalf("test invariant failed: expected pinnedStart>0")
	}

	// Pick "hot" entries that are NOT pinned (so the test isn't trivial).
	hotHeights := []uint64{10, 50, 200}
	var hotHashes [][32]byte
	for _, h := range hotHeights {
		hash, ok := chain.Storage().GetBlockHashByHeight(h)
		if !ok {
			t.Fatalf("missing main-chain hash at hot height %d", h)
		}
		hotHashes = append(hotHashes, hash)
	}

	touchHot := func() {
		for _, hh := range hotHashes {
			if b := chain.GetBlock(hh); b == nil {
				t.Fatalf("expected hot block %x to exist", hh[:8])
			}
		}
	}

	// Initial touch so they're in the cache.
	touchHot()

	// Pressure the caches with many distinct lookups + work/path touches.
	for h := uint64(1); h < pinnedStart; h++ {
		// Skip the hot heights; we manage them explicitly.
		skip := false
		for _, hotH := range hotHeights {
			if h == hotH {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		hash, ok := chain.Storage().GetBlockHashByHeight(h)
		if !ok {
			t.Fatalf("missing main-chain hash at height %d", h)
		}
		if b := chain.GetBlock(hash); b == nil {
			t.Fatalf("expected block at height %d to exist", h)
		}

		chain.mu.Lock()
		if _, err := chain.cumulativeWorkAtLocked(hash); err != nil {
			chain.mu.Unlock()
			t.Fatalf("cumulativeWorkAtLocked failed at height %d: %v", h, err)
		}
		if h%25 == 0 {
			if path := chain.getAncestorPath(hash); path == nil {
				chain.mu.Unlock()
				t.Fatalf("getAncestorPath returned nil at height %d", h)
			}
		}

		// Hard bound invariant: blocks map is the thing eviction targets.
		if len(chain.blocks) > cacheCap {
			chain.mu.Unlock()
			t.Fatalf("cache cap exceeded: len(blocks)=%d cap=%d", len(chain.blocks), cacheCap)
		}
		// LRU/index should be consistent with blocks cache usage.
		if chain.cacheLRU.Len() != len(chain.cacheIndex) {
			chain.mu.Unlock()
			t.Fatalf("cache LRU/index mismatch: lru=%d index=%d", chain.cacheLRU.Len(), len(chain.cacheIndex))
		}
		if len(chain.cacheIndex) > cacheCap {
			chain.mu.Unlock()
			t.Fatalf("cache index exceeded cap: index=%d cap=%d", len(chain.cacheIndex), cacheCap)
		}
		chain.mu.Unlock()

		// Include "miss" lookups: should not mutate tip or inflate caches.
		if h%17 == 0 {
			var miss [32]byte
			miss[0] = 0xEE
			miss[1] = byte(h)
			_ = chain.GetBlock(miss)
		}

		// Keep the hot blocks "hot" under pressure.
		if h%7 == 0 {
			touchHot()
		}
	}

	chain.mu.Lock()
	for _, hh := range hotHashes {
		if _, ok := chain.cacheIndex[hh]; !ok {
			chain.mu.Unlock()
			t.Fatalf("expected hot block %x to remain cached (non-pinned)", hh[:8])
		}
		if chain.blocks[hh] == nil {
			chain.mu.Unlock()
			t.Fatalf("expected hot block %x to remain in blocks cache map", hh[:8])
		}
	}
	// Also ensure the pinned tip is present (pinned blocks must not be evicted).
	if _, ok := chain.cacheIndex[chain.bestHash]; !ok {
		chain.mu.Unlock()
		t.Fatalf("expected pinned tip %x to remain cached", chain.bestHash[:8])
	}
	if chain.blocks[chain.bestHash] == nil {
		chain.mu.Unlock()
		t.Fatalf("expected pinned tip %x to remain in blocks cache map", chain.bestHash[:8])
	}
	chain.mu.Unlock()
}

func TestCumulativeWorkAtColdCacheMatchesStoredWork(t *testing.T) {
	dataDir := t.TempDir()
	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	prevHash, _, prevWork, _ := st.GetTip()
	genesisWork := prevWork
	const tipHeight = uint64(300)
	for h := uint64(1); h <= tipHeight; h++ {
		b := &Block{Header: BlockHeader{
			Version:    1,
			Height:     h,
			PrevHash:   prevHash,
			MerkleRoot: sha3.Sum256([]byte{0xB0, byte(h), byte(h >> 8)}),
			Timestamp:  GenesisTimestamp + int64(h)*BlockIntervalSec,
			Difficulty: MinDifficulty + h,
		}}
		hash := b.Hash()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CommitBlock(&BlockCommit{Block: b, Height: h, Hash: hash, Work: work, IsMainTip: true}); err != nil {
			t.Fatalf("commit height %d: %v", h, err)
		}
		prevHash, prevWork = hash, work
	}
	if err := chain.Close(); err != nil {
		t.Fatal(err)
	}
	chain, err = NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to restart chain: %v", err)
	}
	defer chain.Close()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	clear(chain.workAt)
	got, err := chain.cumulativeWorkAtLocked(chain.bestHash)
	if err != nil {
		t.Fatal(err)
	}
	if got != chain.totalWork || len(chain.workAt) != 0 {
		t.Fatalf("tip work: got %d (cached %d), want %d with no walk", got, len(chain.workAt), chain.totalWork)
	}

	for _, h := range []uint64{1, 2, 150, tipHeight - 1} {
		clear(chain.workAt)
		hash, ok := chain.Storage().GetBlockHashByHeight(h)
		if !ok {
			t.Fatalf("missing hash at height %d", h)
		}
		want := genesisWork
		for i := uint64(1); i <= h; i++ {
			want += MinDifficulty + i
		}
		got, err := chain.cumulativeWorkAtLocked(hash)
		if err != nil {
			t.Fatalf("height %d: %v", h, err)
		}
		if got != want {
			t.Fatalf("height %d: got work %d, want %d", h, got, want)
		}
	}
}
