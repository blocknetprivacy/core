package main

import (
	"sync"
	"testing"
)

// GetBlockByHeight releases c.mu before falling back to storage, so its unlock
// path is no longer a single deferred call. Hammer both the cached and the
// evicted branch against a concurrent writer: a missed unlock deadlocks, a
// double unlock panics, and -race catches an unguarded map read.
func TestGetBlockByHeightIsCorrectWhenCacheMisses(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	const topHeight = 6
	prev := chain.GetBlockByHeight(0)
	if prev == nil {
		t.Fatal("expected genesis block")
	}
	for height := uint64(1); height <= topHeight; height++ {
		pub, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate key at height %d: %v", height, err)
		}
		block := makeOutputOnlyTestBlock(height, prev.Hash(), prev.Header.Timestamp+BlockIntervalSec, []TxOutput{
			{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
		})
		commitMainChainBlockForTest(t, chain, storage, block, height+1)
		prev = block
	}

	// Heights 1..3 now resolve only through storage; 4..6 stay cached.
	for height := uint64(1); height <= 3; height++ {
		mustEvictBlockFromChainCacheForTest(t, chain, height)
	}

	var wg sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				for height := uint64(0); height <= topHeight; height++ {
					block := chain.GetBlockByHeight(height)
					if block == nil {
						t.Errorf("GetBlockByHeight(%d) returned nil", height)
						return
					}
					if block.Header.Height != height {
						t.Errorf("GetBlockByHeight(%d) returned height %d", height, block.Header.Height)
						return
					}
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; round < 200; round++ {
			chain.mu.Lock()
			chain.mu.Unlock()
		}
	}()

	wg.Wait()

	if got := chain.Height(); got != topHeight {
		t.Fatalf("chain height = %d, want %d", got, topHeight)
	}
}
