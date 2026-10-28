package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blocknet/p2p"
	"blocknet/wallet"
)

func TestHandleBlockTemplate_StartupPeerTipGate(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	payee, err := wallet.NewWallet(filepath.Join(t.TempDir(), "payee.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create payee wallet: %v", err)
	}
	path := "/api/mining/blocktemplate?address=" + url.QueryEscape(payee.Address())

	// newGatedAPI returns a server whose daemon has not passed the startup gate.
	newGatedAPI := func(t *testing.T) (*APIServer, *Daemon) {
		t.Helper()
		daemon, stopDaemon := mustStartTestDaemon(t, chain)
		t.Cleanup(stopDaemon)
		daemon.syncMgr = new(p2p.SyncManager)
		daemon.tipGateOpen.Store(false)
		return NewAPIServer(daemon, nil, nil, t.TempDir(), nil), daemon
	}
	get := func(t *testing.T, api *APIServer) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		api.handleBlockTemplate(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}
	wantStarting := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 while waiting for a peer tip, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp["code"] != "node_starting" {
			t.Fatalf("unexpected code: got %q want node_starting", resp["code"])
		}
		// Pools treat 503 bodies mentioning a wallet as a cue to load one.
		if strings.Contains(strings.ToLower(resp["error"]), "wallet") {
			t.Fatalf("node_starting message must not mention a wallet: %q", resp["error"])
		}
	}
	wantOK := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	}

	t.Run("closed before sync starts", func(t *testing.T) {
		api, _ := newGatedAPI(t)
		wantStarting(t, get(t, api))
	})

	t.Run("opens once a peer tip is checked", func(t *testing.T) {
		api, daemon := newGatedAPI(t)
		start := time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)
		daemon.syncStartedAt.Store(&start)
		daemon.tipGateNow = func() time.Time { return start.Add(time.Second) }
		var checked bool
		daemon.peerTipChecked = func() bool { return checked }

		wantStarting(t, get(t, api))
		checked = true
		wantOK(t, get(t, api))
	})

	t.Run("opens at the cap without peers", func(t *testing.T) {
		api, daemon := newGatedAPI(t)
		start := time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)
		daemon.syncStartedAt.Store(&start)
		now := start.Add(blockTemplatePeerTipWait - time.Millisecond)
		daemon.tipGateNow = func() time.Time { return now }
		daemon.peerTipChecked = func() bool { return false }

		wantStarting(t, get(t, api))
		now = start.Add(blockTemplatePeerTipWait)
		wantOK(t, get(t, api))
	})

	t.Run("stays open", func(t *testing.T) {
		api, daemon := newGatedAPI(t)
		checked := true
		daemon.peerTipChecked = func() bool { return checked }
		wantOK(t, get(t, api))

		// Neither signal holds any more; the gate must not close again.
		checked = false
		daemon.syncStartedAt.Store(nil)
		wantOK(t, get(t, api))
	})
}
