package main

import "testing"

func TestBranchAwareRingMemberCheckerIncludesSideBranchOutputs(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	genesisHash := genesis.Hash()

	var mainPub [32]byte
	mainPub[0] = 0x11
	var mainCommit [32]byte
	mainCommit[0] = 0x22

	var sidePub [32]byte
	sidePub[0] = 0x33
	var sideCommit [32]byte
	sideCommit[0] = 0x44

	mainBlock := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesisHash,
			Timestamp:  genesis.Header.Timestamp + 1,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Outputs: []TxOutput{
					{
						PublicKey:  mainPub,
						Commitment: mainCommit,
					},
				},
			},
		},
	}
	mainHash := mainBlock.Hash()

	sideParent := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesisHash,
			Timestamp:  genesis.Header.Timestamp + 2,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Outputs: []TxOutput{
					{
						PublicKey:  sidePub,
						Commitment: sideCommit,
					},
				},
			},
		},
	}
	sideParentHash := sideParent.Hash()

	sideTip := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     2,
			PrevHash:   sideParentHash,
			Timestamp:  genesis.Header.Timestamp + 3,
			Difficulty: MinDifficulty,
		},
	}
	sideTipHash := sideTip.Hash()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	chain.blocks[mainHash] = mainBlock
	chain.bestHash = mainHash
	chain.height = 1
	chain.byHeight[1] = mainHash

	chain.blocks[sideParentHash] = sideParent
	chain.blocks[sideTipHash] = sideTip

	checker, err := chain.branchAwareRingMemberCheckerLocked(sideTipHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware ring checker: %v", err)
	}

	if !checker(sidePub, sideCommit) {
		t.Fatal("expected side-branch ancestor output to be canonical for side-branch validation")
	}
	if checker(mainPub, mainCommit) {
		t.Fatal("expected post-fork main-chain output to be excluded for side-branch validation")
	}
}
