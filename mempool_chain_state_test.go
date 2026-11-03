package main

import (
	"sync/atomic"
	"testing"
)

func chainStateTestMempool(isSpent KeyImageChecker, isMember RingMemberChecker) *Mempool {
	cfg := DefaultMempoolConfig()
	cfg.MinFeeRate = 0
	return NewMempool(cfg, isSpent, isMember)
}

func TestKeepSpendableOnChainEvictsSpentAndOrphanedRingTransactions(t *testing.T) {
	var spent, memberGone atomic.Bool
	m := chainStateTestMempool(
		func([32]byte) bool { return spent.Load() },
		func(_, _ [32]byte) bool { return !memberGone.Load() },
	)
	tx := mustBuildValidRingCTBindingTestTx(t)
	if err := m.AddTransaction(tx, tx.Serialize()); err != nil {
		t.Fatalf("add: %v", err)
	}
	selected := m.GetTransactionsForBlock(MaxBlockSize, 10)
	if got := m.KeepSpendableOnChain(selected); len(got) != 1 {
		t.Fatalf("a valid transaction was dropped, kept %d", len(got))
	}

	memberGone.Store(true)
	if got := m.KeepSpendableOnChain(selected); len(got) != 0 {
		t.Fatalf("kept %d transactions whose ring left the chain", len(got))
	}
	if m.Size() != 0 {
		t.Fatal("the invalid transaction stayed in the mempool")
	}

	memberGone.Store(false)
	if err := m.AddTransaction(tx, tx.Serialize()); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	spent.Store(true)
	if got := m.KeepSpendableOnChain(m.GetTransactionsForBlock(MaxBlockSize, 10)); len(got) != 0 {
		t.Fatalf("kept %d spent transactions", len(got))
	}
	if m.Size() != 0 {
		t.Fatal("the spent transaction stayed in the mempool")
	}
}

func TestAddTransactionRechecksKeyImagesAfterValidation(t *testing.T) {
	var calls atomic.Int32
	tx := mustBuildValidRingCTBindingTestTx(t)
	inputs := int32(len(tx.Inputs))
	// Validation sees unspent images; a block commits before the insert.
	m := chainStateTestMempool(
		func([32]byte) bool { return calls.Add(1) > inputs },
		func(_, _ [32]byte) bool { return true },
	)
	if err := m.AddTransaction(tx, tx.Serialize()); err == nil {
		t.Fatal("a transaction spent during validation entered the mempool")
	}
	if m.Size() != 0 {
		t.Fatal("the mempool should be empty")
	}
}

func TestRemoveUnspendableOnChainChecksEveryEntry(t *testing.T) {
	var memberGone atomic.Bool
	m := chainStateTestMempool(
		func([32]byte) bool { return false },
		func(_, _ [32]byte) bool { return !memberGone.Load() },
	)
	tx := mustBuildValidRingCTBindingTestTx(t)
	if err := m.AddTransaction(tx, tx.Serialize()); err != nil {
		t.Fatalf("add: %v", err)
	}
	m.RemoveUnspendableOnChain()
	if m.Size() != 1 {
		t.Fatal("a valid entry was removed")
	}
	memberGone.Store(true)
	m.RemoveUnspendableOnChain()
	if m.Size() != 0 {
		t.Fatal("an entry whose ring left the chain stayed in the mempool")
	}
}
