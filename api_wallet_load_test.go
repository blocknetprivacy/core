package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"blocknet/wallet"
)

func makeLoadServer(t *testing.T, daemon *Daemon, w *wallet.Wallet, password []byte, walletFile string) (http.Handler, *APIServer) {
	t.Helper()
	api := NewAPIServer(daemon, w, nil, t.TempDir(), password)
	api.cli = &CLI{walletFile: walletFile}
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)
	token := "test-token"
	api.token = token
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)
	return handler, api
}

func doLoadReq(t *testing.T, handler http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/wallet/load", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.10:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestHandleLoadWallet_Success(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	_, err := wallet.NewWallet(walletFile, []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to pre-create wallet: %v", err)
	}

	handler, api := makeLoadServer(t, daemon, nil, nil, walletFile)

	rr := doLoadReq(t, handler, []byte(`{"password":"correct-password"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["loaded"] != true {
		t.Fatalf("expected loaded=true, got %v", body["loaded"])
	}
	if body["address"] == nil || body["address"] == "" {
		t.Fatal("expected non-empty address in response")
	}
	if body["filename"] != "wallet.dat" {
		t.Fatalf("expected filename=wallet.dat, got %v", body["filename"])
	}

	api.mu.RLock()
	walletSet := api.wallet != nil
	scannerSet := api.scanner != nil
	locked := api.locked
	hashSet := api.passwordHashSet
	api.mu.RUnlock()

	if !walletSet || !scannerSet {
		t.Error("expected wallet and scanner to be published after load")
	}
	if locked {
		t.Error("expected unlocked after load")
	}
	if !hashSet {
		t.Error("expected passwordHashSet after load")
	}

	// CLI state should also be published.
	api.cli.mu.RLock()
	cliOK := api.cli.wallet != nil && api.cli.scanner != nil && api.cli.passwordHashSet
	api.cli.mu.RUnlock()
	if !cliOK {
		t.Error("expected CLI wallet/scanner/passwordHashSet after load")
	}
}

func TestHandleLoadWallet_UnlocksAfterUnloadCycle(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw1"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to pre-create wallet: %v", err)
	}

	handler, api := makeLoadServer(t, daemon, w, []byte("pw1"), walletFile)

	// Unload
	req := httptest.NewRequest("POST", "/api/wallet/unload", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.RemoteAddr = "198.51.100.10:1234"
	unload := httptest.NewRecorder()
	handler.ServeHTTP(unload, req)
	if unload.Code != http.StatusOK {
		t.Fatalf("unload: expected 200, got %d: %s", unload.Code, unload.Body.String())
	}

	api.mu.RLock()
	lockedAfterUnload := api.locked
	api.mu.RUnlock()
	if !lockedAfterUnload {
		t.Fatal("expected locked=true after unload")
	}

	// Re-load
	rr := doLoadReq(t, handler, []byte(`{"password":"pw1"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("load: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	api.mu.RLock()
	locked := api.locked
	api.mu.RUnlock()
	if locked {
		t.Fatal("expected unlocked after re-load")
	}
}

func TestHandleLoadWallet_FilepathResolution(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	_, err := wallet.NewWallet(filepath.Join(dir, "other.dat"), []byte("pw2"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to pre-create wallet: %v", err)
	}

	handler, _ := makeLoadServer(t, daemon, nil, nil, filepath.Join(dir, "default.dat"))

	rr := doLoadReq(t, handler, []byte(`{"password":"pw2","filepath":"other.dat"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	if body["filename"] != "other.dat" {
		t.Fatalf("expected filename=other.dat, got %v", body["filename"])
	}
}

func TestHandleLoadWallet_FileNotFound(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeLoadServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "nonexistent.dat"))

	rr := doLoadReq(t, handler, []byte(`{"password":"correct-password"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoadWallet_WrongPassword(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	_, err := wallet.NewWallet(walletFile, []byte("correct-password"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to pre-create wallet: %v", err)
	}

	handler, api := makeLoadServer(t, daemon, nil, nil, walletFile)

	rr := doLoadReq(t, handler, []byte(`{"password":"wrong-password"}`))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}

	api.mu.RLock()
	walletNil := api.wallet == nil
	loading := api.walletLoading
	api.mu.RUnlock()
	if !walletNil {
		t.Error("wallet should remain nil after wrong password")
	}
	if loading {
		t.Error("walletLoading should be cleared after failure")
	}
}

func TestHandleLoadWallet_AlreadyLoaded409(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw1"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	handler, _ := makeLoadServer(t, daemon, w, []byte("pw1"), walletFile)

	rr := doLoadReq(t, handler, []byte(`{"password":"pw1"}`))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoadWallet_ShortPassword(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeLoadServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	rr := doLoadReq(t, handler, []byte(`{"password":"ab"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoadWallet_InvalidJSON(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeLoadServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	rr := doLoadReq(t, handler, []byte(`not json`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoadWallet_Unauthorized(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeLoadServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	req := httptest.NewRequest("POST", "/api/wallet/load", bytes.NewReader([]byte(`{"password":"pw1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.10:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth token, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLoadWallet_DoesNotCreateFile(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	handler, _ := makeLoadServer(t, daemon, nil, nil, walletFile)

	rr := doLoadReq(t, handler, []byte(`{"password":"correct-password"}`))
	if rr.Code == http.StatusOK {
		t.Fatal("load should not succeed when wallet file does not exist")
	}
	if fileExists(walletFile) {
		t.Fatal("load endpoint must not create a wallet file")
	}
}
