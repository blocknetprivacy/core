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

func makeCreateServer(t *testing.T, daemon *Daemon, w *wallet.Wallet, password []byte, walletFile string) (http.Handler, *APIServer) {
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

func doCreateReq(t *testing.T, handler http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/wallet/create", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.20:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestHandleCreateWallet_Success(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	handler, api := makeCreateServer(t, daemon, nil, nil, walletFile)

	rr := doCreateReq(t, handler, []byte(`{"password":"my-password"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["created"] != true {
		t.Fatalf("expected created=true, got %v", body["created"])
	}
	if body["address"] == nil || body["address"] == "" {
		t.Fatal("expected non-empty address")
	}
	if body["filename"] != "wallet.dat" {
		t.Fatalf("expected filename=wallet.dat, got %v", body["filename"])
	}
	if !fileExists(walletFile) {
		t.Fatal("expected wallet file to exist on disk")
	}

	api.mu.RLock()
	walletSet := api.wallet != nil
	scannerSet := api.scanner != nil
	locked := api.locked
	hashSet := api.passwordHashSet
	api.mu.RUnlock()

	if !walletSet || !scannerSet {
		t.Error("expected wallet and scanner to be published after create")
	}
	if locked {
		t.Error("expected unlocked after create")
	}
	if !hashSet {
		t.Error("expected passwordHashSet after create")
	}

	api.cli.mu.RLock()
	cliOK := api.cli.wallet != nil && api.cli.scanner != nil && api.cli.passwordHashSet
	api.cli.mu.RUnlock()
	if !cliOK {
		t.Error("expected CLI wallet/scanner/passwordHashSet after create")
	}
}

func TestHandleCreateWallet_CustomFilename(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	handler, _ := makeCreateServer(t, daemon, nil, nil, filepath.Join(dir, "default.dat"))

	rr := doCreateReq(t, handler, []byte(`{"password":"my-password","filename":"custom.dat"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	if body["filename"] != "custom.dat" {
		t.Fatalf("expected filename=custom.dat, got %v", body["filename"])
	}
	if !fileExists(filepath.Join(dir, "custom.dat")) {
		t.Fatal("expected custom.dat on disk")
	}
}

func TestHandleCreateWallet_FileAlreadyExists409(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")
	_, err := wallet.NewWallet(walletFile, []byte("pw1"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to pre-create wallet: %v", err)
	}

	handler, _ := makeCreateServer(t, daemon, nil, nil, walletFile)

	rr := doCreateReq(t, handler, []byte(`{"password":"my-password"}`))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	json.Unmarshal(rr.Body.Bytes(), &body)
	if body["error"] != "wallet file already exists: wallet.dat" {
		t.Fatalf("unexpected error: %q", body["error"])
	}
}

func TestHandleCreateWallet_AlreadyLoaded409(t *testing.T) {
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

	handler, _ := makeCreateServer(t, daemon, w, []byte("pw1"), filepath.Join(dir, "other.dat"))

	rr := doCreateReq(t, handler, []byte(`{"password":"my-password"}`))
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreateWallet_ShortPassword(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeCreateServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	rr := doCreateReq(t, handler, []byte(`{"password":"ab"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreateWallet_InvalidJSON(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeCreateServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	rr := doCreateReq(t, handler, []byte(`{bad`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreateWallet_Unauthorized(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler, _ := makeCreateServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "w.dat"))

	req := httptest.NewRequest("POST", "/api/wallet/create", bytes.NewReader([]byte(`{"password":"pw1"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.20:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth token, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCreateWallet_LoadableAfterCreate(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	dir := t.TempDir()
	walletFile := filepath.Join(dir, "wallet.dat")

	api := NewAPIServer(daemon, nil, nil, t.TempDir(), nil)
	api.cli = &CLI{walletFile: walletFile}
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)
	token := "test-token"
	api.token = token
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	authHeaders := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
	doReq := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "198.51.100.30:1234"
		for k, v := range authHeaders {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	// Create
	create := doReq("POST", "/api/wallet/create", []byte(`{"password":"my-password"}`))
	if create.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", create.Code, create.Body.String())
	}
	var createBody map[string]any
	json.Unmarshal(create.Body.Bytes(), &createBody)
	createdAddr := createBody["address"]

	// Unload
	unload := doReq("POST", "/api/wallet/unload", nil)
	if unload.Code != http.StatusOK {
		t.Fatalf("unload: expected 200, got %d: %s", unload.Code, unload.Body.String())
	}

	// Load the same wallet back
	load := doReq("POST", "/api/wallet/load", []byte(`{"password":"my-password"}`))
	if load.Code != http.StatusOK {
		t.Fatalf("load: expected 200, got %d: %s", load.Code, load.Body.String())
	}
	var loadBody map[string]any
	json.Unmarshal(load.Body.Bytes(), &loadBody)
	if loadBody["address"] != createdAddr {
		t.Fatalf("address mismatch after create→unload→load: %v != %v", loadBody["address"], createdAddr)
	}
}
