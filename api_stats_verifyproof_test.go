package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"blocknet/wallet"
)

// --- Emission / supply math (pure, no chain needed) ---

func TestEmissionSupplyMath(t *testing.T) {
	// Genesis (height 0) carries no coinbase, so nothing is emitted through it.
	if got := TotalEmittedSupply(0); got != 0 {
		t.Fatalf("TotalEmittedSupply(0) = %d, want 0", got)
	}
	if got, want := TotalEmittedSupply(1), GetBlockReward(1); got != want {
		t.Fatalf("TotalEmittedSupply(1) = %d, want %d", got, want)
	}

	// TotalEmittedSupply must equal an independent incremental sum at every
	// checkpoint, including into the tail-emission region.
	tailStart := uint64(MonthsToTail) * BlocksPerMonth
	checkpoints := map[uint64]bool{1: true, 100: true, 1000: true, BlocksPerMonth: true, tailStart + 5: true}
	var sum uint64
	for h := uint64(1); h <= tailStart+5; h++ {
		sum += GetBlockReward(h)
		if checkpoints[h] {
			if got := TotalEmittedSupply(h); got != sum {
				t.Fatalf("TotalEmittedSupply(%d) = %d, want incremental %d", h, got, sum)
			}
		}
	}

	// Tail emission is a flat TailEmission per block once reached.
	if got := GetBlockReward(tailStart); got != TailEmission {
		t.Fatalf("GetBlockReward(tail) = %d, want TailEmission %d", got, TailEmission)
	}

	// SupplyBreakdown invariants below the target supply.
	emitted, remaining, pct := SupplyBreakdown(1000)
	if emitted+remaining != uint64(TargetSupply) {
		t.Fatalf("emitted+remaining = %d, want TargetSupply %d", emitted+remaining, uint64(TargetSupply))
	}
	if want := float64(emitted) / float64(uint64(TargetSupply)) * 100; pct != want {
		t.Fatalf("pct_emitted = %v, want %v", pct, want)
	}
	if emitted != TotalEmittedSupply(1000) {
		t.Fatalf("SupplyBreakdown emitted disagrees with TotalEmittedSupply")
	}
}

// --- GET /api/stats (functional, genesis-only chain) ---

func statsUint(t *testing.T, m map[string]any, key string) uint64 {
	t.Helper()
	n, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("stats field %q is %T, want number", key, m[key])
	}
	u, err := strconv.ParseUint(n.String(), 10, 64)
	if err != nil {
		t.Fatalf("stats field %q = %q, not uint64: %v", key, n.String(), err)
	}
	return u
}

func statsFloat(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	n, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("stats field %q is %T, want number", key, m[key])
	}
	f, err := n.Float64()
	if err != nil {
		t.Fatalf("stats field %q = %q, not float: %v", key, n.String(), err)
	}
	return f
}

func TestHandleStats(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, dcleanup := mustStartTestDaemon(t, chain)
	defer dcleanup()

	s := NewAPIServer(daemon, nil, nil, t.TempDir(), nil)

	rr := mustMakeHTTPJSONRequest(t, http.HandlerFunc(s.handleStats), "GET", "/api/stats", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	dec := json.NewDecoder(bytes.NewReader(rr.Body.Bytes()))
	dec.UseNumber()
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	for _, k := range []string{
		"height", "difficulty", "network_hashrate", "avg_block_time", "block_reward",
		"emitted", "remaining", "target_supply", "pct_emitted", "tail_emission",
		"months_to_tail", "blocks_per_month", "block_interval_sec",
	} {
		if _, ok := resp[k]; !ok {
			t.Fatalf("stats response missing %q; got %v", k, resp)
		}
	}

	// Genesis-only chain: nothing emitted, full remaining, no block-interval history.
	if got := statsUint(t, resp, "height"); got != 0 {
		t.Fatalf("height = %d, want 0", got)
	}
	if got := statsUint(t, resp, "emitted"); got != 0 {
		t.Fatalf("emitted = %d, want 0", got)
	}
	if got := statsUint(t, resp, "remaining"); got != uint64(TargetSupply) {
		t.Fatalf("remaining = %d, want %d", got, uint64(TargetSupply))
	}
	if got := statsUint(t, resp, "target_supply"); got != uint64(TargetSupply) {
		t.Fatalf("target_supply = %d, want %d", got, uint64(TargetSupply))
	}
	if got := statsUint(t, resp, "block_reward"); got != GetBlockReward(0) {
		t.Fatalf("block_reward = %d, want %d", got, GetBlockReward(0))
	}
	if got := statsUint(t, resp, "tail_emission"); got != uint64(TailEmission) {
		t.Fatalf("tail_emission = %d, want %d", got, uint64(TailEmission))
	}
	if got := statsUint(t, resp, "block_interval_sec"); got != uint64(BlockIntervalSec) {
		t.Fatalf("block_interval_sec = %d, want %d", got, uint64(BlockIntervalSec))
	}
	if got := statsFloat(t, resp, "network_hashrate"); got != 0 {
		t.Fatalf("network_hashrate = %v, want 0 (no history)", got)
	}
	if got := statsFloat(t, resp, "avg_block_time"); got != 0 {
		t.Fatalf("avg_block_time = %v, want 0 (no history)", got)
	}

	// A second call while the tip is unchanged must serve the cached response.
	rr2 := mustMakeHTTPJSONRequest(t, http.HandlerFunc(s.handleStats), "GET", "/api/stats", nil, nil)
	if rr2.Code != http.StatusOK || rr2.Body.String() != rr.Body.String() {
		t.Fatalf("cached stats differ: first=%s second=%s", rr.Body.String(), rr2.Body.String())
	}
	if !s.statsValid || s.statsTip != chain.BestHash() {
		t.Fatalf("stats cache not keyed on chain tip")
	}
}

// --- POST /api/verify-proof (functional, input validation) ---

func TestHandleVerifyProofValidation(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, dcleanup := mustStartTestDaemon(t, chain)
	defer dcleanup()

	s := NewAPIServer(daemon, nil, nil, t.TempDir(), nil)

	hex64 := strings.Repeat("a", 64)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"short_txid", map[string]any{"txid": "abc", "tx_key": hex64, "address": "x"}, http.StatusBadRequest},
		{"short_tx_key", map[string]any{"txid": hex64, "tx_key": "xyz", "address": "x"}, http.StatusBadRequest},
		{"missing_address", map[string]any{"txid": hex64, "tx_key": hex64, "address": ""}, http.StatusBadRequest},
		{"nonhex_tx_key", map[string]any{"txid": hex64, "tx_key": strings.Repeat("g", 64), "address": "x"}, http.StatusBadRequest},
		{"invalid_address", map[string]any{"txid": hex64, "tx_key": hex64, "address": "not-a-real-address"}, http.StatusBadRequest},
		{"nonhex_txid", map[string]any{"txid": strings.Repeat("z", 64), "tx_key": hex64, "address": "x"}, http.StatusBadRequest},
	}

	for i, tc := range cases {
		body, _ := json.Marshal(tc.body)
		req := httptest.NewRequest(http.MethodPost, "/api/verify-proof", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// Distinct source IP per case so the per-IP rate limiter never trips.
		req.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", i+1)
		rr := httptest.NewRecorder()
		s.handleVerifyProof(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d; body = %s", tc.name, rr.Code, tc.want, rr.Body.String())
		}
	}
}

// --- OpenAPI contract: spec documents both endpoints and their schemas ---

func TestOpenAPIStatsAndVerifyProofContract(t *testing.T) {
	specBytes, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatalf("failed to read api_openapi.json: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("failed to parse api_openapi.json: %v", err)
	}

	paths := mustGetMapAny(t, spec, "paths")
	mustGetMapAny(t, mustGetMapAny(t, paths, "/api/stats"), "get")
	mustGetMapAny(t, mustGetMapAny(t, paths, "/api/verify-proof"), "post")

	schemas := mustGetMapAny(t, mustGetMapAny(t, spec, "components"), "schemas")

	statsProps := mustGetMapAny(t, mustGetMapAny(t, schemas, "StatsResponse"), "properties")
	for _, f := range []string{
		"height", "difficulty", "network_hashrate", "avg_block_time", "block_reward",
		"emitted", "remaining", "target_supply", "pct_emitted", "tail_emission",
		"months_to_tail", "blocks_per_month", "block_interval_sec",
	} {
		if _, ok := statsProps[f]; !ok {
			t.Fatalf("StatsResponse missing %q in OpenAPI", f)
		}
	}

	reqProps := mustGetMapAny(t, mustGetMapAny(t, schemas, "VerifyProofRequest"), "properties")
	for _, f := range []string{"txid", "tx_key", "address"} {
		if _, ok := reqProps[f]; !ok {
			t.Fatalf("VerifyProofRequest missing %q in OpenAPI", f)
		}
	}

	respProps := mustGetMapAny(t, mustGetMapAny(t, schemas, "VerifyProofResponse"), "properties")
	for _, f := range []string{"txid", "valid", "total_matched", "outputs"} {
		if _, ok := respProps[f]; !ok {
			t.Fatalf("VerifyProofResponse missing %q in OpenAPI", f)
		}
	}

	outProps := mustGetMapAny(t, mustGetMapAny(t, schemas, "ProofOutput"), "properties")
	for _, f := range []string{"index", "match", "amount", "memo"} {
		if _, ok := outProps[f]; !ok {
			t.Fatalf("ProofOutput missing %q in OpenAPI", f)
		}
	}
}

// --- verify-proof crypto core (real stealth output, no chain needed) ---

func TestVerifyProofMatchOutputs(t *testing.T) {
	keys, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys: %v", err)
	}
	out, err := DeriveStealthAddress(keys.SpendPubKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthAddress: %v", err)
	}

	const amount = uint64(250_000_000)
	// Encrypt the output exactly the way the sender does, so the handler's
	// sender-side derivation must recover it.
	secret, err := DeriveStealthSecretSender(out.TxPrivKey, keys.ViewPubKey)
	if err != nil {
		t.Fatalf("DeriveStealthSecretSender: %v", err)
	}
	blinding := wallet.DeriveBlinding(secret, 0)
	encMemo, err := wallet.EncryptMemo([]byte("invoice 42"), secret, 0)
	if err != nil {
		t.Fatalf("EncryptMemo: %v", err)
	}

	tx := &Transaction{
		TxPublicKey: out.TxPubKey,
		Outputs: []TxOutput{{
			PublicKey:       out.OnetimePubKey,
			EncryptedAmount: EncryptAmount(amount, blinding, 0),
			EncryptedMemo:   encMemo,
		}},
	}

	// Correct recipient: the output matches and amount + memo are recovered.
	res, total := matchProofOutputs(tx, out.TxPrivKey, keys.SpendPubKey, keys.ViewPubKey, false, 0)
	if len(res) != 1 {
		t.Fatalf("got %d outputs, want 1", len(res))
	}
	if res[0]["match"] != true {
		t.Fatalf("expected match=true, got %v", res[0])
	}
	if got := res[0]["amount"].(uint64); got != amount {
		t.Fatalf("amount = %d, want %d", got, amount)
	}
	if got := res[0]["memo"].(string); got != "invoice 42" {
		t.Fatalf("memo = %q, want %q", got, "invoice 42")
	}
	if total != amount {
		t.Fatalf("total_matched = %d, want %d", total, amount)
	}

	// Wrong recipient: no match, nothing revealed.
	other, err := GenerateStealthKeys()
	if err != nil {
		t.Fatalf("GenerateStealthKeys(other): %v", err)
	}
	res2, total2 := matchProofOutputs(tx, out.TxPrivKey, other.SpendPubKey, other.ViewPubKey, false, 0)
	if res2[0]["match"] != false {
		t.Fatalf("expected match=false for wrong recipient, got %v", res2[0])
	}
	if _, hasAmount := res2[0]["amount"]; hasAmount {
		t.Fatalf("wrong recipient must not reveal an amount: %v", res2[0])
	}
	if total2 != 0 {
		t.Fatalf("total_matched = %d for wrong recipient, want 0", total2)
	}
}
