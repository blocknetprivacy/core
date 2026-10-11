package main

import (
	"strings"
	"testing"
)

func TestEnforceReorgFinalityLocked_RejectsDeepReorg(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	defer chain.mu.Unlock()

	genesis := chain.getBlockByHeightLocked(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	buildChild := func(parent *Block, height uint64) ([32]byte, *Block) {
		block := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     height,
				PrevHash:   parent.Hash(),
				Timestamp:  parent.Header.Timestamp + BlockIntervalSec,
				Difficulty: MinDifficulty,
			},
		}
		hash := block.Hash()
		chain.blocks[hash] = block
		chain.workAt[hash] = height + 1
		return hash, block
	}

	// Build main chain to height 105 (finalized boundary = 5).
	mainByHeight := make(map[uint64][32]byte)
	mainByHeight[0] = genesis.Hash()
	parent := genesis
	var mainTip [32]byte
	for h := uint64(1); h <= 105; h++ {
		hash, block := buildChild(parent, h)
		chain.byHeight[h] = hash
		mainByHeight[h] = hash
		parent = block
		mainTip = hash
	}
	chain.bestHash = mainTip
	chain.height = 105
	chain.totalWork = 106
	chain.publishTipLocked()

	// Build heavier fork from height 1, which crosses finalized boundary.
	forkParent := chain.blocks[mainByHeight[1]]
	var forkTip [32]byte
	for h := uint64(2); h <= 107; h++ {
		block := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   forkParent.Hash(),
				// Keep increasing time but skew by +1s so hashes diverge
				// from the main-chain lineage at the fork point.
				Timestamp:  forkParent.Header.Timestamp + BlockIntervalSec + 1,
				Difficulty: MinDifficulty,
			},
		}
		hash := block.Hash()
		chain.blocks[hash] = block
		chain.workAt[hash] = h + 1
		forkParent = block
		forkTip = hash
	}

	err := chain.enforceReorgFinalityLocked(forkTip)
	if err == nil {
		t.Fatal("expected deep finalized reorg to be rejected")
	}
	if !strings.Contains(err.Error(), "reorg crosses finalized boundary") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnforceReorgFinalityLocked_AllowsShallowReorg(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	defer chain.mu.Unlock()

	genesis := chain.getBlockByHeightLocked(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	buildChild := func(parent *Block, height uint64) ([32]byte, *Block) {
		block := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     height,
				PrevHash:   parent.Hash(),
				Timestamp:  parent.Header.Timestamp + BlockIntervalSec,
				Difficulty: MinDifficulty,
			},
		}
		hash := block.Hash()
		chain.blocks[hash] = block
		chain.workAt[hash] = height + 1
		return hash, block
	}

	// Main chain height 105 (boundary = 5).
	mainByHeight := make(map[uint64][32]byte)
	mainByHeight[0] = genesis.Hash()
	parent := genesis
	var mainTip [32]byte
	for h := uint64(1); h <= 105; h++ {
		hash, block := buildChild(parent, h)
		chain.byHeight[h] = hash
		mainByHeight[h] = hash
		parent = block
		mainTip = hash
	}
	chain.bestHash = mainTip
	chain.height = 105
	chain.totalWork = 106
	chain.publishTipLocked()

	// Fork from height 10 (within reorg depth window), should be allowed.
	forkParent := chain.blocks[mainByHeight[10]]
	var forkTip [32]byte
	for h := uint64(11); h <= 107; h++ {
		block := &Block{
			Header: BlockHeader{
				Version:    1,
				Height:     h,
				PrevHash:   forkParent.Hash(),
				// Diverge from main chain while staying monotonically increasing.
				Timestamp:  forkParent.Header.Timestamp + BlockIntervalSec + 1,
				Difficulty: MinDifficulty,
			},
		}
		hash := block.Hash()
		chain.blocks[hash] = block
		chain.workAt[hash] = h + 1
		forkParent = block
		forkTip = hash
	}

	if err := chain.enforceReorgFinalityLocked(forkTip); err != nil {
		t.Fatalf("expected shallow reorg to be allowed, got: %v", err)
	}
}
