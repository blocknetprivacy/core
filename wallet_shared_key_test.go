package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

// sharedKeyOutput builds an output from a given transaction key. Reusing that key
// at the same output index repeats the one-time key.
func sharedKeyOutput(t *testing.T, keys *StealthKeys, txPriv [32]byte, index int, amount uint64) wallet.OutputData {
	t.Helper()
	secret, err := DeriveStealthSecretSenderIndexed(txPriv, keys.ViewPubKey, uint32(index))
	if err != nil {
		t.Fatalf("DeriveStealthSecretSenderIndexed: %v", err)
	}
	blinding := wallet.DeriveBlinding(secret, index)
	commitment, err := CreatePedersenCommitmentWithBlinding(amount, blinding)
	if err != nil {
		t.Fatalf("CreatePedersenCommitmentWithBlinding: %v", err)
	}
	encryptedMemo, err := wallet.EncryptMemo(nil, secret, index)
	if err != nil {
		t.Fatalf("EncryptMemo: %v", err)
	}
	return wallet.OutputData{
		Index:           index,
		PubKey:          ownedOnetimePub(t, secret, keys.SpendPubKey),
		Commitment:      commitment,
		EncryptedAmount: EncryptAmount(amount, blinding, index),
		EncryptedMemo:   encryptedMemo,
	}
}

// TestWalletTreatsRepeatedOneTimeKeyAsOneCoin scans two transactions that repeat
// a one-time key with the daemon's real scanner and crypto.
func TestWalletTreatsRepeatedOneTimeKeyAsOneCoin(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}
	w, err := wallet.NewWalletFromStealthKeys(t.TempDir()+"/wallet.dat", []byte("test password"), wallet.StealthKeys{
		SpendPrivKey: keys.SpendPrivKey,
		SpendPubKey:  keys.SpendPubKey,
		ViewPrivKey:  keys.ViewPrivKey,
		ViewPubKey:   keys.ViewPubKey,
	}, defaultWalletConfig())
	if err != nil {
		t.Fatalf("NewWalletFromStealthKeys: %v", err)
	}
	scanner := wallet.NewScanner(w, defaultScannerConfig())

	const large, small = uint64(40_000_000_000), uint64(100_000_000)
	height := params.IndexedDerivationHeight + 100_000
	sender, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}
	tx := func(id byte, amount uint64) wallet.TxData {
		return wallet.TxData{
			TxID:     [32]byte{id},
			TxPubKey: sender.TxPubKey,
			Outputs:  []wallet.OutputData{sharedKeyOutput(t, keys, sender.TxPrivKey, 0, amount)},
		}
	}
	if found, _ := scanner.ScanBlock(&wallet.BlockData{Height: height, Transactions: []wallet.TxData{tx(1, large)}}); found != 1 {
		t.Fatalf("first transaction: found %d outputs, want 1", found)
	}
	if found, _ := scanner.ScanBlock(&wallet.BlockData{Height: height + 9, Transactions: []wallet.TxData{tx(2, small)}}); found != 1 {
		t.Fatalf("second transaction: found %d outputs, want 1", found)
	}

	tip := height + 100
	if total := len(w.AllOutputs()); total != 2 {
		t.Fatalf("wallet holds %d outputs, want both copies", total)
	}
	if got := w.Balance(); got != large {
		t.Errorf("Balance = %d, want %d", got, large)
	}
	if got := w.SpendableBalance(tip); got != large {
		t.Errorf("SpendableBalance = %d, want %d", got, large)
	}

	// A send the small copy could cover must spend the large one.
	lease, inputs, err := w.ReserveMatureInputs(tip, 1_000, 0)
	if err != nil {
		t.Fatalf("ReserveMatureInputs: %v", err)
	}
	w.ReleaseInputLease(lease)
	if len(inputs) != 1 || inputs[0].Amount != large {
		t.Fatalf("selected %+v, want the large copy", inputs)
	}

	keyImage, err := GenerateKeyImage(inputs[0].OneTimePrivKey)
	if err != nil {
		t.Fatalf("GenerateKeyImage: %v", err)
	}
	spendBlock := &wallet.BlockData{Height: tip + 1, Transactions: []wallet.TxData{{TxID: [32]byte{3}, KeyImages: [][32]byte{keyImage}}}}
	if _, spent := scanner.ScanBlock(spendBlock); spent != 1 {
		t.Fatalf("spend marked %d, want 1", spent)
	}
	if got := w.Balance(); got != 0 {
		t.Errorf("Balance after spend = %d, want 0", got)
	}
	for _, out := range w.AllOutputs() {
		if !out.Spent {
			t.Errorf("amount %d still unspent after the key was spent", out.Amount)
		}
	}
}

// TestWalletAPIHandlesRepeatedOneTimeKey covers balances, output listings, a dry
// run and a real send for a wallet holding two outputs with one one-time key.
func TestWalletAPIHandlesRepeatedOneTimeKey(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	chain.height = 100
	chain.publishTipLocked()
	chain.mu.Unlock()

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	sender, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create sender wallet: %v", err)
	}
	const large, small = uint64(10_000_000), uint64(1_000_000)
	seedCanonicalSpendableOutput(t, chain, storage, sender, large)
	kept := sender.AllOutputs()[0]
	smallCommit, err := CreatePedersenCommitment(small)
	if err != nil {
		t.Fatalf("failed to create commitment: %v", err)
	}
	sender.AddOutput(&wallet.OwnedOutput{
		TxID:           [32]byte{0x77},
		Amount:         small,
		Blinding:       smallCommit.Blinding,
		OneTimePrivKey: kept.OneTimePrivKey,
		OneTimePubKey:  kept.OneTimePubKey,
		Commitment:     smallCommit.Commitment,
		BlockHeight:    1,
	})

	recipient, err := wallet.NewWallet(filepath.Join(t.TempDir(), "recipient.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create recipient wallet: %v", err)
	}

	api := NewAPIServer(daemon, sender, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)
	const token = "test-token"
	handler := maxBodySize(authMiddleware(token, mux), maxRequestBodyBytes)

	// Sends are rate limited per client, so each request gets its own address.
	requests := 0
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		requests++
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", requests)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	send := func(amount uint64, dryRun bool) *httptest.ResponseRecorder {
		t.Helper()
		return do(http.MethodPost, "/api/wallet/send", fmt.Sprintf(
			`{"recipients":[{"address":%q,"amount":%d,"memo_hex":"00"}],"dry_run":%v}`,
			recipient.Address(), amount, dryRun))
	}

	balance := do(http.MethodGet, "/api/wallet/balance", "")
	var bal struct {
		Spendable, Total uint64
	}
	if err := json.Unmarshal(balance.Body.Bytes(), &bal); err != nil {
		t.Fatalf("decode balance %s: %v", balance.Body.String(), err)
	}
	if bal.Spendable != large || bal.Total != large {
		t.Fatalf("balance = %s, want one coin of %d", balance.Body.String(), large)
	}

	listing := do(http.MethodGet, "/api/wallet/outputs", "")
	var outputs struct {
		Unspent, Unspendable int
		Outputs              []struct {
			Amount uint64
			Status string
		}
	}
	if err := json.Unmarshal(listing.Body.Bytes(), &outputs); err != nil {
		t.Fatalf("decode outputs %s: %v", listing.Body.String(), err)
	}
	if outputs.Unspent != 1 || outputs.Unspendable != 1 {
		t.Fatalf("outputs counts = %+v", outputs)
	}
	for _, out := range outputs.Outputs {
		want := "unspent"
		if out.Amount == small {
			want = "unspendable"
		}
		if out.Status != want {
			t.Fatalf("amount %d status = %q, want %q", out.Amount, out.Status, want)
		}
	}

	history := do(http.MethodGet, "/api/wallet/history", "")
	var hist struct {
		Outputs []struct {
			Amount      uint64
			Unspendable bool
		}
	}
	if err := json.Unmarshal(history.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode history %s: %v", history.Body.String(), err)
	}
	for _, out := range hist.Outputs {
		if out.Unspendable != (out.Amount == small) {
			t.Fatalf("history amount %d unspendable = %v", out.Amount, out.Unspendable)
		}
	}

	// Only one copy can be spent, so asking for more than the kept coin fails
	// up front rather than passing the dry run and failing at the real send.
	if r := send(large+small/2, true); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "insufficient") {
		t.Fatalf("dry run over one coin: %d %s", r.Code, r.Body.String())
	}

	// A send the small copy could cover uses the large one.
	dry := send(1_000, true)
	if dry.Code != http.StatusOK {
		t.Fatalf("dry run: %d %s", dry.Code, dry.Body.String())
	}
	var preview struct {
		InputTotal uint64 `json:"input_total"`
		InputCount int    `json:"input_count"`
	}
	if err := json.Unmarshal(dry.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode dry run: %v", err)
	}
	if preview.InputTotal != large || preview.InputCount != 1 {
		t.Fatalf("dry run selected %+v, want the large copy", preview)
	}

	if r := send(1_000, false); r.Code != http.StatusOK {
		t.Fatalf("send: %d %s", r.Code, r.Body.String())
	}
	if got := sender.Balance(); got != 0 {
		t.Fatalf("balance after send = %d, want 0 until change confirms", got)
	}
	for _, out := range sender.AllOutputs() {
		if !out.Spent {
			t.Fatalf("copy of amount %d still unspent after send", out.Amount)
		}
	}
}
