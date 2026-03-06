package main

import "testing"

func TestBranchAwareSpentCheckerIncludesSideBranchAncestry(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	genesisHash := genesis.Hash()

	var mainChainKeyImage [32]byte
	mainChainKeyImage[0] = 0x11

	var preForkKeyImage [32]byte
	preForkKeyImage[0] = 0x44

	var sideBranchKeyImage [32]byte
	sideBranchKeyImage[0] = 0x22

	var unrelatedBranchKeyImage [32]byte
	unrelatedBranchKeyImage[0] = 0x33

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
				Inputs: []TxInput{
					{KeyImage: mainChainKeyImage},
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
			Height:     2,
			PrevHash:   sideParentHash,
			Timestamp:  genesis.Header.Timestamp + 3,
			Difficulty: MinDifficulty,
		},
	}
	sideTipHash := sideTip.Hash()

	unrelatedSide := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesisHash,
			Timestamp:  genesis.Header.Timestamp + 4,
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

	chain.blocks[mainHash] = mainBlock
	chain.bestHash = mainHash
	chain.height = 1
	chain.byHeight[1] = mainHash
	chain.keyImages[mainChainKeyImage] = 1
	chain.keyImages[preForkKeyImage] = 0

	chain.blocks[sideParentHash] = sideParent
	chain.blocks[sideTipHash] = sideTip
	chain.blocks[unrelatedSideHash] = unrelatedSide

	checker, err := chain.branchAwareSpentCheckerLocked(sideTipHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware spent checker: %v", err)
	}

	if !checker(sideBranchKeyImage) {
		t.Fatal("expected side-branch ancestor key image to be treated as spent")
	}
	if checker(mainChainKeyImage) {
		t.Fatal("expected post-fork main-chain key image to be ignored for side-branch validation")
	}
	if !checker(preForkKeyImage) {
		t.Fatal("expected pre-fork key image to remain spent for side-branch validation")
	}
	if checker(unrelatedBranchKeyImage) {
		t.Fatal("expected unrelated side-branch key image to not be treated as spent")
	}
}

func TestBranchAwareSpentCheckerTipExtensionKeepsMainSpent(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	var spentOnTip [32]byte
	spentOnTip[0] = 0xaa

	mainBlock := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + 1,
			Difficulty: MinDifficulty,
		},
	}
	mainHash := mainBlock.Hash()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	chain.blocks[mainHash] = mainBlock
	chain.bestHash = mainHash
	chain.height = 1
	chain.byHeight[1] = mainHash
	chain.keyImages[spentOnTip] = 1

	checker, err := chain.branchAwareSpentCheckerLocked(mainHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware spent checker: %v", err)
	}

	if !checker(spentOnTip) {
		t.Fatal("expected same-branch spent key image to remain spent for tip extension validation")
	}
}
