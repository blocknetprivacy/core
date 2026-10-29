package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"blocknet/wallet"
)

func TestHandleUnloadWallet_NoWallet503(t *testing.T) {
	s := NewAPIServer(nil, nil, nil, t.TempDir(), nil)

	resp := mustMakeHTTPJSONRequest(
		t,
		http.HandlerFunc(s.handleUnloadWallet),
		http.MethodPost,
		"/api/wallet/unload",
		nil,
		nil,
	)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no wallet, got %d: %s", resp.Code, resp.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["error"] != "no wallet loaded" {
		t.Fatalf("unexpected error message: %q", body["error"])
	}
}

func TestHandleUnloadWallet_Success(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	s := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	s.cli = &CLI{walletFile: walletFile}

	resp := mustMakeHTTPJSONRequest(
		t,
		http.HandlerFunc(s.handleUnloadWallet),
		http.MethodPost,
		"/api/wallet/unload",
		nil,
		nil,
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var body map[string]bool
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if !body["unloaded"] {
		t.Fatalf("expected unloaded=true, got %v", body)
	}

	// Server state is fully cleared.
	s.mu.RLock()
	walletNil := s.wallet == nil
	scannerNil := s.scanner == nil
	locked := s.locked
	hashSet := s.passwordHashSet
	var zeroHash [32]byte
	hash := s.passwordHash
	s.mu.RUnlock()

	if !walletNil {
		t.Error("expected s.wallet to be nil after unload")
	}
	if !scannerNil {
		t.Error("expected s.scanner to be nil after unload")
	}
	if !locked {
		t.Error("expected s.locked to be true after unload")
	}
	if hashSet {
		t.Error("expected s.passwordHashSet to be false after unload")
	}
	if hash != zeroHash {
		t.Error("expected s.passwordHash to be zeroed after unload")
	}

	// CLI references are also cleared.
	s.cli.mu.RLock()
	cliWalletNil := s.cli.wallet == nil
	cliScannerNil := s.cli.scanner == nil
	cliHashSet := s.cli.passwordHashSet
	s.cli.mu.RUnlock()

	if !cliWalletNil {
		t.Error("expected cli.wallet to be nil after unload")
	}
	if !cliScannerNil {
		t.Error("expected cli.scanner to be nil after unload")
	}
	if cliHashSet {
		t.Error("expected cli.passwordHashSet to be false after unload")
	}
}

func TestHandleUnloadWallet_SecondCallReturns503(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	s := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	s.cli = &CLI{walletFile: walletFile}

	handler := http.HandlerFunc(s.handleUnloadWallet)

	first := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/wallet/unload", nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first unload: expected 200, got %d: %s", first.Code, first.Body.String())
	}

	second := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/wallet/unload", nil, nil)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second unload: expected 503, got %d: %s", second.Code, second.Body.String())
	}
}

// A catch-up scan running when its wallet is unloaded used to keep scanning
// and re-saving the wallet file until the daemon restarted. It now stops at
// its next step, and the unload returns only after any step or save in
// flight has finished.
func TestHandleUnloadWallet_StopsCatchUpScan(t *testing.T) {
	walletScanTestnet(t)
	chain, _, cleanup := mustCreateTestChain(t)
	t.Cleanup(cleanup)
	mustAddGenesisBlock(t, chain)
	other, err := GenerateStealthKeys()
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h <= 50; h++ {
		cb, err := CreateCoinbase(other.SpendPubKey, other.ViewPubKey, 50_000_000, h)
		if err != nil {
			t.Fatal(err)
		}
		walletScanAppendBlock(t, chain, cb.Tx)
	}
	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	t.Cleanup(stopDaemon)

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatal(err)
	}
	sc := wallet.NewScanner(w, defaultScannerConfig())
	s := NewAPIServer(daemon, w, sc, t.TempDir(), []byte("pw"))
	s.cli = &CLI{ctx: t.Context(), daemon: daemon, walletFile: walletFile, wallet: w, scanner: sc}

	// Hold the scan lock so the catch-up cannot move before the unload.
	s.cli.walletScanMu.Lock()
	scanned := make(chan struct{})
	go func() { s.catchUpScan(); close(scanned) }()
	time.Sleep(100 * time.Millisecond)
	unloaded := make(chan *httptest.ResponseRecorder)
	go func() {
		unloaded <- mustMakeHTTPJSONRequest(t, http.HandlerFunc(s.handleUnloadWallet), http.MethodPost, "/api/wallet/unload", nil, nil)
	}()
	for s.cli.walletLoaded(w) {
		time.Sleep(time.Millisecond)
	}
	s.cli.walletScanMu.Unlock()

	if resp := <-unloaded; resp.Code != http.StatusOK {
		t.Fatalf("unload: %d %s", resp.Code, resp.Body.String())
	}
	before, err := os.Stat(walletFile)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-scanned:
	case <-time.After(10 * time.Second):
		t.Fatal("catch-up scan kept running after unload")
	}
	if h := w.SyncedHeight(); h != 0 {
		t.Fatalf("unloaded wallet scanned to height %d", h)
	}
	after, err := os.Stat(walletFile)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("wallet file written after unload")
	}
}
