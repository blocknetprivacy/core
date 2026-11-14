package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"blocknet/protocol/params"
	"blocknet/wallet"
)

type proofRegressionFixture struct {
	tx        *Transaction
	txPriv    [32]byte
	recipient *StealthKeys
	other     *StealthKeys
	amounts   []uint64
	memos     []string
}

func proofRegressionKeys(t *testing.T) *StealthKeys {
	t.Helper()
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// Build fixtures through the sender's primitives, independently of the proof
// matcher: H(r*A[, index])*G + B, a real commitment, and encrypted amount/memo.
// Inputs are placeholders because the synthetic mempool bypasses admission;
// these tests exercise proof lookup/decoding, not transaction validation.
func newProofRegressionFixture(t *testing.T, indexed bool) proofRegressionFixture {
	t.Helper()
	f := proofRegressionFixture{
		recipient: proofRegressionKeys(t),
		other:     proofRegressionKeys(t),
		amounts:   []uint64{125_000_001, 987_654_321, 250_000_007},
		memos:     []string{"invoice alpha", "another recipient", "invoice beta"},
	}
	txKey, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatal(err)
	}
	f.txPriv = txKey.PrivateKey
	f.tx = &Transaction{Version: 1, TxPublicKey: txKey.PublicKey, Inputs: []TxInput{{}}}
	for i, recipient := range []*StealthKeys{f.recipient, f.other, f.recipient} {
		f.tx.Outputs = append(f.tx.Outputs, proofRegressionOutput(t, f.txPriv, recipient, i, f.amounts[i], f.memos[i], indexed))
	}
	return f
}

func proofRegressionOutput(t *testing.T, txPriv [32]byte, recipient *StealthKeys, index int, amount uint64, memo string, indexed bool) TxOutput {
	t.Helper()
	var secret [32]byte
	var err error
	if indexed {
		secret, err = DeriveStealthSecretSenderIndexed(txPriv, recipient.ViewPubKey, uint32(index))
	} else {
		secret, err = DeriveStealthSecretSender(txPriv, recipient.ViewPubKey)
	}
	if err != nil {
		t.Fatal(err)
	}
	point, err := ScalarToPubKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := CommitmentAdd(point, recipient.SpendPubKey)
	if err != nil {
		t.Fatal(err)
	}
	blinding := wallet.DeriveBlinding(secret, index)
	commitment, err := CreatePedersenCommitmentWithBlinding(amount, blinding)
	if err != nil {
		t.Fatal(err)
	}
	encryptedMemo, err := wallet.EncryptMemo([]byte(memo), secret, index)
	if err != nil {
		t.Fatal(err)
	}
	return TxOutput{
		PublicKey: pub, Commitment: commitment,
		EncryptedAmount: EncryptAmount(amount, blinding, index), EncryptedMemo: encryptedMemo,
	}
}

func (f proofRegressionFixture) wantOutputs(matched bool, coinUnits bool) []map[string]any {
	want := make([]map[string]any, len(f.tx.Outputs))
	for i := range want {
		want[i] = map[string]any{"index": i, "match": matched && i != 1}
		if matched && i != 1 {
			want[i]["amount"] = f.amounts[i]
			if coinUnits {
				want[i]["amount"] = float64(f.amounts[i]) / 100_000_000
			}
			if f.memos[i] != "" {
				want[i]["memo"] = f.memos[i]
			}
		}
	}
	return want
}

func TestProofRegressionDerivationCompatibility(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, height := range []uint64{0, params.IndexedDerivationHeight - 1, params.IndexedDerivationHeight, params.IndexedDerivationHeight + 1, params.IndexedDerivationHeight + 127, params.IndexedDerivationHeight + 128, params.IndexedDerivationHeight + 100_000} {
			t.Run(fmt.Sprintf("indexed=%t/height=%d", indexed, height), func(t *testing.T) {
				f := newProofRegressionFixture(t, indexed)
				got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, height)
				if want := f.wantOutputs(true, false); !reflect.DeepEqual(got, want) {
					t.Fatalf("outputs = %#v, want %#v", got, want)
				}
				if want := f.amounts[0] + f.amounts[2]; total != want {
					t.Fatalf("total = %d, want %d", total, want)
				}
			})
		}
	}
}

func TestProofRegressionInputCountIndependence(t *testing.T) {
	f := newProofRegressionFixture(t, true)
	for _, count := range []int{1, 126, 252, 256} {
		t.Run(fmt.Sprintf("inputs=%d", count), func(t *testing.T) {
			// Only the input count changes. These placeholders test proof matching,
			// not consensus acceptance of an unsigned transaction of this size.
			tx := Transaction{
				Version:     f.tx.Version,
				TxPublicKey: f.tx.TxPublicKey,
				Inputs:      make([]TxInput, count),
				Outputs:     f.tx.Outputs,
			}
			got, total := matchProofOutputs(&tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, 0)
			if want := f.wantOutputs(true, false); !reflect.DeepEqual(got, want) || total != f.amounts[0]+f.amounts[2] {
				t.Fatalf("%d inputs returned %#v, total %d; want %#v", count, got, total, want)
			}
		})
	}
}

func TestProofRegressionRejectsUnrelatedKeysAndIndices(t *testing.T) {
	f := newProofRegressionFixture(t, true)
	stranger := proofRegressionKeys(t)
	var malformed [32]byte
	for i := range malformed {
		malformed[i] = 0xff
	}
	for _, tc := range []struct {
		name                string
		txPriv, spend, view [32]byte
	}{
		{"wrong spend key", f.txPriv, stranger.SpendPubKey, f.recipient.ViewPubKey},
		{"wrong view key", f.txPriv, f.recipient.SpendPubKey, stranger.ViewPubKey},
		{"wrong transaction key", stranger.SpendPrivKey, f.recipient.SpendPubKey, f.recipient.ViewPubKey},
		{"malformed spend point", f.txPriv, malformed, f.recipient.ViewPubKey},
		{"malformed view point", f.txPriv, f.recipient.SpendPubKey, malformed},
		{"noncanonical scalar", malformed, f.recipient.SpendPubKey, f.recipient.ViewPubKey},
		{"zero scalar", [32]byte{}, f.recipient.SpendPubKey, f.recipient.ViewPubKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, total := matchProofOutputs(f.tx, tc.txPriv, tc.spend, tc.view, false, 0)
			if !reflect.DeepEqual(got, f.wantOutputs(false, false)) || total != 0 {
				t.Fatalf("unrelated key revealed outputs: %#v, total %d", got, total)
			}
		})
	}
	// The same recipient owns outputs 0 and 2, but their indexed one-time keys
	// cannot be moved to another position and still match.
	f.tx.Outputs[0], f.tx.Outputs[2] = f.tx.Outputs[2], f.tx.Outputs[0]
	got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, 0)
	if !reflect.DeepEqual(got, f.wantOutputs(false, false)) || total != 0 {
		t.Fatalf("reordered indexed outputs matched: %#v, total %d", got, total)
	}
}

func TestProofRegressionEmptyAndMixedOutputs(t *testing.T) {
	t.Run("no outputs", func(t *testing.T) {
		f := newProofRegressionFixture(t, true)
		f.tx.Outputs = nil
		got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, 0)
		if got == nil || len(got) != 0 || total != 0 {
			t.Fatalf("empty transaction returned %#v, total %d; want non-nil empty slice, zero total", got, total)
		}
	})
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty memo/indexed=%t", indexed), func(t *testing.T) {
			f := newProofRegressionFixture(t, indexed)
			f.memos[2] = ""
			f.tx.Outputs[2] = proofRegressionOutput(t, f.txPriv, f.recipient, 2, f.amounts[2], "", indexed)
			got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, 0)
			if want := f.wantOutputs(true, false); !reflect.DeepEqual(got, want) || total != f.amounts[0]+f.amounts[2] {
				t.Fatalf("empty memo returned %#v, total %d; want %#v", got, total, want)
			}
		})
	}
	t.Run("mixed derivations", func(t *testing.T) {
		f := newProofRegressionFixture(t, true)
		f.tx.Outputs[0] = proofRegressionOutput(t, f.txPriv, f.recipient, 0, f.amounts[0], f.memos[0], false)
		got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, params.IndexedDerivationHeight+100_000)
		if want := f.wantOutputs(true, false); !reflect.DeepEqual(got, want) || total != f.amounts[0]+f.amounts[2] {
			t.Fatalf("mixed derivations returned %#v, total %d; want %#v", got, total, want)
		}
	})
}

func TestProofRegressionTamperedMemo(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			f := newProofRegressionFixture(t, indexed)
			// Corrupt the encrypted envelope version, guaranteeing rejection rather
			// than relying on a random checksum collision not happening.
			f.tx.Outputs[0].EncryptedMemo[0] ^= 0xff
			want := f.wantOutputs(true, false)
			delete(want[0], "memo")
			got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, false, 0)
			if !reflect.DeepEqual(got, want) || total != f.amounts[0]+f.amounts[2] {
				t.Fatalf("tampered memo response = %#v, total %d; want %#v", got, total, want)
			}
		})
	}
}

func TestProofRegressionCoinbaseConsensusBlinding(t *testing.T) {
	for _, height := range []uint64{params.IndexedDerivationHeight - 1, params.IndexedDerivationHeight + 1} {
		t.Run(fmt.Sprintf("height=%d", height), func(t *testing.T) {
			f := newProofRegressionFixture(t, false)
			f.tx.Inputs = nil
			f.tx.Outputs = f.tx.Outputs[:1]
			const reward = uint64(5_000_000_000)
			// Use the consensus producer's derivation, not the wallet helper used
			// by the proof verifier, so this also catches accidental KDF drift.
			blinding := deriveCoinbaseConsensusBlinding(f.tx.TxPublicKey, height, 0)
			commitment, err := CreatePedersenCommitmentWithBlinding(reward, blinding)
			if err != nil {
				t.Fatal(err)
			}
			memo, err := wallet.EncryptMemo(nil, blinding, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.tx.Outputs[0].Commitment = commitment
			f.tx.Outputs[0].EncryptedAmount = EncryptAmount(reward, blinding, 0)
			f.tx.Outputs[0].EncryptedMemo = memo
			got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, true, height)
			want := []map[string]any{{"index": 0, "match": true, "amount": reward}}
			if !reflect.DeepEqual(got, want) || total != reward {
				t.Fatalf("coinbase response = %#v, total %d; want %#v", got, total, want)
			}
		})
	}
	t.Run("indexed coinbase does not match", func(t *testing.T) {
		f := newProofRegressionFixture(t, true)
		f.tx.Inputs = nil
		got, total := matchProofOutputs(f.tx, f.txPriv, f.recipient.SpendPubKey, f.recipient.ViewPubKey, true, params.IndexedDerivationHeight+1)
		if !reflect.DeepEqual(got, f.wantOutputs(false, false)) || total != 0 {
			t.Fatalf("coinbase accepted indexed outputs: %#v, total %d", got, total)
		}
	})
}

func proofRegressionCheckJSON(t *testing.T, rr *httptest.ResponseRecorder, status int, want any) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("HTTP status = %d, want %d: %s", rr.Code, status, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q, want JSON", rr.Header().Get("Content-Type"))
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got, normalizedWant any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &normalizedWant); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, normalizedWant) {
		t.Fatalf("response = %s, want %s", rr.Body.String(), wantJSON)
	}
}

func TestProofRegressionMempoolHandlers(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	daemon, stop := mustStartTestDaemon(t, chain)
	defer stop()
	api := NewAPIServer(daemon, nil, nil, t.TempDir(), nil)
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	explorer := NewExplorer(daemon)
	requestCount := 0
	request := func(handler http.Handler, path, contentType, body string) *httptest.ResponseRecorder {
		requestCount++
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		// Do not make crypto assertions depend on the per-IP request budget.
		req.RemoteAddr = fmt.Sprintf("10.1.0.%d:1234", requestCount)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			f := newProofRegressionFixture(t, indexed)
			txID := mustSeedMempoolTx(t, daemon.mempool, f.tx)
			txHash := fmt.Sprintf("%x", txID)
			if _, _, found := chain.FindTxByHashStr(txHash); found {
				t.Fatal("fixture unexpectedly resolved from the chain instead of the mempool")
			}
			stranger := proofRegressionKeys(t)
			for _, tc := range []struct {
				name      string
				recipient *StealthKeys
				txPriv    [32]byte
				matched   bool
				valid     bool
			}{
				{"valid", f.recipient, f.txPriv, true, true},
				{"wrong address", stranger, f.txPriv, false, true},
				{"wrong transaction key", f.recipient, stranger.SpendPrivKey, false, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					addressKeys := wallet.StealthKeys{SpendPubKey: tc.recipient.SpendPubKey, ViewPubKey: tc.recipient.ViewPubKey}
					body, err := json.Marshal(map[string]string{"txid": txHash, "tx_key": fmt.Sprintf("%x", tc.txPriv), "address": addressKeys.Address()})
					if err != nil {
						t.Fatal(err)
					}
					var total uint64
					outputs := f.wantOutputs(tc.matched, false)
					if tc.matched {
						total = f.amounts[0] + f.amounts[2]
					}
					if !tc.valid {
						outputs = []map[string]any{}
					}
					t.Run("API", func(t *testing.T) {
						rr := request(mux, "/api/verify-proof", "application/json", string(body))
						proofRegressionCheckJSON(t, rr, http.StatusOK, map[string]any{"txid": txHash, "valid": tc.valid, "total_matched": total, "outputs": outputs})
					})
					t.Run("sender explorer", func(t *testing.T) {
						form := url.Values{"tx": {txHash}, "tx_priv": {fmt.Sprintf("%x", tc.txPriv)}, "spend_pub": {fmt.Sprintf("%x", tc.recipient.SpendPubKey)}, "view_pub": {fmt.Sprintf("%x", tc.recipient.ViewPubKey)}}
						rr := request(explorer, "/prove-send", "application/x-www-form-urlencoded", form.Encode())
						if !tc.valid {
							proofRegressionCheckJSON(t, rr, http.StatusBadRequest, map[string]string{"error": "tx private key does not match this transaction's public key"})
							return
						}
						proofRegressionCheckJSON(t, rr, http.StatusOK, f.wantOutputs(tc.matched, true))
					})
					if tc.valid { // Receiver proof has no sender key to validate.
						t.Run("receiver explorer", func(t *testing.T) {
							form := url.Values{"tx": {txHash}, "spend_pub": {fmt.Sprintf("%x", tc.recipient.SpendPubKey)}, "view_priv": {fmt.Sprintf("%x", tc.recipient.ViewPrivKey)}}
							rr := request(explorer, "/prove", "application/x-www-form-urlencoded", form.Encode())
							proofRegressionCheckJSON(t, rr, http.StatusOK, f.wantOutputs(tc.matched, true))
						})
					}
				})
			}
		})
	}
}
