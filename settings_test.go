package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

const testWhitelistPeer = "12D3KooWQUNGJrsU5nRXNk45FT3ZumdtWC9Sg9Xt2AgU3XkP382R"

func newSettingsTestServer(t *testing.T) (*APIServer, *Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := NewDaemon(DaemonConfig{DataDir: dir, ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}}, nil)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	st := defaultSettings()
	s := NewAPIServer(d, nil, nil, dir, nil)
	s.cli = &CLI{daemon: d, dataDir: dir, walletFile: st.WalletFile, settings: st, runningSettings: st}
	return s, d, dir
}

func patchSettings(t *testing.T, s *APIServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handlePatchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body)))
	return rr
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestPatchSettingsAppliesLiveFieldsAndPersists(t *testing.T) {
	s, d, dir := newSettingsTestServer(t)
	explorerAddr := freeTCPAddr(t)
	pid, err := peer.Decode(testWhitelistPeer)
	if err != nil {
		t.Fatal(err)
	}
	d.Node().BanPeer(pid, "test")

	rr := patchSettings(t, s, `{"save_checkpoints":true,"explorer_addr":"`+explorerAddr+`","wallet_file":"/tmp/x/main.wallet.dat","p2p_whitelist":["`+testWhitelistPeer+`"],"listen":"/ip4/127.0.0.1/tcp/1234"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	var resp settingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.RestartRequired, []string{"listen"}) {
		t.Fatalf("restart_required = %v, want [listen]", resp.RestartRequired)
	}
	if !d.saveCheckpoints.Load() {
		t.Fatal("save_checkpoints not applied")
	}
	if got := s.cli.walletFilePath(); got != "/tmp/x/main.wallet.dat" {
		t.Fatalf("wallet file = %q", got)
	}
	if d.Node().IsBanned(pid) {
		t.Fatal("whitelisted peer still banned")
	}
	if _, err := http.Get("http://" + explorerAddr + "/"); err != nil {
		t.Fatalf("explorer not serving: %v", err)
	}

	saved, exists, err := loadSettings(dir)
	if err != nil || !exists {
		t.Fatalf("load saved settings: exists=%v err=%v", exists, err)
	}
	if saved.ExplorerAddr != explorerAddr || !saved.SaveCheckpoints || saved.Listen != "/ip4/127.0.0.1/tcp/1234" || !slices.Equal(saved.P2PWhitelist, []string{testWhitelistPeer}) {
		t.Fatalf("saved settings mismatch: %+v", saved)
	}

	if rr := patchSettings(t, s, `{"explorer_addr":""}`); rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	client := http.Client{Timeout: time.Second}
	if _, err := client.Get("http://" + explorerAddr + "/"); err == nil {
		t.Fatal("explorer still serving after explorer_addr was cleared")
	}
}

func TestPatchSettingsRejectsBadInputWithoutSaving(t *testing.T) {
	s, d, dir := newSettingsTestServer(t)
	for _, body := range []string{
		`{"no_such_setting":true}`,
		`{"listen":"not a multiaddr","save_checkpoints":true}`,
		`{"p2p_whitelist":["not-a-peer"],"save_checkpoints":true}`,
		`{"p2p_max_inbound":-1,"save_checkpoints":true}`,
	} {
		if rr := patchSettings(t, s, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", body, rr.Code)
		}
	}
	if _, err := os.Stat(settingsPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("settings file written after rejected patches: %v", err)
	}
	if d.saveCheckpoints.Load() {
		t.Fatal("rejected patch changed save_checkpoints")
	}
}

func TestLegacyFlagsMigrateIntoSettings(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	l := registerLegacySettingsFlags(fs)
	registerRemovedCLIFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if legacySettingsFlagsUsed(fs) {
		t.Fatal("no flags passed but legacy use reported")
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	l = registerLegacySettingsFlags(fs)
	if err := fs.Parse([]string{"--explorer", "127.0.0.1:9999", "--save-checkpoints", "--p2p-max-inbound", "7", "--p2p-whitelist-peer", testWhitelistPeer, "--no-version-check"}); err != nil {
		t.Fatal(err)
	}
	if !legacySettingsFlagsUsed(fs) {
		t.Fatal("legacy flags not detected")
	}
	base := defaultSettings()
	base.ChainCacheCap = 999
	base.Seed = true
	got, err := l.apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExplorerAddr != "127.0.0.1:9999" || !got.SaveCheckpoints || got.P2PMaxInbound != 7 || !slices.Equal(got.P2PWhitelist, []string{testWhitelistPeer}) {
		t.Fatalf("flags not applied: %+v", got)
	}
	if got.ChainCacheCap != 999 {
		t.Fatalf("API-only setting lost: %+v", got)
	}
	if got.Seed {
		t.Fatal("an old mantle that omits --seed must turn seed mode off")
	}
}

func TestUnexpectedArgumentsExit(t *testing.T) {
	res := runMainHelper(t, helperRun{args: []string{"--data", t.TempDir(), "--version", "/ip4/1.2.3.4/tcp/28080"}})
	if res.exitCode != 1 || !strings.Contains(res.stderr, "unexpected arguments") {
		t.Fatalf("exit %d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}
}

func TestPurgeKeepsSettings(t *testing.T) {
	s, _, dir := newSettingsTestServer(t)
	if rr := patchSettings(t, s, `{"save_checkpoints":true}`); rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	want, _ := os.ReadFile(settingsPath(dir))
	rr := httptest.NewRecorder()
	s.handlePurgeData(rr, httptest.NewRequest(http.MethodPost, "/api/purge", bytes.NewReader([]byte(`{"confirm":true}`))))
	if rr.Code != http.StatusOK {
		t.Fatalf("purge: %d %s", rr.Code, rr.Body.String())
	}
	got, err := os.ReadFile(settingsPath(dir))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("settings not kept after purge: err=%v", err)
	}
}
