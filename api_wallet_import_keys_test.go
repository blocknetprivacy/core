package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/p2p"
	"blocknet/wallet"
)

func doImportReq(t *testing.T, handler http.Handler, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/wallet/import", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.21:1234"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	var out map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	return rr, out
}

func importKeysFixture(t *testing.T) (http.Handler, *APIServer, *wallet.Wallet, func()) {
	t.Helper()
	chain, _, cleanup := mustCreateTestChain(t)
	mustAddGenesisBlock(t, chain)
	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	src, err := wallet.NewWallet(filepath.Join(t.TempDir(), "src.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("create source wallet: %v", err)
	}
	handler, api := makeCreateServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "wallet.dat"))
	return handler, api, src, func() { stopDaemon(); cleanup() }
}

func TestImportWallet_FromSpendAndViewKeys(t *testing.T) {
	handler, api, src, done := importKeysFixture(t)
	defer done()
	keys := src.Keys()

	rr, body := doImportReq(t, handler, map[string]any{
		"spend_priv": hex.EncodeToString(keys.SpendPrivKey[:]),
		"view_priv":  hex.EncodeToString(keys.ViewPrivKey[:]),
		"password":   "pw123",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if body["address"] != src.Address() {
		t.Fatalf("address %v, want %s", body["address"], src.Address())
	}
	if body["view_only"] != false {
		t.Fatalf("expected a spending wallet, got view_only=%v", body["view_only"])
	}
	api.mu.RLock()
	defer api.mu.RUnlock()
	if api.wallet == nil || api.wallet.IsViewOnly() {
		t.Fatal("expected the imported spending wallet to be loaded")
	}
}

func TestImportWallet_ViewOnlyFromSpendPubAndViewPriv(t *testing.T) {
	handler, api, src, done := importKeysFixture(t)
	defer done()
	keys := src.Keys()

	rr, body := doImportReq(t, handler, map[string]any{
		"spend_pub": hex.EncodeToString(keys.SpendPubKey[:]),
		"view_priv": hex.EncodeToString(keys.ViewPrivKey[:]),
		"password":  "pw123",
		"filename":  "watch.dat",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if body["address"] != src.Address() {
		t.Fatalf("address %v, want %s", body["address"], src.Address())
	}
	if body["view_only"] != true {
		t.Fatalf("expected view_only=true, got %v", body["view_only"])
	}
	api.mu.RLock()
	defer api.mu.RUnlock()
	if api.wallet == nil || !api.wallet.IsViewOnly() {
		t.Fatal("expected the view-only wallet to be loaded")
	}
}

func TestImportWallet_RejectsMixedOrPartialKeys(t *testing.T) {
	handler, _, src, done := importKeysFixture(t)
	defer done()
	keys := src.Keys()
	spendPriv := hex.EncodeToString(keys.SpendPrivKey[:])
	viewPriv := hex.EncodeToString(keys.ViewPrivKey[:])

	for name, body := range map[string]map[string]any{
		"mnemonic plus keys": {"mnemonic": "abandon ability able about above absent absorb abstract absurd abuse access accident", "spend_priv": spendPriv, "view_priv": viewPriv, "password": "pw123"},
		"spend key only":     {"spend_priv": spendPriv, "password": "pw123"},
		"short view key":     {"spend_priv": spendPriv, "view_priv": viewPriv[:10], "password": "pw123"},
		"nothing":            {"password": "pw123"},
	} {
		rr, _ := doImportReq(t, handler, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestStatus_ReportsPeerAddrs(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	cfg := p2p.DefaultNodeConfig()
	cfg.ListenAddrs = []string{"/ip4/0.0.0.0/tcp/0"}
	cfg.SeedNodes = nil
	node, err := p2p.NewNode(cfg)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	defer node.Stop()
	daemon.node = node
	daemon.syncMgr = p2p.NewSyncManager(node, p2p.SyncConfig{})
	want := node.FullMultiaddrs()
	if len(want) == 0 {
		t.Skip("host has no non-loopback address")
	}

	handler, _ := makeCreateServer(t, daemon, nil, nil, filepath.Join(t.TempDir(), "wallet.dat"))
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var body struct {
		PeerID    string   `json:"peer_id"`
		PeerAddrs []string `json:"peer_addrs"`
	}
	json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.PeerAddrs) != len(want) {
		t.Fatalf("peer_addrs %v, want %v", body.PeerAddrs, want)
	}
	for i, a := range body.PeerAddrs {
		if a != want[i] || !strings.HasSuffix(a, "/p2p/"+body.PeerID) {
			t.Fatalf("peer_addrs[%d] = %s, want %s ending in /p2p/%s", i, a, want[i], body.PeerID)
		}
	}
}
