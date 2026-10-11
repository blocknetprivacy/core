package main

import (
	"testing"
	"time"

	"golang.org/x/crypto/sha3"
)

func newPrecheckTestChain(t *testing.T, tipHeight uint64) (*Chain, [32]byte, int64) {
	t.Helper()
	chain, err := NewChain(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	t.Cleanup(func() { _ = chain.Close() })
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
			MerkleRoot: sha3.Sum256([]byte{0xB0, byte(h)}),
			Timestamp:  prevTimestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
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
	return chain, prevHash, prevTimestamp
}

func precheckCandidate(t *testing.T, chain *Chain, prevHash [32]byte, height uint64, timestamp int64) *Block {
	t.Helper()
	b := &Block{
		Header:       BlockHeader{Version: 1, Height: height, PrevHash: prevHash, Timestamp: timestamp},
		Transactions: []*Transaction{{Version: 1}},
	}
	parent := chain.GetBlockFast(prevHash)
	difficulty, err := expectedDifficultyFromParent(parent, chain.GetBlockFast)
	if err != nil {
		t.Fatalf("expected difficulty: %v", err)
	}
	b.Header.Difficulty = difficulty
	root, err := b.ComputeMerkleRoot()
	if err != nil {
		t.Fatalf("merkle root: %v", err)
	}
	b.Header.MerkleRoot = root
	return b
}

func TestCheapPrecheckAcceptsValidHeaderFromAncestorHeaders(t *testing.T) {
	chain, tip, tipTime := newPrecheckTestChain(t, 80)
	b := precheckCandidate(t, chain, tip, 81, tipTime+BlockIntervalSec)
	if !chain.passesCheapPrecheck(b) {
		t.Fatal("valid candidate failed the cheap precheck")
	}
	b.Header.Difficulty++
	if chain.passesCheapPrecheck(b) {
		t.Fatal("wrong difficulty passed the cheap precheck")
	}
}

func TestCheapPrecheckRejectsCheckpointMismatchBeforePreverify(t *testing.T) {
	chain, tip, tipTime := newPrecheckTestChain(t, 80)
	b := precheckCandidate(t, chain, tip, 81, tipTime+BlockIntervalSec)
	chain.SetTrustedCheckpoints(map[uint64][32]byte{81: sha3.Sum256([]byte("other block"))}, 81)
	if chain.passesCheapPrecheck(b) {
		t.Fatal("checkpoint mismatch passed the cheap precheck")
	}
	chain.SetTrustedCheckpoints(map[uint64][32]byte{81: b.Hash()}, 81)
	if !chain.passesCheapPrecheck(b) {
		t.Fatal("matching checkpoint failed the cheap precheck")
	}
}

func TestCheapPrecheckRejectsBadTimestampBeforePreverify(t *testing.T) {
	chain, tip, _ := newPrecheckTestChain(t, 80)
	b := precheckCandidate(t, chain, tip, 81, time.Now().Add(TimestampFuturLimit+time.Hour).Unix())
	if chain.passesCheapPrecheck(b) {
		t.Fatal("future timestamp passed the cheap precheck")
	}
}
