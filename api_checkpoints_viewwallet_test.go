package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/wallet"
)

func decodeJSONMap(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("failed to decode JSON response: %v (body: %s)", err, rr.Body.String())
	}
	return m
}

// TestHandleViewKeys_CreateViewWalletFile verifies the extended viewkeys endpoint
// both returns the keys and writes a loadable view-only wallet file encrypted with
// its own password (never the main wallet password), and 409s on a duplicate.
func TestHandleViewKeys_CreateViewWalletFile(t *testing.T) {
	dir := t.TempDir()
	walletFile := filepath.Join(dir, "main.wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("main-pass"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("new wallet: %v", err)
	}

	s := NewAPIServer(nil, w, nil, dir, []byte("main-pass"))
	s.cli = &CLI{walletFile: walletFile}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "203.0.113.5:1234"
		s.handleViewKeys(w, r)
	})

	body := []byte(`{"password":"main-pass","create_file":true,"file_password":"view-pass"}`)
	rr := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/wallet/viewkeys", body,
		map[string]string{"Content-Type": "application/json"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSONMap(t, rr)
	if resp["created"] != true {
		t.Fatalf("expected created=true, got %v", resp["created"])
	}
	if resp["view_priv"] == nil || resp["view_priv"] == "" {
		t.Fatal("expected view_priv in response alongside the created file")
	}

	viewPath := filepath.Join(dir, "main.view.wallet.dat")
	if _, err := os.Stat(viewPath); err != nil {
		t.Fatalf("view-only wallet file not created at %s: %v", viewPath, err)
	}

	// The created file must be loadable and view-only with its OWN password.
	vw, err := wallet.LoadWallet(viewPath, []byte("view-pass"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("load created view wallet with its own password: %v", err)
	}
	if !vw.IsViewOnly() {
		t.Fatal("created wallet is not view-only")
	}
	// It must NOT open with the main wallet password — separate credentials.
	if _, err := wallet.LoadWallet(viewPath, []byte("main-pass"), defaultWalletConfig()); err == nil {
		t.Fatal("created view wallet must not open with the main wallet password")
	}

	// A second create for the same file must 409.
	rr2 := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/wallet/viewkeys", body,
		map[string]string{"Content-Type": "application/json"})
	if rr2.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate create, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// TestHandleViewKeys_CreateRequiresFilePassword checks that create_file without a
// valid file_password is rejected (and no file is written).
func TestHandleViewKeys_CreateRequiresFilePassword(t *testing.T) {
	dir := t.TempDir()
	walletFile := filepath.Join(dir, "main.wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("main-pass"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("new wallet: %v", err)
	}
	s := NewAPIServer(nil, w, nil, dir, []byte("main-pass"))
	s.cli = &CLI{walletFile: walletFile}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "203.0.113.6:1234"
		s.handleViewKeys(w, r)
	})

	body := []byte(`{"password":"main-pass","create_file":true,"file_password":"no"}`)
	rr := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/wallet/viewkeys", body,
		map[string]string{"Content-Type": "application/json"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short file_password, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "main.view.wallet.dat")); err == nil {
		t.Fatal("view wallet file should not exist after a rejected create")
	}
}

// TestHandleLoadCheckpoints_FromFile writes a checkpoints file and verifies the
// endpoint loads it (no download) and reports the count and max height.
func TestHandleLoadCheckpoints_FromFile(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	daemon, stop := mustStartTestDaemon(t, chain)
	defer stop()

	dir := t.TempDir()
	content := "100:" + strings.Repeat("AB", 32) + "\n200:" + strings.Repeat("CD", 32) + "\n"
	if err := os.WriteFile(checkpointsPath(dir), []byte(content), 0o644); err != nil {
		t.Fatalf("write checkpoints file: %v", err)
	}

	s := NewAPIServer(daemon, nil, nil, dir, nil)
	handler := http.HandlerFunc(s.handleLoadCheckpoints)

	rr := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/checkpoints/load", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSONMap(t, rr)
	if resp["loaded"].(float64) != 2 {
		t.Fatalf("expected loaded=2, got %v", resp["loaded"])
	}
	if resp["max_height"].(float64) != 200 {
		t.Fatalf("expected max_height=200, got %v", resp["max_height"])
	}
	if resp["downloaded"] != false {
		t.Fatalf("expected downloaded=false (file present), got %v", resp["downloaded"])
	}
}

// TestHandleSaveCheckpoints_EmptyChain verifies the save endpoint is a clean no-op
// when there is nothing above the genesis to checkpoint.
func TestHandleSaveCheckpoints_EmptyChain(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	daemon, stop := mustStartTestDaemon(t, chain)
	defer stop()

	dir := t.TempDir()
	s := NewAPIServer(daemon, nil, nil, dir, nil)
	handler := http.HandlerFunc(s.handleSaveCheckpoints)

	rr := mustMakeHTTPJSONRequest(t, handler, http.MethodPost, "/api/checkpoints/save", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSONMap(t, rr)
	if resp["written"].(float64) != 0 {
		t.Fatalf("expected written=0 on empty chain, got %v", resp["written"])
	}
}
