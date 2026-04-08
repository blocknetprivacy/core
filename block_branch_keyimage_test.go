package main

import "testing"

func TestBranchAwareSpentCheckerScopesSpendsToCandidateBranch(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	genesisHash := genesis.Hash()

	var commonAncestorKeyImage [32]byte
	commonAncestorKeyImage[0] = 0x11

	var divergentMainChainKeyImage [32]byte
	divergentMainChainKeyImage[0] = 0x22

	var sideBranchKeyImage [32]byte
	sideBranchKeyImage[0] = 0x33

	var unrelatedBranchKeyImage [32]byte
	unrelatedBranchKeyImage[0] = 0x44

	commonAncestor := &Block{
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
				Inputs: []TxInput{
					{KeyImage: commonAncestorKeyImage},
				},
			},
		},
	}
	commonAncestorHash := commonAncestor.Hash()

	mainTip := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     2,
			PrevHash:   commonAncestorHash,
			Timestamp:  genesis.Header.Timestamp + 2,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Inputs: []TxInput{
					{KeyImage: divergentMainChainKeyImage},
				},
			},
		},
	}
	mainTipHash := mainTip.Hash()

	sideParent := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     2,
			PrevHash:   commonAncestorHash,
			Timestamp:  genesis.Header.Timestamp + 3,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Inputs: []TxInput{
					{KeyImage: sideBranchKeyImage},
				},
			},
		},
	}
	sideParentHash := sideParent.Hash()

	sideTip := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     3,
			PrevHash:   sideParentHash,
			Timestamp:  genesis.Header.Timestamp + 4,
			Difficulty: MinDifficulty,
		},
	}
	sideTipHash := sideTip.Hash()

	unrelatedSide := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     2,
			PrevHash:   commonAncestorHash,
			Timestamp:  genesis.Header.Timestamp + 5,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Inputs: []TxInput{
					{KeyImage: unrelatedBranchKeyImage},
				},
			},
		},
	}
	unrelatedSideHash := unrelatedSide.Hash()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	chain.blocks[commonAncestorHash] = commonAncestor
	chain.byHeight[1] = commonAncestorHash
	chain.keyImages[commonAncestorKeyImage] = 1

	chain.blocks[mainTipHash] = mainTip
	chain.byHeight[2] = mainTipHash
	chain.keyImages[divergentMainChainKeyImage] = 2

	chain.blocks[sideParentHash] = sideParent
	chain.blocks[sideTipHash] = sideTip
	chain.blocks[unrelatedSideHash] = unrelatedSide
	chain.bestHash = mainTipHash
	chain.height = 2

	checker, err := chain.branchAwareSpentCheckerLocked(sideTipHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware spent checker: %v", err)
	}

	if !checker(commonAncestorKeyImage) {
		t.Fatal("expected common-ancestor key image to remain spent")
	}
	if checker(divergentMainChainKeyImage) {
		t.Fatal("expected divergent main-chain key image above the fork point to be ignored")
	}
	if !checker(sideBranchKeyImage) {
		t.Fatal("expected side-branch ancestor key image to be treated as spent")
	}
	if checker(unrelatedBranchKeyImage) {
		t.Fatal("expected unrelated side-branch key image to not be treated as spent")
	}
}
