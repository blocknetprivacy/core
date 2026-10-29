package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/wallet"
)

func TestHandleSendFragmentedBalanceUsesSingleInput(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	chain.mu.Lock()
	chain.height = 100
	chain.publishTipLocked()
	chain.mu.Unlock()

	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatal(err)
	}
	// The small outputs alone can cover the payment, but would add hundreds
	// of inputs. They should remain unused, so synthetic outpoints suffice.
	for i := 0; i < 256; i++ {
		w.AddOutput(&wallet.OwnedOutput{OutputIndex: i, Amount: 100_000, BlockHeight: 1})
	}
	seedCanonicalSpendableOutput(t, chain, storage, w, 30_000_000)
	api := NewAPIServer(&Daemon{chain: chain}, w, nil, t.TempDir(), []byte("pw"))
	body := `{"recipients":[{"address":"` + w.Address() + `","amount":20000000}],"dry_run":true}`
	// Repeating the estimate also verifies that its input lease is released.
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		api.handleSend(rr, httptest.NewRequest(http.MethodPost, "/api/wallet/send", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("dry-run returned %d: %s", rr.Code, rr.Body.String())
		}
		var got struct {
			InputCount int    `json:"input_count"`
			InputTotal uint64 `json:"input_total"`
			Fee        uint64 `json:"fee"`
			Change     uint64 `json:"change"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		wantFee := uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize)) * sendFeePerByte
		if got.InputCount != 1 || got.InputTotal != 30_000_000 || got.Fee != wantFee || got.Change != 10_000_000-wantFee {
			t.Fatalf("unexpected single-input estimate: %+v, want fee %d", got, wantFee)
		}
	}

	// Exercise the ordinary send builder with real signing and range proofs,
	// then validate the resulting transaction against the fixture's chain.
	keys := w.Keys()
	result, err := api.createTxBuilder().Transfer([]wallet.Recipient{{
		SpendPubKey: keys.SpendPubKey,
		ViewPubKey:  keys.ViewPubKey,
		Amount:      20_000_000,
	}}, sendFeePerByte, chain.Height())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := DeserializeTx(result.TxData)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Inputs) != 1 || len(tx.Outputs) != 2 {
		t.Fatalf("built %d inputs and %d outputs, want 1 and 2", len(tx.Inputs), len(tx.Outputs))
	}
	if err := ValidateTransaction(tx, chain.IsKeyImageSpent, chain.IsCanonicalRingMember); err != nil {
		t.Fatalf("built transaction failed validation: %v", err)
	}
	if tx.Fee != uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize))*sendFeePerByte {
		t.Fatalf("built fee %d differs from the dry-run estimate", tx.Fee)
	}
	t.Logf("fragmented-wallet payment: %d inputs, %d serialized bytes, fee %d", len(tx.Inputs), len(result.TxData), tx.Fee)
}
