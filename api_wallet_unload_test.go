package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

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
