package main

import (
	"strings"
	"testing"
)

func TestValidateBlockP2P_EnforcesDifficultyOnNonTipFork(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	mainTip := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
	}
	forkParent := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + 2*BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
	}

	mainTipHash := mainTip.Hash()
	forkParentHash := forkParent.Hash()

	chain.mu.Lock()
	chain.blocks[mainTipHash] = mainTip
	chain.blocks[forkParentHash] = forkParent
	chain.byHeight[1] = mainTipHash
	chain.bestHash = mainTipHash
	chain.height = 1
	chain.publishTipLocked()
	chain.mu.Unlock()

	forkChild := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     2,
			PrevHash:   forkParentHash, // not current best hash => non-tip fork path
			Timestamp:  forkParent.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty + 1, // wrong for this parent context
		},
	}

	err := ValidateBlockP2P(forkChild, chain)
	if err == nil {
		t.Fatal("expected non-tip fork block with wrong difficulty to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid difficulty") {
		t.Fatalf("unexpected error: %v", err)
	}
}
