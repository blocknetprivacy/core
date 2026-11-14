package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// /api/mempool reports the estimate, and a backlog raises it.
func TestAPIFeeRates(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{chain: chain, mempool: NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)}
	api := NewAPIServer(d, w, nil, t.TempDir(), []byte("pw"))

	rates := func() map[string]uint64 {
		rr := httptest.NewRecorder()
		api.handleMempool(rr, httptest.NewRequest(http.MethodGet, "/api/mempool", nil))
		var stats struct {
			FeeRates map[string]uint64 `json:"fee_rates"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil || len(stats.FeeRates) != 2 {
			t.Fatalf("mempool stats: %s", rr.Body.String())
		}
		return stats.FeeRates
	}
	if got := rates(); got["next_block"] != sendFeePerByte || got["within_3_blocks"] != sendFeePerByte {
		t.Fatalf("empty mempool: %v", got)
	}
	// Two blocks of transactions at 30, then one more at 20.
	seedSizedEntries(d.mempool, [2]int{30, 2 * MaxBlockSize}, [2]int{20, MaxBlockSize})
	if got := rates(); got["next_block"] != 31 || got["within_3_blocks"] != 21 {
		t.Fatalf("backlog: %v", got)
	}
}
