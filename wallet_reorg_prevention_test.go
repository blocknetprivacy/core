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

func TestRewindWalletToCanonicalTipHandlesDeepFork(t *testing.T) {
	for _, retainHistory := range []bool{true, false} {
		name := "legacy_tip_only"
		if retainHistory {
			name = "recent_history"
		}
		t.Run(name, func(t *testing.T) {
			chain, _, cleanup := mustCreateTestChain(t)
			defer cleanup()
			mustAddGenesisBlock(t, chain)
			for height := uint64(1); height <= 3; height++ {
				addSyntheticCanonicalBlock(t, chain, height, nil)
			}
			w, err := wallet.NewWallet(t.TempDir()+"/wallet.dat", []byte("pw"), defaultWalletConfig())
			if err != nil {
				t.Fatal(err)
			}
			// Height 1 is the common ancestor. The stale output is below the
			// tip, so rewinding only the last block cannot repair this fork.
			w.AddOutput(&wallet.OwnedOutput{TxID: [32]byte{1}, BlockHeight: 1, Amount: 10})
			w.AddOutput(&wallet.OwnedOutput{TxID: [32]byte{2}, BlockHeight: 2, Amount: 20})
			if retainHistory {
				w.SetSyncedBlock(1, chain.GetBlockByHeight(1).Hash())
				w.SetSyncedBlock(2, [32]byte{0xfa})
			}
			w.SetSyncedBlock(3, [32]byte{0xfb})
			removed := rewindWalletToCanonicalTip(w, chain)
			wantHeight, wantRemoved := uint64(0), 2
			if retainHistory {
				wantHeight, wantRemoved = 1, 1
			}
			if got := w.SyncedHeight(); got != wantHeight || removed != wantRemoved {
				t.Fatalf("height=%d removed=%d, want %d/%d", got, removed, wantHeight, wantRemoved)
			}
			// A second reconciliation must not rewind a matching ancestor.
			if got := rewindWalletToCanonicalTip(w, chain); got != 0 {
				t.Fatalf("second rewind removed %d outputs", got)
			}
		})
	}
}
