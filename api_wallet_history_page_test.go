package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"blocknet/wallet"
)

// TestHandleHistoryPagePaginationContract exercises GET /api/wallet/history/page
// end to end through the real mux + auth middleware. The properties that matter
// for anyone paging this endpoint: a deterministic total order, pages that never
// overlap or skip an output, asc as the exact reverse of desc, limit clamping,
// and 400s on bad input.
func TestHandleHistoryPagePaginationContract(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	chain.mu.Lock()
	chain.height = 300
	chain.publishTipLocked()
	chain.mu.Unlock()

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	type spec struct {
		b      byte
		idx    int
		height uint64
		spent  bool
		memo   []byte
	}
	// Two pending (height 0), a same-height pair (tie-break on txid then idx),
	// a same-txid pair (tie-break on output index), and a spent output carrying
	// a memo. Insertion order deliberately does not match sorted order.
	specs := []spec{
		{b: 0x03, idx: 0, height: 100},
		{b: 0x06, idx: 0, height: 200, spent: true, memo: []byte{0x0a, 0x0b}},
		{b: 0x01, idx: 0, height: 0},
		{b: 0x05, idx: 1, height: 150},
		{b: 0x04, idx: 0, height: 200},
		{b: 0x02, idx: 0, height: 0},
		{b: 0x05, idx: 0, height: 150},
	}
	for _, sp := range specs {
		out := &wallet.OwnedOutput{
			TxID:        [32]byte{sp.b},
			OutputIndex: sp.idx,
			Amount:      uint64(sp.b) * 1000,
			BlockHeight: sp.height,
			Spent:       sp.spent,
			Memo:        sp.memo,
		}
		if sp.spent {
			out.SpentHeight = sp.height + 10
		}
		w.AddOutput(out)
	}

	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)
	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	type pageOutput struct {
		TxID        string `json:"txid"`
		OutputIndex int    `json:"output_index"`
		BlockHeight uint64 `json:"block_height"`
		Spent       bool   `json:"spent"`
		SpentHeight uint64 `json:"spent_height"`
		MemoHex     string `json:"memo_hex"`
	}
	type pageResp struct {
		Count   int          `json:"count"`
		Total   int          `json:"total"`
		Limit   int          `json:"limit"`
		Offset  int          `json:"offset"`
		Order   string       `json:"order"`
		Outputs []pageOutput `json:"outputs"`
	}

	get := func(t *testing.T, query string) (*httptest.ResponseRecorder, pageResp) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/wallet/history/page"+query, nil)
		req.RemoteAddr = "198.51.100.20:1234"
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		var pr pageResp
		if rr.Code == http.StatusOK {
			if err := json.Unmarshal(rr.Body.Bytes(), &pr); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
			}
		}
		return rr, pr
	}

	hexOf := func(b byte) string { return fmt.Sprintf("%x", [32]byte{b}) }
	key := func(txid string, idx int) string { return fmt.Sprintf("%s:%d", txid, idx) }

	// Newest first: pending (height 0) ahead of confirmed, then descending
	// height, ties broken by txid then output index.
	expectedDesc := []string{
		key(hexOf(0x01), 0),
		key(hexOf(0x02), 0),
		key(hexOf(0x04), 0),
		key(hexOf(0x06), 0),
		key(hexOf(0x05), 0),
		key(hexOf(0x05), 1),
		key(hexOf(0x03), 0),
	}

	// Full desc page: order, count/total, and memo/spent passthrough.
	{
		rr, pr := get(t, "?limit=100")
		if rr.Code != http.StatusOK {
			t.Fatalf("desc: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		if pr.Total != 7 || pr.Count != 7 {
			t.Fatalf("desc: total/count want 7/7 got %d/%d", pr.Total, pr.Count)
		}
		got := make([]string, 0, len(pr.Outputs))
		for _, o := range pr.Outputs {
			got = append(got, key(o.TxID, o.OutputIndex))
		}
		for i := range expectedDesc {
			if got[i] != expectedDesc[i] {
				t.Fatalf("desc order mismatch at %d:\n got  %v\n want %v", i, got, expectedDesc)
			}
		}
		for _, o := range pr.Outputs {
			if o.TxID == hexOf(0x06) {
				if o.MemoHex != "0a0b" {
					t.Fatalf("memo_hex want 0a0b got %q", o.MemoHex)
				}
				if !o.Spent || o.SpentHeight != 210 {
					t.Fatalf("spent/spent_height want true/210 got %v/%d", o.Spent, o.SpentHeight)
				}
			}
		}
	}

	// Paging in steps of 2 must reproduce the full desc order exactly, with each
	// output appearing exactly once (no overlap, no skip).
	{
		var paged []string
		for off := 0; off < 100; off += 2 {
			rr, pr := get(t, fmt.Sprintf("?limit=2&offset=%d", off))
			if rr.Code != http.StatusOK {
				t.Fatalf("page offset %d: got %d", off, rr.Code)
			}
			if pr.Total != 7 {
				t.Fatalf("page offset %d: total want 7 got %d", off, pr.Total)
			}
			if pr.Count == 0 {
				break
			}
			for _, o := range pr.Outputs {
				paged = append(paged, key(o.TxID, o.OutputIndex))
			}
		}
		if len(paged) != len(expectedDesc) {
			t.Fatalf("paged length want %d got %d (%v)", len(expectedDesc), len(paged), paged)
		}
		seen := map[string]int{}
		for i := range expectedDesc {
			if paged[i] != expectedDesc[i] {
				t.Fatalf("paged mismatch at %d:\n got  %v\n want %v", i, paged, expectedDesc)
			}
			seen[paged[i]]++
		}
		for _, k := range expectedDesc {
			if seen[k] != 1 {
				t.Fatalf("key %s appeared %d times (want 1)", k, seen[k])
			}
		}
	}

	// asc is the exact reverse of desc.
	{
		_, pr := get(t, "?limit=100&order=asc")
		if pr.Count != 7 {
			t.Fatalf("asc count want 7 got %d", pr.Count)
		}
		for i, o := range pr.Outputs {
			want := expectedDesc[len(expectedDesc)-1-i]
			if key(o.TxID, o.OutputIndex) != want {
				t.Fatalf("asc[%d] got %s want %s", i, key(o.TxID, o.OutputIndex), want)
			}
		}
	}

	// Invalid order → 400.
	{
		rr, _ := get(t, "?order=sideways")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid order want 400 got %d", rr.Code)
		}
	}

	// Offset past the end → empty page, total still reported.
	{
		_, pr := get(t, "?offset=100")
		if pr.Count != 0 || pr.Total != 7 {
			t.Fatalf("offset past end: want count 0 total 7 got %d/%d", pr.Count, pr.Total)
		}
	}

	// limit above 500 is clamped to 500; the effective limit is echoed back.
	{
		_, pr := get(t, "?limit=99999")
		if pr.Limit != 500 {
			t.Fatalf("limit clamp want 500 got %d", pr.Limit)
		}
	}

	// Default page size is 20 when limit is omitted.
	{
		_, pr := get(t, "")
		if pr.Limit != 20 {
			t.Fatalf("default limit want 20 got %d", pr.Limit)
		}
	}
}
