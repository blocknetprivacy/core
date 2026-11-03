package main

import "testing"

func TestDeepSideChainBlocksAreRejectedCheaply(t *testing.T) {
	dir := t.TempDir()
	chain, err := NewChain(dir)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)
	st := chain.Storage()
	prevHash, _, prevWork, _ := st.GetTip()
	ts := GenesisTimestamp
	var parentAtTen [32]byte
	var parentTime int64
	const tip = MaxReorgDepth + 40
	for h := uint64(1); h <= tip; h++ {
		ts += BlockIntervalSec
		b := &Block{Header: BlockHeader{Version: 1, Height: h, PrevHash: prevHash, MerkleRoot: [32]byte{0xB0, byte(h), byte(h >> 8)}, Timestamp: ts, Difficulty: MinDifficulty}}
		hash := b.Hash()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatalf("work: %v", err)
		}
		if err := st.CommitBlock(&BlockCommit{Block: b, Height: h, Hash: hash, Work: work, IsMainTip: true}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		prevHash, prevWork = hash, work
		if h == 10 {
			parentAtTen, parentTime = hash, ts
		}
	}
	if err := chain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	chain, err = NewChain(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer chain.Close()

	side := mustAnnouncementBlock(t, chain, parentAtTen, 11, parentTime+7)
	if chain.passesCheapPrecheck(side) {
		t.Fatal("a block below the reorg limit should fail the cheap checks")
	}
	if chain.NeedsPreverification(side) {
		t.Fatal("a block below the reorg limit should not get a PoW check")
	}
	if _, _, err := chain.ProcessBlock(side); err == nil {
		t.Fatal("a block below the reorg limit should be rejected")
	}
	if chain.HasBlock(side.Hash()) {
		t.Fatal("a block below the reorg limit should not be stored")
	}
}

func TestRepeatedOutputKeepsItsEarlierCopyInTheRingIndex(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.canonicalRingIndexReady = true
	chain.canonicalRingIndex = make(map[[64]byte]struct{})
	chain.canonicalRingHeights = make(map[[64]byte]uint64)

	out := TxOutput{PublicKey: [32]byte{1}, Commitment: [32]byte{2}}
	block := func(height uint64) *Block {
		return &Block{Header: BlockHeader{Height: height}, Transactions: []*Transaction{{Outputs: []TxOutput{out}}}}
	}
	key := canonicalRingIndexKey(out.PublicKey, out.Commitment)

	chain.updateCanonicalRingIndexForConnect([]*Block{block(10)}, [32]byte{1})
	chain.updateCanonicalRingIndexForConnect([]*Block{block(20)}, [32]byte{2})
	chain.updateCanonicalRingIndexForDisconnect([]*Block{block(20)})
	if _, ok := chain.canonicalRingIndex[key]; !ok {
		t.Fatal("disconnecting a later copy removed the earlier canonical output")
	}
	chain.updateCanonicalRingIndexForDisconnect([]*Block{block(10)})
	if _, ok := chain.canonicalRingIndex[key]; ok {
		t.Fatal("disconnecting the only remaining copy should remove the output")
	}
}
