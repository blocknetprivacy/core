package main

import (
	"testing"

	"blocknet/wallet"
)

func TestCLIRecoverWalletAfterChainReset_RewindsWhenWalletAhead(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	chain.height = 2
	chain.publishTipLocked()
	chain.mu.Unlock()

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	walletPath := t.TempDir() + "/wallet.dat"
	w, err := wallet.NewWallet(walletPath, []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	w.SetSyncedHeight(4)
	if err := w.Save(); err != nil {
		t.Fatalf("failed to save wallet fixture: %v", err)
	}

	c := &CLI{daemon: d, wallet: w}
	c.recoverWalletAfterChainReset()

	if got := w.SyncedHeight(); got != 2 {
		t.Fatalf("expected synced height rewound to 2, got %d", got)
	}
}

func TestCLIRecoverWalletAfterChainReset_RewindsSameHeightStaleState(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	chain.height = 2
	chain.publishTipLocked()
	chain.mu.Unlock()

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	walletPath := t.TempDir() + "/wallet.dat"
	w, err := wallet.NewWallet(walletPath, []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	w.AddOutput(&wallet.OwnedOutput{
		TxID:        [32]byte{0xEE},
		OutputIndex: 0,
		Amount:      500,
		BlockHeight: 2,
	})
	w.SetSyncedHeight(2)
	if err := w.Save(); err != nil {
		t.Fatalf("failed to save wallet fixture: %v", err)
	}

	c := &CLI{daemon: d, wallet: w}
	c.recoverWalletAfterChainReset()

	_, unspent := w.OutputCount()
	if unspent != 0 {
		t.Fatalf("expected stale same-height outputs to be rewound, got unspent=%d", unspent)
	}
}
