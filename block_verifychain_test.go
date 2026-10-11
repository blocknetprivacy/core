package main

import (
	"testing"

	"golang.org/x/crypto/sha3"
)

func TestVerifyChain_DifficultyDerivedFromParentWindow(t *testing.T) {
	// Reproduces the historical "first mismatch at height 60" bug:
	// - consensus keeps MinDifficulty for blocks whose parent height < LWMAWindow
	// - VerifyChain must compute expected difficulty using the parent window (h-1),
	//   not a window ending at the block being checked (h).

	dataDir := t.TempDir()

	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	mustClose := func() {
		t.Helper()
		if err := chain.Close(); err != nil {
			t.Fatalf("failed to close chain: %v", err)
		}
	}
	defer mustClose()

	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	prevHash, _, prevWork, found := st.GetTip()
	if !found {
		t.Fatalf("expected tip after genesis")
	}

	prevTimestamp := GenesisTimestamp

	// Build a chain where the first LWMA-window boundary would *want* to ramp
	// difficulty if the algorithm were applied at height 60, but consensus
	// doesn't apply LWMA until the parent height is >= LWMAWindow.
	//
	// Use 1-second block times (clamped by LWMAMinSolvetime=1), which would
	// yield a large LWMA-derived difficulty when the window becomes active.
	const tipHeight = uint64(LWMAWindow + 10) // cross the boundary + a little
	blocks := make([]*Block, tipHeight+1)
	genesis, err := st.GetBlock(prevHash)
	if err != nil || genesis == nil {
		t.Fatalf("failed to load genesis from storage: block=%v err=%v", genesis, err)
	}
	blocks[0] = genesis

	for h := uint64(1); h <= tipHeight; h++ {
		timestamp := prevTimestamp + 1 // fast, but valid (> parent / > median)

		var difficulty uint64
		parentHeight := h - 1
		if parentHeight < uint64(LWMAWindow) {
			difficulty = MinDifficulty
		} else {
			difficulty = computeLWMA(blocks, parentHeight)
		}

		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   prevHash,
				MerkleRoot: sha3.Sum256([]byte{byte(h)}), // deterministic filler
				Timestamp:  timestamp,
				Difficulty: difficulty,
				Nonce:      0, // PoW is intentionally not part of this test
			},
		}
		blocks[h] = b

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
			t.Fatalf("failed to commit block at height %d: %v", h, err)
		}

		prevHash = hash
		prevWork = work
		prevTimestamp = timestamp
	}

	// Restart to ensure VerifyChain is operating on realistic hydrated state.
	mustClose()

	restarted, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to restart chain: %v", err)
	}
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Fatalf("failed to close restarted chain: %v", err)
		}
	}()

	violations := restarted.VerifyChain()
	if len(violations) != 0 {
		t.Fatalf("expected no VerifyChain violations, got %d (first: height=%d msg=%q)", len(violations), violations[0].Height, violations[0].Message)
	}
}


func restartLWMATestChain(t *testing.T) (*Chain, []*Block, uint64) {
	t.Helper()
	dataDir := t.TempDir()
	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	prevHash, _, prevWork, _ := st.GetTip()
	genesis, err := st.GetBlock(prevHash)
	if err != nil || genesis == nil {
		t.Fatalf("failed to load genesis: %v", err)
	}
	const tipHeight = uint64(LWMAWindow + 10)
	blocks := []*Block{genesis}
	prevTimestamp := GenesisTimestamp
	for h := uint64(1); h <= tipHeight; h++ {
		difficulty := uint64(MinDifficulty)
		if h-1 >= uint64(LWMAWindow) {
			difficulty = computeLWMA(blocks, h-1)
		}
		b := &Block{Header: BlockHeader{
			Version:    1,
			Height:     h,
			PrevHash:   prevHash,
			MerkleRoot: sha3.Sum256([]byte{byte(h)}),
			Timestamp:  prevTimestamp + int64(h%7) + 1,
			Difficulty: difficulty,
		}}
		blocks = append(blocks, b)
		work, err := addCumulativeWork(prevWork, difficulty)
		if err != nil {
			t.Fatalf("work at height %d: %v", h, err)
		}
		if err := st.CommitBlock(&BlockCommit{Block: b, Height: h, Hash: b.Hash(), Work: work, IsMainTip: true}); err != nil {
			t.Fatalf("commit at height %d: %v", h, err)
		}
		prevHash, prevWork, prevTimestamp = b.Hash(), work, b.Header.Timestamp
	}
	if err := chain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	restarted, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })

	restarted.mu.Lock()
	for hash := range restarted.blocks {
		restarted.cacheForgetLocked(hash)
	}
	restarted.mu.Unlock()
	return restarted, blocks, tipHeight
}

func TestNextDifficultyReadsWindowFromStorageWhenCacheIsCold(t *testing.T) {
	chain, blocks, tipHeight := restartLWMATestChain(t)
	want := computeLWMA(blocks, tipHeight)
	if got := chain.NextDifficulty(); got != want {
		t.Fatalf("NextDifficulty with cold cache: got %d, want %d", got, want)
	}
}

func TestNextDifficultyWalksHashesWhenHeightIndexIsMissing(t *testing.T) {
	chain, blocks, tipHeight := restartLWMATestChain(t)
	missing := tipHeight - 5
	chain.mu.Lock()
	delete(chain.byHeight, missing)
	chain.mu.Unlock()
	if err := chain.Storage().RemoveMainChainBlock(missing); err != nil {
		t.Fatalf("remove height index: %v", err)
	}

	want := computeLWMA(blocks, tipHeight)
	if got := chain.NextDifficulty(); got != want {
		t.Fatalf("NextDifficulty with a missing height index: got %d, want %d", got, want)
	}
}
