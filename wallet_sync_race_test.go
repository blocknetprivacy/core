package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"blocknet/wallet"
)

// A wallet catch-up scan (after load or import) and the live block loop used
// to advance the same wallet without excluding each other: each read the
// other's tip as a reorg and rewound, livelocking the scan when a block
// arrived mid-import. Both now step through syncWalletStep under
// walletScanMu; concurrent scanners must reach the tip with the same outputs
// a single sequential scan finds.
func TestConcurrentWalletScansAgree(t *testing.T) {
	walletScanTestnet(t)
	chain, _, cleanup := mustCreateTestChain(t)
	t.Cleanup(cleanup)
	mustAddGenesisBlock(t, chain)

	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	other, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	const blocks = 150
	var owned uint64
	for h := uint64(1); h <= blocks; h++ {
		decoy, err := CreateCoinbase(other.SpendPubKey, other.ViewPubKey, 50_000_000, h)
		if err != nil {
			t.Fatal(err)
		}
		txs := []*Transaction{decoy.Tx}
		if h%7 == 0 {
			reward, err := CreateCoinbase(keys.SpendPubKey, keys.ViewPubKey, 1_000_000+h, h)
			if err != nil {
				t.Fatal(err)
			}
			txs = append(txs, reward.Tx)
			owned += 1_000_000 + h
		}
		walletScanAppendBlock(t, chain, txs...)
	}

	sk := wallet.StealthKeys{SpendPrivKey: keys.SpendPrivKey, SpendPubKey: keys.SpendPubKey, ViewPrivKey: keys.ViewPrivKey, ViewPubKey: keys.ViewPubKey}
	newWallet := func(name string) (*wallet.Wallet, *wallet.Scanner) {
		w, err := wallet.NewWalletFromStealthKeys(filepath.Join(t.TempDir(), name), []byte("scan-race-password"), sk, defaultWalletConfig())
		if err != nil {
			t.Fatal(err)
		}
		return w, wallet.NewScanner(w, defaultScannerConfig())
	}

	// Reference: one scanner on its own.
	ref, refScanner := newWallet("ref.dat")
	for syncWalletStep(chain, nil, ref, refScanner) {
	}

	// Four scanners on one wallet at once, as catch-up plus live blocks do.
	w, sc := newWallet("w.dat")
	c := &CLI{daemon: &Daemon{chain: chain}, wallet: w, scanner: sc}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				moved := c.scanWalletStep(w, sc)
				if !moved {
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent wallet scans did not reach the tip")
	}

	tipHash := chain.GetBlockByHeight(chain.Height()).Hash()
	for name, got := range map[string]*wallet.Wallet{"sequential": ref, "concurrent": w} {
		height, hash := got.SyncedBlock()
		if height != chain.Height() || hash != tipHash {
			t.Fatalf("%s scan stopped at %d (%x), tip %d (%x)", name, height, hash[:4], chain.Height(), tipHash[:4])
		}
	}
	if got := w.Balance(); got != owned || ref.Balance() != owned {
		t.Fatalf("balances: concurrent %d, sequential %d, want %d", got, ref.Balance(), owned)
	}
	if len(w.AllOutputs()) != len(ref.AllOutputs()) {
		t.Fatalf("outputs: concurrent %d, sequential %d", len(w.AllOutputs()), len(ref.AllOutputs()))
	}
}
