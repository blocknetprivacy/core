package main

import (
	"strings"
	"testing"
)

func makeOutputOnlyTestBlock(height uint64, prevHash [32]byte, timestamp int64, outputs []TxOutput) *Block {
	return &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     height,
			PrevHash:   prevHash,
			Timestamp:  timestamp,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{
			{
				Version: 1,
				Outputs: outputs,
			},
		},
	}
}

func blockOutputsForCommit(t *testing.T, block *Block) []*UTXO {
	t.Helper()

	var outputs []*UTXO
	for _, tx := range block.Transactions {
		txID, err := tx.TxID()
		if err != nil {
			t.Fatalf("failed to hash tx outputs: %v", err)
		}
		for idx, out := range tx.Outputs {
			outputs = append(outputs, &UTXO{
				TxID:        txID,
				OutputIndex: uint32(idx),
				Output:      out,
				BlockHeight: block.Header.Height,
			})
		}
	}
	return outputs
}

func commitMainChainBlockForTest(t *testing.T, chain *Chain, storage *Storage, block *Block, work uint64) {
	t.Helper()

	hash := block.Hash()
	if err := storage.CommitBlock(&BlockCommit{
		Block:      block,
		Height:     block.Header.Height,
		Hash:       hash,
		Work:       work,
		IsMainTip:  true,
		NewOutputs: blockOutputsForCommit(t, block),
	}); err != nil {
		t.Fatalf("failed to commit block at height %d: %v", block.Header.Height, err)
	}

	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.blocks[hash] = block
	chain.workAt[hash] = work
	chain.byHeight[block.Header.Height] = hash
	chain.bestHash = hash
	chain.height = block.Header.Height
	chain.totalWork = work
	chain.canonicalRingIndexDirty = true
}

func TestBranchAwareRingMemberCheckerScopesOutputsToCandidateBranch(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	var commonPub, commonComm [32]byte
	commonPub[0] = 0x11
	commonComm[0] = 0x12

	var mainPub, mainComm [32]byte
	mainPub[0] = 0x21
	mainComm[0] = 0x22

	var sidePub, sideComm [32]byte
	sidePub[0] = 0x31
	sideComm[0] = 0x32

	var unrelatedPub, unrelatedComm [32]byte
	unrelatedPub[0] = 0x41
	unrelatedComm[0] = 0x42

	commonAncestor := makeOutputOnlyTestBlock(
		1,
		genesis.Hash(),
		genesis.Header.Timestamp+BlockIntervalSec,
		[]TxOutput{{PublicKey: commonPub, Commitment: commonComm}},
	)
	commitMainChainBlockForTest(t, chain, storage, commonAncestor, 2)

	mainTip := makeOutputOnlyTestBlock(
		2,
		commonAncestor.Hash(),
		commonAncestor.Header.Timestamp+BlockIntervalSec,
		[]TxOutput{{PublicKey: mainPub, Commitment: mainComm}},
	)
	commitMainChainBlockForTest(t, chain, storage, mainTip, 3)

	sideParent := makeOutputOnlyTestBlock(
		2,
		commonAncestor.Hash(),
		commonAncestor.Header.Timestamp+2*BlockIntervalSec,
		[]TxOutput{{PublicKey: sidePub, Commitment: sideComm}},
	)

	sideParentHash := sideParent.Hash()

	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.blocks[sideParentHash] = sideParent

	checker, err := chain.branchAwareRingMemberCheckerLocked(sideParentHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware ring checker: %v", err)
	}

	if !checker(commonPub, commonComm) {
		t.Fatal("expected common-ancestor output to remain canonical for the candidate branch")
	}
	if checker(mainPub, mainComm) {
		t.Fatal("expected divergent main-chain output above the fork point to be ignored")
	}
	if !checker(sidePub, sideComm) {
		t.Fatal("expected side-branch ancestor output to be canonical for the candidate branch")
	}
	if checker(unrelatedPub, unrelatedComm) {
		t.Fatal("expected unrelated output to not be canonical for the candidate branch")
	}
}

func TestValidateBlockWithContext_AcceptsSideBranchRingMembers(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	spendTx := mustBuildValidRingCTBindingTestTx(t)

	var commonPub, commonComm [32]byte
	commonPub[0] = 0x51
	commonComm[0] = 0x52

	var mainPub, mainComm [32]byte
	mainPub[0] = 0x61
	mainComm[0] = 0x62

	commonAncestor := makeOutputOnlyTestBlock(
		1,
		genesis.Hash(),
		genesis.Header.Timestamp+BlockIntervalSec,
		[]TxOutput{{PublicKey: commonPub, Commitment: commonComm}},
	)
	commitMainChainBlockForTest(t, chain, storage, commonAncestor, 2)

	mainTip := makeOutputOnlyTestBlock(
		2,
		commonAncestor.Hash(),
		commonAncestor.Header.Timestamp+BlockIntervalSec,
		[]TxOutput{{PublicKey: mainPub, Commitment: mainComm}},
	)
	commitMainChainBlockForTest(t, chain, storage, mainTip, 3)

	sideOutputs := make([]TxOutput, len(spendTx.Inputs[0].RingMembers))
	for i := range spendTx.Inputs[0].RingMembers {
		sideOutputs[i] = TxOutput{
			PublicKey:  spendTx.Inputs[0].RingMembers[i],
			Commitment: spendTx.Inputs[0].RingCommitments[i],
		}
	}
	sideParent := makeOutputOnlyTestBlock(
		2,
		commonAncestor.Hash(),
		commonAncestor.Header.Timestamp+2*BlockIntervalSec,
		sideOutputs,
	)
	sideParentHash := sideParent.Hash()

	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("failed to generate coinbase keys: %v", err)
	}
	coinbase, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, GetBlockReward(3), 3)
	if err != nil {
		t.Fatalf("failed to create coinbase: %v", err)
	}

	candidate := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     3,
			PrevHash:   sideParentHash,
			Timestamp:  sideParent.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{coinbase.Tx, spendTx},
	}
	merkleRoot, err := candidate.ComputeMerkleRoot()
	if err != nil {
		t.Fatalf("failed to compute merkle root: %v", err)
	}
	candidate.Header.MerkleRoot = merkleRoot

	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.blocks[sideParentHash] = sideParent

	spentChecker, err := chain.branchAwareSpentCheckerLocked(sideParentHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware spent checker: %v", err)
	}

	if err := validateBlockWithContext(
		candidate,
		chain.bestHash,
		chain.height,
		chain.getBlockByHashLocked,
		spentChecker,
		chain.isCanonicalRingMemberLocked,
		true,
		true,
	); err == nil || !strings.Contains(err.Error(), "not a canonical on-chain output") {
		t.Fatalf("expected current canonical checker to reject side-branch ring members, got: %v", err)
	}

	ringChecker, err := chain.branchAwareRingMemberCheckerLocked(sideParentHash)
	if err != nil {
		t.Fatalf("failed to construct branch-aware ring checker: %v", err)
	}

	if err := validateBlockWithContext(
		candidate,
		chain.bestHash,
		chain.height,
		chain.getBlockByHashLocked,
		spentChecker,
		ringChecker,
		true,
		true,
	); err != nil {
		t.Fatalf("expected branch-aware ring checker to accept side-branch block, got: %v", err)
	}
}
