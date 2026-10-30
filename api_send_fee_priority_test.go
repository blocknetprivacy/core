package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"blocknet/wallet"
)

func newFeePriorityTestAPI(t *testing.T) (*APIServer, *wallet.Wallet, *Daemon) {
	t.Helper()
	chain, storage, cleanup := mustCreateTestChain(t)
	t.Cleanup(cleanup)
	mustAddGenesisBlock(t, chain)
	chain.mu.Lock()
	chain.height = 100
	chain.publishTipLocked()
	chain.mu.Unlock()

	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatal(err)
	}
	seedCanonicalSpendableOutput(t, chain, storage, w, 30_000_000)
	d := &Daemon{chain: chain, mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)}
	return NewAPIServer(d, w, nil, t.TempDir(), []byte("pw")), w, d
}

func sendRequest(body string, n int) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/wallet/send", strings.NewReader(body))
	req.RemoteAddr = "198.51.100." + strconv.Itoa(n+1) + ":1234"
	return req
}

func sendBody(addr string, extra string, dryRun bool) string {
	body := `{"recipients":[{"address":"` + addr + `","amount":20000000}],"dry_run":` + strconv.FormatBool(dryRun)
	if extra != "" {
		body += `,"extra_fee_per_byte":` + extra
	}
	return body + "}"
}

func TestHandleSendExtraFeePerByteRaisesDryRunFee(t *testing.T) {
	api, w, _ := newFeePriorityTestAPI(t)
	size := uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize))
	cases := []struct {
		extra string
		rate  uint64
	}{{"", sendFeePerByte}, {"null", sendFeePerByte}, {"0", sendFeePerByte}, {"10", sendFeePerByte + 10}, {"40", sendFeePerByte + 40}, {"1001", sendFeePerByte + 1001}}
	for i, tc := range cases {
		rr := httptest.NewRecorder()
		api.handleSend(rr, sendRequest(sendBody(w.Address(), tc.extra, true), i))
		if rr.Code != http.StatusOK {
			t.Fatalf("extra %q: dry-run returned %d: %s", tc.extra, rr.Code, rr.Body.String())
		}
		var got struct {
			Fee        uint64 `json:"fee"`
			FeePerByte uint64 `json:"fee_per_byte"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Fee != size*tc.rate || got.FeePerByte != tc.rate {
			t.Fatalf("extra %q: fee %d at %d per byte, want %d at %d", tc.extra, got.Fee, got.FeePerByte, size*tc.rate, tc.rate)
		}
	}
}

func TestHandleSendRejectsBadExtraFeePerByte(t *testing.T) {
	api, w, _ := newFeePriorityTestAPI(t)
	tooLarge := strconv.FormatUint(uint64(maxSendFeeRate)-sendFeePerByte+1, 10)
	cases := []struct{ extra, want string }{
		{tooLarge, "too large"},
		{"18446744073709551615", "too large"},
		{"99999999999999999999999", "too large"},
		{"-1", "whole number"},
		{"1.5", "whole number"},
		{"1e3", "whole number"},
		{`"10"`, "whole number"},
		{"true", "whole number"},
	}
	for i, tc := range cases {
		rr := httptest.NewRecorder()
		api.handleSend(rr, sendRequest(sendBody(w.Address(), tc.extra, true), i))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "extra_fee_per_byte") || !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("extra %s: got %d %s, want 400 naming extra_fee_per_byte and %q", tc.extra, rr.Code, rr.Body.String(), tc.want)
		}
	}
}

func TestHandleSendExtraFeePerByteReachesMempool(t *testing.T) {
	api, w, d := newFeePriorityTestAPI(t)
	rr := httptest.NewRecorder()
	api.handleSend(rr, sendRequest(sendBody(w.Address(), "40", false), 0))
	if rr.Code != http.StatusOK {
		t.Fatalf("send returned %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		TxID       string `json:"txid"`
		Fee        uint64 `json:"fee"`
		FeePerByte uint64 `json:"fee_per_byte"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	rate := sendFeePerByte + 40
	if want := uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize)) * rate; got.Fee != want || got.FeePerByte != rate {
		t.Fatalf("fee %d at %d per byte, want %d at %d", got.Fee, got.FeePerByte, want, rate)
	}
	entries := d.mempool.GetTransactionsForBlock(MaxBlockSize, 10)
	if len(entries) != 1 {
		t.Fatalf("mempool holds %d txs, want 1", len(entries))
	}
	if got := entries[0].Fee / uint64(entries[0].Size()); got < rate {
		t.Fatalf("mempool fee rate %d, want at least %d", got, rate)
	}
}

func TestHandleSendAdvancedExtraFeePerByteRaisesDryRunFee(t *testing.T) {
	api, w, _ := newFeePriorityTestAPI(t)
	outs := w.SpendableOutputs()
	if len(outs) != 1 {
		t.Fatalf("wallet holds %d spendable outputs, want 1", len(outs))
	}
	in := `"inputs":[{"txid":"` + hex.EncodeToString(outs[0].TxID[:]) + `","output_index":` + strconv.Itoa(outs[0].OutputIndex) + `}]`
	size := uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize))
	for i, tc := range []struct {
		extra string
		rate  uint64
	}{{"0", sendFeePerByte}, {"40", sendFeePerByte + 40}} {
		body := `{"recipients":[{"address":"` + w.Address() + `","amount":20000000}],"dry_run":true,"extra_fee_per_byte":` + tc.extra + `,` + in + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/wallet/send/advanced", strings.NewReader(body))
		req.RemoteAddr = "198.51.100." + strconv.Itoa(i+1) + ":1234"
		rr := httptest.NewRecorder()
		api.handleSendAdvanced(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("extra %s: dry-run returned %d: %s", tc.extra, rr.Code, rr.Body.String())
		}
		var got struct {
			Fee uint64 `json:"fee"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Fee != size*tc.rate {
			t.Fatalf("extra %s: fee %d, want %d", tc.extra, got.Fee, size*tc.rate)
		}
	}
}

func TestHandleMempoolReportsBaseFee(t *testing.T) {
	api, _, _ := newFeePriorityTestAPI(t)
	rr := httptest.NewRecorder()
	api.handleMempool(rr, httptest.NewRequest(http.MethodGet, "/api/mempool", nil))
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["base_fee_per_byte"] != float64(sendFeePerByte) || got["min_tx_fee"] != float64(sendMinFee) {
		t.Fatalf("mempool fee fields: %v", got)
	}
	if _, ok := got["count"]; !ok {
		t.Fatalf("mempool stats lost their fields: %v", got)
	}
}

func TestHandleSendExplainsFundsErrors(t *testing.T) {
	cases := []struct {
		name    string
		dust    int
		amount  uint64
		extra   string
		want    []string
		notWant string
	}{
		{"fee tips it over", 0, 29_990_000, "0", []string{"insufficient funds: ", "to send plus", "per byte", "spendable"}, "insufficient funds: insufficient funds"},
		{"higher priority tips it over", 0, 29_900_000, "40", []string{"insufficient funds: ", "50 per byte"}, ""},
		{"too many coins", 300, 30_000_000 + 280*100_000, "0", []string{"more than 256 coins", "fee priority"}, ""},
		{"dust at a crazy rate", 200, 35_000_000, "1000", []string{"insufficient funds: ", "to send plus"}, "did not converge"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, w, _ := newFeePriorityTestAPI(t)
			for j := 0; j < tc.dust; j++ {
				// Each coin needs its own one-time key: coins that share one count once.
				w.AddOutput(&wallet.OwnedOutput{OutputIndex: j, Amount: 100_000, BlockHeight: 1, OneTimePubKey: [32]byte{0xd0, byte(j), byte(j >> 8)}})
			}
			body := `{"recipients":[{"address":"` + w.Address() + `","amount":` + strconv.FormatUint(tc.amount, 10) + `}],"dry_run":true,"extra_fee_per_byte":` + tc.extra + `}`
			rr := httptest.NewRecorder()
			api.handleSend(rr, sendRequest(body, i))
			got := rr.Body.String()
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d: %s", rr.Code, got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("error %s lacks %q", got, want)
				}
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Fatalf("error %s still says %q", got, tc.notWant)
			}
		})
	}
}
