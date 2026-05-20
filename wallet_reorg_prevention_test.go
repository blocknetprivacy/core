package main

import (
	"testing"

	"blocknet/wallet"
)

func TestRewindWalletToCanonicalTipRewindsKnownForkedTip(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	forked := makeOutputOnlyTestBlock(
		1,
		genesis.Hash(),
		genesis.Header.Timestamp+BlockIntervalSec,
		nil,
	)
	canonical := makeOutputOnlyTestBlock(
		1,
		genesis.Hash(),
		genesis.Header.Timestamp+BlockIntervalSec+1,
		nil,
	)
	commitMainChainBlockForTest(t, chain, storage, canonical, MinDifficulty)

	w, err := wallet.NewWallet(t.TempDir()+"/wallet.dat", []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	forkedHash := forked.Hash()
	w.AddOutput(&wallet.OwnedOutput{
		TxID:        [32]byte{0x01},
		OutputIndex: 0,
		Amount:      500,
		BlockHeight: 1,
		BlockHash:   forkedHash,
	})
	w.SetSyncedBlock(1, forkedHash)

	removed := rewindWalletToCanonicalTip(w, chain)
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	if got := w.SyncedHeight(); got != 0 {
		t.Fatalf("SyncedHeight=%d, want 0", got)
	}
	total, unspent := w.OutputCount()
	if total != 0 || unspent != 0 {
		t.Fatalf("OutputCount total=%d unspent=%d, want 0/0", total, unspent)
	}
}

func TestRewindWalletToCanonicalTipKeepsMatchingTip(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	canonical := makeOutputOnlyTestBlock(
		1,
		genesis.Hash(),
		genesis.Header.Timestamp+BlockIntervalSec,
		nil,
	)
	commitMainChainBlockForTest(t, chain, storage, canonical, MinDifficulty)

	w, err := wallet.NewWallet(t.TempDir()+"/wallet.dat", []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	canonicalHash := canonical.Hash()
	w.SetSyncedBlock(1, canonicalHash)

	if removed := rewindWalletToCanonicalTip(w, chain); removed != 0 {
		t.Fatalf("removed=%d, want 0", removed)
	}
	if got := w.SyncedHeight(); got != 1 {
		t.Fatalf("SyncedHeight=%d, want 1", got)
	}
}
