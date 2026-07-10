package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"blocknet/p2p"
	"blocknet/wallet"
)

func mustBlockToWalletScanData(t *testing.T, b *Block) *wallet.BlockData {
	t.Helper()

	if b == nil {
		t.Fatal("nil block")
	}

	out := &wallet.BlockData{
		Height:       b.Header.Height,
		Transactions: make([]wallet.TxData, 0, len(b.Transactions)),
	}

	for _, tx := range b.Transactions {
		txid, err := tx.TxID()
		if err != nil {
			t.Fatalf("failed to compute txid: %v", err)
		}

		txData := wallet.TxData{
			TxID:       txid,
			TxPubKey:   tx.TxPublicKey,
			IsCoinbase: tx.IsCoinbase(),
			Outputs:    make([]wallet.OutputData, len(tx.Outputs)),
		}

		for i, o := range tx.Outputs {
			txData.Outputs[i] = wallet.OutputData{
				Index:           i,
				PubKey:          o.PublicKey,
				Commitment:      o.Commitment,
				EncryptedAmount: o.EncryptedAmount,
				EncryptedMemo:   o.EncryptedMemo,
			}
		}

		for _, in := range tx.Inputs {
			txData.KeyImages = append(txData.KeyImages, in.KeyImage)
		}

		out.Transactions = append(out.Transactions, txData)
	}

	return out
}

func TestHandleBlockTemplate_RewardAddressOverrideAlternates(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()
	daemon.syncMgr = new(p2p.SyncManager) // avoid nil deref in handler

	walletAFile := filepath.Join(t.TempDir(), "wallet-a.dat")
	wA, err := wallet.NewWallet(walletAFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet A: %v", err)
	}
	walletBFile := filepath.Join(t.TempDir(), "wallet-b.dat")
	wB, err := wallet.NewWallet(walletBFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet B: %v", err)
	}

	api := NewAPIServer(daemon, wA, nil, t.TempDir(), []byte("pw"))
	templateNow := time.Date(2026, time.July, 10, 12, 0, 0, 0, time.UTC)
	api.templateNow = func() time.Time { return templateNow }
	api.templateTTL = 90 * time.Second
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)

	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	doReq := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "198.51.100.30:1234"
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	scannerA := wallet.NewScanner(wA, defaultScannerConfig())
	scannerB := wallet.NewScanner(wB, defaultScannerConfig())

	type resp struct {
		Block                       *Block `json:"block"`
		RewardAddressUsed           string `json:"reward_address_used"`
		Target                      string `json:"target"`
		HeaderBase                  string `json:"header_base"`
		TemplateID                  string `json:"template_id"`
		TemplateExpiresAtUnixMillis int64  `json:"template_expires_at_unix_ms"`
	}

	// Default: pays to wallet A (loaded wallet)
	{
		rr := doReq("/api/mining/blocktemplate")
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var got resp
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}
		if got.Block == nil || len(got.Block.Transactions) == 0 {
			t.Fatalf("expected non-empty block template")
		}
		if got.RewardAddressUsed != wA.Address() {
			t.Fatalf("reward_address_used mismatch: got %q want %q", got.RewardAddressUsed, wA.Address())
		}
		if got.TemplateID == "" {
			t.Fatal("expected non-empty template_id")
		}
		if want := templateNow.Add(api.templateTTL).UnixMilli(); got.TemplateExpiresAtUnixMillis != want {
			t.Fatalf("template expiry mismatch: got %d want %d", got.TemplateExpiresAtUnixMillis, want)
		}

		foundA, _ := scannerA.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		foundB, _ := scannerB.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		if foundA != 1 {
			t.Fatalf("expected wallet A to find 1 coinbase output, got %d", foundA)
		}
		if foundB != 0 {
			t.Fatalf("expected wallet B to find 0 outputs, got %d", foundB)
		}
	}

	// Override: pays to wallet B
	{
		rr := doReq("/api/mining/blocktemplate?address=" + url.QueryEscape(wB.Address()))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var got resp
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}
		if got.RewardAddressUsed != wB.Address() {
			t.Fatalf("reward_address_used mismatch: got %q want %q", got.RewardAddressUsed, wB.Address())
		}

		foundA, _ := scannerA.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		foundB, _ := scannerB.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		if foundA != 0 {
			t.Fatalf("expected wallet A to find 0 outputs, got %d", foundA)
		}
		if foundB != 1 {
			t.Fatalf("expected wallet B to find 1 coinbase output, got %d", foundB)
		}
	}

	// Override again: back to wallet A
	{
		rr := doReq("/api/mining/blocktemplate?address=" + url.QueryEscape(wA.Address()))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var got resp
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}
		if got.RewardAddressUsed != wA.Address() {
			t.Fatalf("reward_address_used mismatch: got %q want %q", got.RewardAddressUsed, wA.Address())
		}

		foundA, _ := scannerA.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		foundB, _ := scannerB.ScanBlock(mustBlockToWalletScanData(t, got.Block))
		if foundA != 1 {
			t.Fatalf("expected wallet A to find 1 coinbase output, got %d", foundA)
		}
		if foundB != 0 {
			t.Fatalf("expected wallet B to find 0 outputs, got %d", foundB)
		}
	}
}

func TestHandleBlockTemplate_InvalidAddressOverrideReturns400(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()
	daemon.syncMgr = new(p2p.SyncManager) // avoid nil deref in handler

	walletAFile := filepath.Join(t.TempDir(), "wallet-a.dat")
	wA, err := wallet.NewWallet(walletAFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	api := NewAPIServer(daemon, wA, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)

	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	req := httptest.NewRequest(http.MethodGet, "/api/mining/blocktemplate?address=not-a-real-address", nil)
	req.RemoteAddr = "198.51.100.31:1234"
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleBlockTemplate_LiveSameTipCacheFullReturns503(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()
	daemon.syncMgr = new(p2p.SyncManager)

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	now := time.Date(2026, time.July, 10, 19, 0, 0, 0, time.UTC)
	api.templateNow = func() time.Time { return now }
	api.templateTTL = time.Hour

	tip := chain.TemplateParams()
	cached := &Block{Header: BlockHeader{Version: 1, Height: tip.Height, PrevHash: tip.PrevHash}}
	for i := 0; i < maxMiningTemplateCacheEntries; i++ {
		if _, _, err := api.rememberMiningTemplateLease(cached); err != nil {
			t.Fatalf("fill template cache at entry %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/mining/blocktemplate", nil)
	rr := httptest.NewRecorder()
	api.handleBlockTemplate(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	const want = "mining template cache full, retry after a lease expires or the chain tip changes"
	if resp["error"] != want {
		t.Fatalf("unexpected cache-full error: got %q want %q", resp["error"], want)
	}
	if got := resp["code"]; got != "mining_template_cache_full" {
		t.Fatalf("unexpected cache-full code: got %q", got)
	}
	if got := rr.Header().Get("Retry-After"); got != "3600" {
		t.Fatalf("unexpected Retry-After header: got %q want %q", got, "3600")
	}
	if len(api.templateCache) != maxMiningTemplateCacheEntries {
		t.Fatalf("cache size changed on rejected template: got %d", len(api.templateCache))
	}
}
