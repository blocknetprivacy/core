package main

import (
	"strings"
	"testing"
)

func TestMempoolRejectsCoinbaseTransaction(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("failed to generate stealth keys: %v", err)
	}

	coinbase, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, GetBlockReward(1), 1)
	if err != nil {
		t.Fatalf("failed to create coinbase tx: %v", err)
	}

	mempool := NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)
	err = mempool.AddTransaction(coinbase.Tx, coinbase.Tx.Serialize())
	if err == nil {
		t.Fatal("expected coinbase transaction to be rejected by mempool")
	}
	if !strings.Contains(err.Error(), "coinbase transaction cannot be added to mempool") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mempool.Size(); got != 0 {
		t.Fatalf("mempool should remain empty, size=%d", got)
	}
}

func TestMempoolRejectsTamperedRingCTExternalKeyImage(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	tx.Inputs[0].KeyImage[0] ^= 0x01

	mempool := NewMempool(
		DefaultMempoolConfig(),
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	err := mempool.AddTransaction(tx, tx.Serialize())
	if err == nil {
		t.Fatal("expected tampered RingCT key image transaction to be rejected by mempool")
	}
	if !strings.Contains(err.Error(), "key image does not match signed RingCT payload") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mempool.Size(); got != 0 {
		t.Fatalf("mempool should remain empty, size=%d", got)
	}
}

func TestMempoolRejectsUnsupportedTxVersion(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	tx.Version = 2

	mempool := NewMempool(
		DefaultMempoolConfig(),
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)
	err := mempool.AddTransaction(tx, tx.Serialize())
	if err == nil {
		t.Fatal("expected unsupported tx version to be rejected by mempool")
	}
	if !strings.Contains(err.Error(), "unsupported tx version") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mempool.Size(); got != 0 {
		t.Fatalf("mempool should remain empty, size=%d", got)
	}
}

func TestMempoolRejectsTrailingBytes(t *testing.T) {
	mempool := NewMempool(
		DefaultMempoolConfig(),
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)

	// Minimal parseable tx bytes; cryptographic validity is irrelevant here because
	// AddTransaction should fail closed on non-canonical txData before validation.
	tx := &Transaction{
		Version:     1,
		TxPublicKey: [32]byte{0xAA},
		Inputs:      nil,
		Outputs: []TxOutput{
			{
				PublicKey:       [32]byte{0xBB},
				Commitment:      [32]byte{0xCC},
				EncryptedAmount: [8]byte{0x01},
			},
		},
		Fee: 0,
	}

	canonical := tx.Serialize()
	withTrailing := append(append([]byte(nil), canonical...), 0xDE, 0xAD, 0xBE, 0xEF)

	err := mempool.AddTransaction(&Transaction{}, withTrailing)
	if err == nil {
		t.Fatal("expected trailing-byte tx to be rejected by mempool")
	}
	if !strings.Contains(err.Error(), "trailing bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mempool.Size(); got != 0 {
		t.Fatalf("mempool should remain empty, size=%d", got)
	}
}

func TestMempoolGenerationTracksContentChanges(t *testing.T) {
	tx := mustBuildValidRingCTBindingTestTx(t)
	cfg := DefaultMempoolConfig()
	cfg.MinFeeRate = 0
	mempool := NewMempool(
		cfg,
		func(_ [32]byte) bool { return false },
		func(_, _ [32]byte) bool { return true },
	)

	if got := mempool.Stats().Generation; got != 0 {
		t.Fatalf("initial generation = %d, want 0", got)
	}
	if err := mempool.AddTransaction(tx, tx.Serialize()); err != nil {
		t.Fatalf("add transaction: %v", err)
	}
	if got := mempool.Stats().Generation; got != 1 {
		t.Fatalf("generation after add = %d, want 1", got)
	}

	selected, generation := mempool.GetTransactionsForBlockSnapshot(MaxBlockSize, 1000)
	if len(selected) != 1 || generation != 1 {
		t.Fatalf("snapshot = (%d txs, generation %d), want (1, 1)", len(selected), generation)
	}

	// Duplicate delivery is not a content change.
	if err := mempool.AddTransaction(tx, tx.Serialize()); err != nil {
		t.Fatalf("add duplicate transaction: %v", err)
	}
	if got := mempool.Stats().Generation; got != 1 {
		t.Fatalf("generation after duplicate = %d, want 1", got)
	}

	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("transaction id: %v", err)
	}
	mempool.RemoveTransaction(txID)
	if stats := mempool.Stats(); stats.Generation != 2 || stats.Count != 0 {
		t.Fatalf("stats after removal = %+v, want generation 2 and count 0", stats)
	}

	// Removing or clearing an already-empty pool must not report a false change.
	mempool.RemoveTransaction(txID)
	mempool.Clear()
	if got := mempool.Stats().Generation; got != 2 {
		t.Fatalf("generation after no-op removals = %d, want 2", got)
	}
}
