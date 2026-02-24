package main

import (
	"testing"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/sha3"
)

func TestReorg_NearFinalityBoundary_AfterRestartWithPartialHydration(t *testing.T) {
	// This test is about startup hydration + deep reorg correctness, not PoW.
	// We build a persistent chain state through production storage commit paths,
	// restart the chain (so only the recent window is hydrated into in-memory
	// indexes), then trigger a reorg whose fork point is exactly at the finalized
	// boundary (height - MaxReorgDepth). Historically this class of scenario could
	// break if reorg logic assumed deeper byHeight/workAt/maps were always loaded.

	dataDir := t.TempDir()

	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	mustClose := func(c *Chain) {
		t.Helper()
		if err := c.Close(); err != nil {
			t.Fatalf("failed to close chain: %v", err)
		}
	}
	defer mustClose(chain)

	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	genesisHash, genesisHeight, genesisWork, found := st.GetTip()
	if !found || genesisHeight != 0 {
		t.Fatalf("expected genesis tip at height 0 (found=%v height=%d)", found, genesisHeight)
	}

	// Pick a height where loadFromStorage will NOT hydrate the full chain, but
	// WILL hydrate the entire MaxReorgDepth window.
	//
	// preloadBack = max(LWMAWindow+10, MaxReorgDepth) == MaxReorgDepth in this codebase.
	mainTipHeight := uint64(MaxReorgDepth + 5)

	prevHash := genesisHash
	prevWork := genesisWork
	prevTimestamp := GenesisTimestamp

	for h := uint64(1); h <= mainTipHeight; h++ {
		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   prevHash,
				MerkleRoot: sha3.Sum256([]byte{byte(h)}), // deterministic, doesn't matter for this test
				Timestamp:  prevTimestamp + BlockIntervalSec,
				Difficulty: MinDifficulty,
				Nonce:      0, // PoW is intentionally not part of this test
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

	// Restart: this is the part that used to be risky (only a window is hydrated).
	mustClose(chain)
	chain = nil

	restarted, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to restart chain: %v", err)
	}
	defer mustClose(restarted)

	if got := restarted.Height(); got != mainTipHeight {
		t.Fatalf("unexpected restarted height: got %d, want %d", got, mainTipHeight)
	}

	// Confirm the "partial hydration" property: byHeight is not populated from genesis.
	// (The chain should still be able to serve old blocks from storage, but the
	// in-memory height index should be a recent window.)
	preloadBack := max(uint64(LWMAWindow+10), uint64(MaxReorgDepth))
	startHeight := uint64(0)
	if mainTipHeight > preloadBack {
		startHeight = mainTipHeight - preloadBack
	}
	if startHeight == 0 {
		t.Fatalf("test invariant failed: expected partial hydration startHeight>0 for tipHeight=%d", mainTipHeight)
	}
	if _, ok := restarted.byHeight[0]; ok {
		t.Fatalf("expected restarted byHeight to not be hydrated from genesis (startHeight=%d)", startHeight)
	}

	// Build a competing branch that forks exactly at the finalized boundary.
	// Boundary height = tip - MaxReorgDepth, which should be within the hydrated window.
	forkHeight := mainTipHeight - uint64(MaxReorgDepth)
	if forkHeight != startHeight {
		// With current preloadBack formula, these should match for tipHeight=MaxReorgDepth+5.
		t.Fatalf("unexpected fork height relationship: forkHeight=%d startHeight=%d", forkHeight, startHeight)
	}

	forkHash, ok := restarted.Storage().GetBlockHashByHeight(forkHeight)
	if !ok {
		t.Fatalf("missing main-chain hash at forkHeight=%d", forkHeight)
	}
	forkBlock, err := restarted.Storage().GetBlock(forkHash)
	if err != nil || forkBlock == nil {
		t.Fatalf("failed to load fork block at height %d: block=%v err=%v", forkHeight, forkBlock, err)
	}

	// Create a longer fork so it becomes heavier (same MinDifficulty per block).
	newTipHeight := mainTipHeight + 2
	parentHash := forkHash
	parentTimestamp := forkBlock.Header.Timestamp

	var newTipHash [32]byte
	for h := forkHeight + 1; h <= newTipHeight; h++ {
		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   parentHash,
				MerkleRoot: sha3.Sum256([]byte{0xF0, byte(h)}),
				Timestamp:  parentTimestamp + BlockIntervalSec + 1, // ensure strictly increasing
				Difficulty: MinDifficulty,
				Nonce:      0,
			},
		}
		if err := restarted.Storage().SaveBlock(b); err != nil {
			t.Fatalf("failed to save fork block at height %d: %v", h, err)
		}
		parentHash = b.Hash()
		parentTimestamp = b.Header.Timestamp
		newTipHash = parentHash
	}

	// Trigger the reorg using the real reorganize code path while holding the chain lock,
	// mirroring how ProcessBlock calls into it.
	restarted.mu.Lock()
	err = restarted.reorganizeTo(newTipHash)
	restarted.mu.Unlock()
	if err != nil {
		t.Fatalf("reorg failed: %v", err)
	}

	if got := restarted.Height(); got != newTipHeight {
		t.Fatalf("unexpected post-reorg height: got %d, want %d", got, newTipHeight)
	}
	if got := restarted.BestHash(); got != newTipHash {
		t.Fatalf("unexpected post-reorg tip hash")
	}

	tipHash, tipHeight, _, found := restarted.Storage().GetTip()
	if !found {
		t.Fatalf("expected storage tip after reorg")
	}
	if tipHeight != newTipHeight || tipHash != newTipHash {
		t.Fatalf("unexpected storage tip after reorg: height=%d hash=%x", tipHeight, tipHash[:8])
	}
}

func TestReorg_MissingDeepAncestorOutsideForkPath_DoesNotBlockNearTipReorg(t *testing.T) {
	// Simulate a locally-corrupted ancient block while testing a near-tip fork.
	// Reorg should still be able to find a common ancestor and switch tips
	// without requiring a full tip->genesis path.
	dataDir := t.TempDir()

	chain, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	mustClose := func(c *Chain) {
		t.Helper()
		if err := c.Close(); err != nil {
			t.Fatalf("failed to close chain: %v", err)
		}
	}
	defer mustClose(chain)

	mustAddGenesisBlock(t, chain)

	st := chain.Storage()
	genesisHash, _, genesisWork, found := st.GetTip()
	if !found {
		t.Fatal("expected genesis tip to exist")
	}

	mainTipHeight := uint64(MaxReorgDepth + 20)
	prevHash := genesisHash
	prevWork := genesisWork
	prevTimestamp := GenesisTimestamp

	for h := uint64(1); h <= mainTipHeight; h++ {
		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   prevHash,
				MerkleRoot: sha3.Sum256([]byte{byte(h)}),
				Timestamp:  prevTimestamp + BlockIntervalSec,
				Difficulty: MinDifficulty,
				Nonce:      0,
			},
		}

		hash := b.Hash()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatalf("failed to compute work at height %d: %v", h, err)
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

	mustClose(chain)
	chain = nil

	restarted, err := NewChain(dataDir)
	if err != nil {
		t.Fatalf("failed to restart chain: %v", err)
	}
	defer mustClose(restarted)

	if got := restarted.Height(); got != mainTipHeight {
		t.Fatalf("unexpected restarted height: got %d want %d", got, mainTipHeight)
	}

	missingHeight := uint64(1)
	missingHash, ok := restarted.Storage().GetBlockHashByHeight(missingHeight)
	if !ok {
		t.Fatalf("missing main-chain hash at height %d", missingHeight)
	}
	if err := restarted.Storage().db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlocks).Delete(missingHash[:])
	}); err != nil {
		t.Fatalf("failed to delete deep ancestor block: %v", err)
	}
	if b, err := restarted.Storage().GetBlock(missingHash); err != nil {
		t.Fatalf("failed to read deleted block hash: %v", err)
	} else if b != nil {
		t.Fatalf("expected block %x at height %d to be deleted", missingHash[:8], missingHeight)
	}

	forkHeight := mainTipHeight - 2
	forkHash, ok := restarted.Storage().GetBlockHashByHeight(forkHeight)
	if !ok {
		t.Fatalf("missing fork hash at height %d", forkHeight)
	}
	forkBlock, err := restarted.Storage().GetBlock(forkHash)
	if err != nil || forkBlock == nil {
		t.Fatalf("failed to load fork block at height %d: block=%v err=%v", forkHeight, forkBlock, err)
	}

	parentHash := forkHash
	parentTimestamp := forkBlock.Header.Timestamp
	newTipHeight := mainTipHeight + 1
	var newTipHash [32]byte

	for h := forkHeight + 1; h <= newTipHeight; h++ {
		b := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   parentHash,
				MerkleRoot: sha3.Sum256([]byte{0xD0, byte(h)}),
				Timestamp:  parentTimestamp + BlockIntervalSec + 1,
				Difficulty: MinDifficulty,
				Nonce:      0,
			},
		}
		if err := restarted.Storage().SaveBlock(b); err != nil {
			t.Fatalf("failed to save fork block at height %d: %v", h, err)
		}
		parentHash = b.Hash()
		parentTimestamp = b.Header.Timestamp
		newTipHash = parentHash
	}

	restarted.mu.Lock()
	err = restarted.reorganizeTo(newTipHash)
	restarted.mu.Unlock()
	if err != nil {
		t.Fatalf("reorg failed: %v", err)
	}

	if got := restarted.Height(); got != newTipHeight {
		t.Fatalf("unexpected post-reorg height: got %d want %d", got, newTipHeight)
	}
	if got := restarted.BestHash(); got != newTipHash {
		t.Fatalf("unexpected post-reorg tip hash")
	}
}
