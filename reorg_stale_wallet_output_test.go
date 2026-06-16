package main

import (
	"testing"
	"time"

	"blocknet/wallet"
)

// TestReorgLeavesStaleSpendableWalletOutput reproduces the mempool rejection
//
//	input N ring member k: not a canonical on-chain output
//
// by validating the full causal chain end-to-end:
//
//  1. The wallet owns output O, received in block A (canonical at height 1).
//  2. The chain reorgs: block A is orphaned, replaced by block B (which does
//     NOT contain O). The daemon drops O from canonicalRingIndex. (The chain
//     side is also covered by TestCanonicalRingIndexRefreshesAcrossReorgTipChange.)
//  3. The wallet processes the new tip exactly as the live path does
//     (scanner.ScanBlock on the connected block + SetSyncedHeight). That path
//     only ADDS owned outputs / marks spends — it never rewinds — so O survives
//     as an unspent output.
//  4. The send path (handleSendAdvanced) resolves caller inputs via
//     ReserveSpecificInputs, which checks only wallet-local state, so it hands
//     O back as a real input. The builder signs it as ring member
//     (OneTimePubKey, Commitment) — builder.go:552 — and the mempool then
//     rejects the tx because O is no longer canonical. (The validator's error
//     string is covered by block_branch_ringmember_test.go.)
//
// Invariant under test: every spendable wallet output must be canonical
// on-chain. Before the reorg-rollback fix the wallet retained O after the reorg
// and this failed; applyBlockToWallet now reconciles the reorg and drops O.
func TestReorgLeavesStaleSpendableWalletOutput(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	// --- Output O, received by the wallet in block A (canonical, height 1) ---
	pubA, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("keypair pubA: %v", err)
	}
	commA, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("keypair commA: %v", err)
	}
	txA := &Transaction{
		Version: 1,
		Outputs: []TxOutput{{PublicKey: pubA.PublicKey, Commitment: commA.PublicKey}},
	}
	txAID, err := txA.TxID()
	if err != nil {
		t.Fatalf("txid txA: %v", err)
	}
	blockA := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{txA},
	}
	if err := storage.CommitBlock(&BlockCommit{
		Block:     blockA,
		Height:    1,
		Hash:      blockA.Hash(),
		Work:      2,
		IsMainTip: true,
		NewOutputs: []*UTXO{{
			TxID:        txAID,
			OutputIndex: 0,
			Output:      txA.Outputs[0],
			BlockHeight: 1,
		}},
	}); err != nil {
		t.Fatalf("commit blockA: %v", err)
	}

	// The wallet owns O and is synced to the block-A tip.
	walletPath := t.TempDir() + "/wallet.dat"
	w, err := wallet.NewWallet(walletPath, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("new wallet: %v", err)
	}
	w.AddOutput(&wallet.OwnedOutput{
		TxID:          txAID,
		OutputIndex:   0,
		Amount:        1000,
		OneTimePubKey: pubA.PublicKey,
		Commitment:    commA.PublicKey,
		BlockHeight:   1,
	})
	w.SetSyncedTip(1, blockA.Hash())

	// Precondition: before the reorg, O is canonical.
	if !chain.IsCanonicalRingMember(pubA.PublicKey, commA.PublicKey) {
		t.Fatal("precondition: O should be canonical before reorg")
	}

	// --- Reorg: orphan block A, connect block B (does NOT contain O) ---
	pubB, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("keypair pubB: %v", err)
	}
	commB, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("keypair commB: %v", err)
	}
	txB := &Transaction{
		Version: 1,
		Outputs: []TxOutput{{PublicKey: pubB.PublicKey, Commitment: commB.PublicKey}},
	}
	blockB := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     1,
			PrevHash:   genesis.Hash(),
			Timestamp:  genesis.Header.Timestamp + BlockIntervalSec + 1,
			Difficulty: MinDifficulty,
		},
		Transactions: []*Transaction{txB},
	}
	if err := storage.CommitReorg(&ReorgCommit{
		Disconnect: []*Block{blockA},
		Connect:    []*Block{blockB},
		NewTip:     blockB.Hash(),
		NewHeight:  1,
		NewWork:    2,
	}); err != nil {
		t.Fatalf("reorg A->B: %v", err)
	}

	// Reflect the reorged tip in the in-memory chain state the wallet reconcile
	// reads (storage.CommitReorg updates storage but not c.height).
	chain.mu.Lock()
	chain.height = 1
	chain.bestHash = blockB.Hash()
	chain.mu.Unlock()

	// The wallet processes the new tip through the same entrypoint the live scan
	// loop uses. blockB does not build on the wallet's synced tip (blockA), so
	// applyBlockToWallet detects the reorg and rewinds the orphaned output
	// instead of blindly advancing.
	scanner := wallet.NewScanner(w, defaultScannerConfig())
	applyBlockToWallet(chain, w, scanner, blockB)

	// The chain has dropped O from its canonical set.
	if chain.IsCanonicalRingMember(pubA.PublicKey, commA.PublicKey) {
		t.Fatal("expected O to be non-canonical after reorg")
	}

	// Evidence that the send entrypoint still accepts O as a real input. A deep
	// reorg leaves the orphaned output mature, so pass a mature height (the
	// handler passes the live chain height to ReserveSpecificInputs).
	matureHeight := uint64(1 + SafeConfirmations)
	if _, inputs, rerr := w.ReserveSpecificInputs(
		[]wallet.OutputRef{{TxID: txAID, OutputIndex: 0}},
		matureHeight,
		time.Minute,
	); rerr == nil && len(inputs) == 1 {
		in := inputs[0]
		canonical := chain.IsCanonicalRingMember(in.OneTimePubKey, in.Commitment)
		t.Logf("send path reserved O as a real input; canonical=%v (txid %x idx %d)",
			canonical, in.TxID, in.OutputIndex)
	}

	// Invariant: every spendable wallet output must be canonical on-chain.
	// This is what fails today and is exactly the precondition for the
	// "not a canonical on-chain output" mempool rejection.
	for _, out := range w.AllOutputs() {
		if out.Spent {
			continue
		}
		if !chain.IsCanonicalRingMember(out.OneTimePubKey, out.Commitment) {
			t.Errorf("wallet still offers a non-canonical output as spendable "+
				"(txid %x idx %d); a send including it is rejected by the mempool "+
				"with \"not a canonical on-chain output\"", out.TxID, out.OutputIndex)
		}
	}
}
