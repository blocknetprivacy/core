package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"blocknet/wallet"
)

func TestAuditWalletCanonicalOutputsDetectsStaleOutputs(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	pub := [32]byte{0x10}
	commitment := [32]byte{0x20}
	tx := Transaction{
		Version: 1,
		Outputs: []TxOutput{{
			PublicKey:  pub,
			Commitment: commitment,
		}},
	}
	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to compute txid: %v", err)
	}
	addSyntheticCanonicalBlock(t, chain, 1, []*Transaction{&tx})

	issues := auditWalletCanonicalOutputs(chain, []*wallet.OwnedOutput{
		{TxID: txID, OutputIndex: 0, Amount: 10, BlockHeight: 1, OneTimePubKey: pub, Commitment: commitment},
		{TxID: [32]byte{0x99}, OutputIndex: 0, Amount: 20, BlockHeight: 1, OneTimePubKey: pub, Commitment: commitment},
		{TxID: txID, OutputIndex: 2, Amount: 30, BlockHeight: 1, OneTimePubKey: pub, Commitment: commitment},
		{TxID: txID, OutputIndex: 0, Amount: 40, BlockHeight: 1, OneTimePubKey: [32]byte{0x11}, Commitment: commitment},
		{TxID: txID, OutputIndex: 0, Amount: 50, BlockHeight: 1, OneTimePubKey: pub, Commitment: [32]byte{0x21}},
		{TxID: txID, OutputIndex: 0, Amount: 60, BlockHeight: 99, OneTimePubKey: pub, Commitment: commitment},
		{TxID: txID, OutputIndex: -1, Amount: 70, BlockHeight: 1, OneTimePubKey: pub, Commitment: commitment},
	})

	wantReasons := map[string]bool{
		walletOutputIssueMissingTx:          false,
		walletOutputIssueIndexOutOfRange:    false,
		walletOutputIssuePublicKeyMismatch:  false,
		walletOutputIssueCommitmentMismatch: false,
		walletOutputIssueMissingBlock:       false,
		walletOutputIssueNegativeIndex:      false,
	}
	if len(issues) != len(wantReasons) {
		t.Fatalf("expected %d issues, got %d: %#v", len(wantReasons), len(issues), issues)
	}
	for _, issue := range issues {
		if _, ok := wantReasons[issue.Reason]; !ok {
			t.Fatalf("unexpected issue reason %q: %#v", issue.Reason, issue)
		}
		wantReasons[issue.Reason] = true
	}
	for reason, seen := range wantReasons {
		if !seen {
			t.Fatalf("missing issue reason %q", reason)
		}
	}
}

func TestHandleWalletOutputsAuditCanRepairStaleOutputs(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	pub := [32]byte{0x30}
	commitment := [32]byte{0x40}
	tx := Transaction{
		Version: 1,
		Outputs: []TxOutput{{
			PublicKey:  pub,
			Commitment: commitment,
		}},
	}
	txID, err := tx.TxID()
	if err != nil {
		t.Fatalf("failed to compute txid: %v", err)
	}
	addSyntheticCanonicalBlock(t, chain, 1, []*Transaction{&tx})

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	walletFile := t.TempDir() + "/wallet.dat"
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	w.AddOutput(&wallet.OwnedOutput{
		TxID:          txID,
		OutputIndex:   0,
		Amount:        10,
		BlockHeight:   1,
		OneTimePubKey: pub,
		Commitment:    commitment,
	})
	w.AddOutput(&wallet.OwnedOutput{
		TxID:          [32]byte{0x99},
		OutputIndex:   0,
		Amount:        20,
		BlockHeight:   1,
		OneTimePubKey: pub,
		Commitment:    commitment,
	})

	s := NewAPIServer(d, w, nil, t.TempDir(), []byte("pw"))
	resp := mustMakeHTTPJSONRequest(
		t,
		http.HandlerFunc(s.handleWalletOutputsAudit),
		http.MethodPost,
		"/api/wallet/outputs/audit",
		[]byte(`{"repair":true}`),
		map[string]string{"Content-Type": "application/json"},
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got := int(body["stale_outputs"].(float64)); got != 1 {
		t.Fatalf("expected stale_outputs=1, got %d (%v)", got, body)
	}
	if got := int(body["removed_outputs"].(float64)); got != 1 {
		t.Fatalf("expected removed_outputs=1, got %d (%v)", got, body)
	}
	if got := uint64(body["removed_amount"].(float64)); got != 20 {
		t.Fatalf("expected removed_amount=20, got %d (%v)", got, body)
	}

	total, unspent := w.OutputCount()
	if total != 1 || unspent != 1 {
		t.Fatalf("expected one valid output after repair, got total=%d unspent=%d", total, unspent)
	}
}

func TestRecoverWalletAfterChainResetPrunesDeepNonCanonicalOutputs(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	pub := [32]byte{0x50}
	commitment := [32]byte{0x60}
	tx := Transaction{
		Version: 1,
		Outputs: []TxOutput{{
			PublicKey:  pub,
			Commitment: commitment,
		}},
	}
	addSyntheticCanonicalBlock(t, chain, 1, []*Transaction{&tx})

	chain.mu.Lock()
	chain.height = 3
	chain.mu.Unlock()

	d, stop := mustStartTestDaemon(t, chain)
	defer stop()

	walletFile := t.TempDir() + "/wallet.dat"
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	w.AddOutput(&wallet.OwnedOutput{
		TxID:          [32]byte{0x99},
		OutputIndex:   0,
		Amount:        20,
		BlockHeight:   1,
		OneTimePubKey: pub,
		Commitment:    commitment,
	})
	w.SetSyncedHeight(3)

	c := &CLI{wallet: w, daemon: d, noColor: true}
	c.recoverWalletAfterChainReset()

	total, unspent := w.OutputCount()
	if total != 0 || unspent != 0 {
		t.Fatalf("expected stale deep output to be removed, got total=%d unspent=%d", total, unspent)
	}
}

func addSyntheticCanonicalBlock(t *testing.T, chain *Chain, height uint64, txs []*Transaction) {
	t.Helper()

	prev := chain.GetBlockByHeight(height - 1)
	if prev == nil {
		t.Fatalf("missing previous block at height %d", height-1)
	}
	block := &Block{
		Header: BlockHeader{
			Version:    1,
			Height:     height,
			PrevHash:   prev.Hash(),
			Timestamp:  prev.Header.Timestamp + BlockIntervalSec,
			Difficulty: MinDifficulty,
		},
		Transactions: txs,
	}
	hash := block.Hash()

	chain.mu.Lock()
	chain.blocks[hash] = block
	chain.byHeight[height] = hash
	if chain.height < height {
		chain.height = height
	}
	chain.mu.Unlock()
}
