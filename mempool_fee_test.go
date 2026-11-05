package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/wallet"
)

// The estimate is the rate that ranks a new transaction inside the given
// number of blocks, as templates rank by fee rate.
func TestMempoolFeeRateFor(t *testing.T) {
	m := NewMempool(DefaultMempoolConfig(), nil, nil)
	if got := m.FeeRateFor(1, 1000, 10, 100, 10); got != 10 {
		t.Fatalf("empty mempool: %d", got)
	}
	// 1000-byte blocks: 600 bytes at 50, then 300 at 20, then 500 at 10.
	seedSizedEntries(m, [2]int{50, 300}, [2]int{50, 300}, [2]int{20, 300}, [2]int{10, 500})
	if got := m.FeeRateFor(1, 1000, 10, 100, 10); got != 11 {
		t.Fatalf("next block, 100 bytes fit after the 20s: %d", got)
	}
	if got := m.FeeRateFor(1, 1000, 10, 200, 10); got != 21 {
		t.Fatalf("next block, 200 bytes only fit ahead of the 20s: %d", got)
	}
	if got := m.FeeRateFor(2, 1000, 10, 100, 10); got != 10 {
		t.Fatalf("two blocks hold everything: %d", got)
	}
	if got := m.FeeRateFor(1, 1000, 2, 100, 10); got != 51 {
		t.Fatalf("next block with two slots: %d", got)
	}
	m2 := NewMempool(DefaultMempoolConfig(), nil, nil)
	seedSizedEntries(m2, [2]int{5, 5000})
	if got := m2.FeeRateFor(1, 1000, 10, 100, 10); got != 10 {
		t.Fatalf("below-floor backlog: %d", got)
	}
}

// /api/mempool reports the estimate, and a send's fee_per_byte sets its fee
// within the allowed range.
func TestAPIFeeRates(t *testing.T) {
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
	seedCanonicalSpendableOutput(t, chain, storage, w, 30_000_000)
	d := &Daemon{chain: chain, mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)}
	api := NewAPIServer(d, w, nil, t.TempDir(), []byte("pw"))

	rr := httptest.NewRecorder()
	api.handleMempool(rr, httptest.NewRequest(http.MethodGet, "/api/mempool", nil))
	var stats struct {
		Count    int               `json:"count"`
		FeeRates map[string]uint64 `json:"fee_rates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil || stats.FeeRates["next_block"] != sendFeePerByte || stats.FeeRates["within_3_blocks"] != sendFeePerByte {
		t.Fatalf("mempool stats: %s", rr.Body.String())
	}

	calls := 0
	send := func(rate string) (int, uint64) {
		body := `{"recipients":[{"address":"` + w.Address() + `","amount":20000000}],"dry_run":true` + rate + `}`
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/wallet/send", strings.NewReader(body))
		calls++
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", calls) // past the per-client send limit
		api.handleSend(rr, req)
		var got struct {
			Fee uint64 `json:"fee"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		return rr.Code, got.Fee
	}
	size := uint64(wallet.EstimateTxSizeBytes(1, 2, RingSize))
	if code, fee := send(""); code != http.StatusOK || fee != size*sendFeePerByte {
		t.Fatalf("default rate: HTTP %d fee %d", code, fee)
	}
	if code, fee := send(`,"fee_per_byte":50`); code != http.StatusOK || fee != size*50 {
		t.Fatalf("rate 50: HTTP %d fee %d, want %d", code, fee, size*50)
	}
	for _, bad := range []string{`,"fee_per_byte":5`, `,"fee_per_byte":1001`} {
		if code, _ := send(bad); code != http.StatusBadRequest {
			t.Fatalf("%s: HTTP %d", bad, code)
		}
	}
}
