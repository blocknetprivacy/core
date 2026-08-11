package main

import "testing"

// TruncateToHeight now collects everything before it writes. A failure to reach
// the target block must leave the chain untouched: the old per-height loop
// deleted from storage first, so an error left a half-truncated chain behind.
func TestTruncateToHeightIsAtomicOnFailure(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	const top = uint64(5)
	const keep = uint64(3)
	mustBuildChainToHeight(t, chain, storage, top)

	// Make the truncate target unreachable: drop its height entry and evict it
	// from the cache, so getBlockByHeightLocked returns nil for it.
	mustEvictBlockFromChainCacheForTest(t, chain, keep)
	mustDeleteMainChainHeightForTest(t, storage, keep)

	if err := chain.TruncateToHeight(keep); err == nil {
		t.Fatal("expected TruncateToHeight to fail when the target block is unreachable")
	}

	for h := keep + 1; h <= top; h++ {
		if _, found := storage.GetBlockHashByHeight(h); !found {
			t.Fatalf("height %d was removed from storage by a truncate that failed", h)
		}
	}

	if _, tipHeight, _, found := storage.GetTip(); !found || tipHeight != top {
		t.Fatalf("stored tip height = %d (found %t), want %d unchanged", tipHeight, found, top)
	}
}

// The successful path must drop heights, key images, and index entries together.
func TestTruncateToHeightRemovesStorageState(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	const top = uint64(5)
	const keep = uint64(2)
	mustBuildChainToHeight(t, chain, storage, top)

	removed := chain.GetBlockByHeight(top)
	if removed == nil {
		t.Fatalf("expected block at height %d", top)
	}
	removedTxID, err := removed.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash removed tx: %v", err)
	}

	if err := chain.TruncateToHeight(keep); err != nil {
		t.Fatalf("truncate failed: %v", err)
	}

	if got := chain.Height(); got != keep {
		t.Fatalf("chain height = %d, want %d", got, keep)
	}
	for h := keep + 1; h <= top; h++ {
		if _, found := storage.GetBlockHashByHeight(h); found {
			t.Fatalf("height %d still in the main-chain index after truncate", h)
		}
	}
	if _, found := storage.GetTxHeight(removedTxID); found {
		t.Fatal("truncated block's transaction is still in the index")
	}
	if _, tipHeight, _, found := storage.GetTip(); !found || tipHeight != keep {
		t.Fatalf("stored tip height = %d (found %t), want %d", tipHeight, found, keep)
	}
}
