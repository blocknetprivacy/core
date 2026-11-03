package main

import (
	"testing"

	"golang.org/x/crypto/sha3"
)

// newForkChoiceTestChain commits tipHeight blocks of the given difficulty, one
// target interval apart, so the next block's difficulty follows LWMA rather than
// the minimum.
func newForkChoiceTestChain(t *testing.T, tipHeight, difficulty uint64) (*Chain, [32]byte, int64) {
	t.Helper()
	dir := t.TempDir()
	chain, err := NewChain(dir)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	mustAddGenesisBlock(t, chain)
	st := chain.Storage()
	prevHash, _, prevWork, found := st.GetTip()
	if !found {
		t.Fatal("missing genesis tip")
	}
	prevTimestamp := GenesisTimestamp
	for h := uint64(1); h <= tipHeight; h++ {
		b := &Block{Header: BlockHeader{
			Version:    1,
			Height:     h,
			PrevHash:   prevHash,
			MerkleRoot: sha3.Sum256([]byte{0xF0, byte(h)}),
			Timestamp:  prevTimestamp + BlockIntervalSec,
			Difficulty: difficulty,
		}}
		hash := b.Hash()
		work, err := addCumulativeWork(prevWork, b.Header.Difficulty)
		if err != nil {
			t.Fatalf("work at height %d: %v", h, err)
		}
		if err := st.CommitBlock(&BlockCommit{Block: b, Height: h, Hash: hash, Work: work, IsMainTip: true}); err != nil {
			t.Fatalf("commit height %d: %v", h, err)
		}
		prevHash, prevWork, prevTimestamp = hash, work, b.Header.Timestamp
	}
	if err := chain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Reopen so the chain loads the committed tip.
	chain, err = NewChain(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = chain.Close() })
	return chain, prevHash, prevTimestamp
}

func processForkChoiceBlock(t *testing.T, chain *Chain, b *Block) bool {
	t.Helper()
	accepted, isMain, err := chain.ProcessPreverifiedBlock(b, BlockPreverification{Verified: true})
	if err != nil || !accepted {
		t.Fatalf("block at height %d not accepted: accepted=%v err=%v", b.Header.Height, accepted, err)
	}
	return isMain
}

// A sibling stamped one second after its parent makes its child harder, so its
// branch carries slightly more work at the same height. The node keeps the branch
// it saw first until the other one is longer.
func TestSameLengthBranchNeedsMoreThanTimestampWork(t *testing.T) {
	chain, tip, tipTime := newForkChoiceTestChain(t, 80, 1_000_000)

	ours := mustAnnouncementBlock(t, chain, tip, 81, tipTime+BlockIntervalSec)
	rival := mustAnnouncementBlock(t, chain, tip, 81, tipTime+1)
	if ours.Header.Difficulty != rival.Header.Difficulty {
		t.Fatalf("siblings should share a difficulty: %d vs %d", ours.Header.Difficulty, rival.Header.Difficulty)
	}
	if !processForkChoiceBlock(t, chain, ours) {
		t.Fatal("our block should become the tip")
	}
	if processForkChoiceBlock(t, chain, rival) {
		t.Fatal("an equal-work sibling replaced the tip")
	}

	ourChild := mustAnnouncementBlock(t, chain, ours.Hash(), 82, tipTime+2*BlockIntervalSec)
	rivalChild := mustAnnouncementBlock(t, chain, rival.Hash(), 82, tipTime+2*BlockIntervalSec)
	extra := rivalChild.Header.Difficulty - ourChild.Header.Difficulty
	if rivalChild.Header.Difficulty <= ourChild.Header.Difficulty || extra > ourChild.Header.Difficulty/2 {
		t.Fatalf("fixture: rival child difficulty %d should be slightly above ours %d", rivalChild.Header.Difficulty, ourChild.Header.Difficulty)
	}
	if !processForkChoiceBlock(t, chain, ourChild) {
		t.Fatal("our child should extend the tip")
	}
	if processForkChoiceBlock(t, chain, rivalChild) {
		t.Fatalf("a same-length branch %d work heavier replaced the tip", extra)
	}
	assertTipUnchanged(t, chain, ourChild.Hash(), 82)
	if got := chain.TipDifficultyFast(); got != ourChild.Header.Difficulty {
		t.Fatalf("tip difficulty %d, want %d", got, ourChild.Header.Difficulty)
	}

	rivalGrandchild := mustAnnouncementBlock(t, chain, rivalChild.Hash(), 83, tipTime+3*BlockIntervalSec)
	if !processForkChoiceBlock(t, chain, rivalGrandchild) {
		t.Fatal("a longer branch with more work should replace the tip")
	}
	if got := chain.BestHash(); got != rivalGrandchild.Hash() {
		t.Fatalf("tip %x, want the rival branch", got[:8])
	}
	if got := chain.TipDifficultyFast(); got != rivalGrandchild.Header.Difficulty {
		t.Fatalf("tip difficulty after the reorg %d, want %d", got, rivalGrandchild.Header.Difficulty)
	}
}
