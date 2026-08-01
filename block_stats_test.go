package main

import (
	"math"
	"testing"
)

func TestRecentBlockStatsUsesObservedIntervalWork(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	prev := genesis
	timestamp := genesis.Header.Timestamp
	var totalWork uint64
	for height, difficulty := range []uint64{100, 300, 500} {
		timestamp += int64((height + 1) * 10)
		block := &Block{Header: BlockHeader{
			Version:    1,
			Height:     uint64(height + 1),
			PrevHash:   prev.Hash(),
			Timestamp:  timestamp,
			Difficulty: difficulty,
		}}
		if err := storage.SaveBlock(block); err != nil {
			t.Fatalf("save block %d: %v", height+1, err)
		}
		if err := storage.SetMainChainBlock(uint64(height+1), block.Hash()); err != nil {
			t.Fatalf("index block %d: %v", height+1, err)
		}
		prev = block
		totalWork += difficulty
	}

	chain.mu.Lock()
	chain.height = 3
	chain.bestHash = prev.Hash()
	chain.mu.Unlock()

	hashrate, avgBlockTime := chain.RecentBlockStats(3)
	if want := 20.0; math.Abs(avgBlockTime-want) > 1e-9 {
		t.Fatalf("average block time = %v, want %v", avgBlockTime, want)
	}
	if want := float64(totalWork) / 60.0; math.Abs(hashrate-want) > 1e-9 {
		t.Fatalf("network hashrate = %v, want observed work/time %v", hashrate, want)
	}
}
