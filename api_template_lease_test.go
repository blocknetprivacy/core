package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type renewTemplateTestResponse struct {
	TemplateID                  string `json:"template_id"`
	TemplateExpiresAtUnixMillis int64  `json:"template_expires_at_unix_ms"`
	Error                       string `json:"error"`
}

func requestTemplateRenewal(t *testing.T, handler http.Handler, token, templateID string) (*httptest.ResponseRecorder, renewTemplateTestResponse) {
	t.Helper()

	body, err := json.Marshal(map[string]string{"template_id": templateID})
	if err != nil {
		t.Fatalf("marshal renewal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mining/renewtemplate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var resp renewTemplateTestResponse
	if rr.Body.Len() > 0 && rr.Header().Get("Content-Type") == "application/json" {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode renewal response: %v (body=%q)", err, rr.Body.String())
		}
	}
	return rr, resp
}

func authenticatedMiningHandler(s *APIServer, token string) http.Handler {
	mux := http.NewServeMux()
	s.registerPrivateRoutes(mux)
	return authMiddleware(token, mux)
}

func TestHandleRenewBlockTemplate_ExtendsLeaseAcrossLongMiningRound(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 15, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = time.Minute

	tip := chain.TemplateParams()
	templateID, initialExpiry := s.rememberMiningTemplateLease(&Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}})
	if want := now.Add(time.Minute); !initialExpiry.Equal(want) {
		t.Fatalf("initial expiry mismatch: got %s want %s", initialExpiry, want)
	}

	const token = "template-lease-token"
	handler := authenticatedMiningHandler(s, token)

	unauthorized, _ := requestTemplateRenewal(t, handler, "", templateID)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("renewal route must require bearer auth: got %d body=%q", unauthorized.Code, unauthorized.Body.String())
	}

	// Renew shortly before each expiry. The total simulated mining round exceeds
	// the original lease several times without changing the template itself.
	for i := 0; i < 4; i++ {
		now = now.Add(50 * time.Second)
		rr, resp := requestTemplateRenewal(t, handler, token, templateID)
		if rr.Code != http.StatusOK {
			t.Fatalf("renewal %d: got %d body=%q", i+1, rr.Code, rr.Body.String())
		}
		if resp.TemplateID != templateID {
			t.Fatalf("renewal %d template id mismatch: got %q want %q", i+1, resp.TemplateID, templateID)
		}
		if want := now.Add(time.Minute).UnixMilli(); resp.TemplateExpiresAtUnixMillis != want {
			t.Fatalf("renewal %d expiry mismatch: got %d want %d", i+1, resp.TemplateExpiresAtUnixMillis, want)
		}
		if _, ok := s.getMiningTemplate(templateID); !ok {
			t.Fatalf("renewal %d did not keep compact-submit template available", i+1)
		}
	}

	// At the exact renewed expiry instant the lease is no longer renewable or
	// available to the existing compact submit path.
	now = now.Add(time.Minute)
	rr, resp := requestTemplateRenewal(t, handler, token, templateID)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expired renewal: got %d body=%q", rr.Code, rr.Body.String())
	}
	if resp.Error != "unknown or expired template_id" {
		t.Fatalf("expired renewal error mismatch: got %q", resp.Error)
	}
	if _, ok := s.getMiningTemplate(templateID); ok {
		t.Fatal("expired template remained available for compact submission")
	}
}

func TestHandleRenewBlockTemplate_RefusesTemplateAfterCanonicalTipChanges(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 16, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = time.Minute

	tip := chain.TemplateParams()
	templateID, initialExpiry := s.rememberMiningTemplateLease(&Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}})

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	newTip := makeOutputOnlyTestBlock(tip.Height, tip.PrevHash, genesis.Header.Timestamp+BlockIntervalSec, nil)
	commitMainChainBlockForTest(t, chain, storage, newTip, chain.TotalWork()+MinDifficulty)
	chain.mu.Lock()
	chain.updateTipSnapshotLocked()
	chain.mu.Unlock()

	now = now.Add(10 * time.Second)
	handler := authenticatedMiningHandler(s, "template-lease-token")
	rr, resp := requestTemplateRenewal(t, handler, "template-lease-token", templateID)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale-tip renewal: got %d body=%q", rr.Code, rr.Body.String())
	}
	if resp.Error != "template no longer builds on current tip" {
		t.Fatalf("stale-tip renewal error mismatch: got %q", resp.Error)
	}

	// Refusing renewal must not change existing submit semantics: the cached
	// template remains addressable until its original lease expires, so the
	// existing submitblock path can apply its normal stale-block rejection.
	s.templateMu.Lock()
	cached := s.templateCache[templateID]
	s.templateMu.Unlock()
	if !cached.expiresAt.Equal(initialExpiry) {
		t.Fatalf("stale-tip refusal extended lease: got %s want %s", cached.expiresAt, initialExpiry)
	}
	if _, ok := s.getMiningTemplate(templateID); !ok {
		t.Fatal("stale-tip refusal removed template from existing compact-submit cache")
	}
}

func TestHandleRenewBlockTemplate_ValidatesTemplateID(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	handler := authenticatedMiningHandler(s, "template-lease-token")

	rr, resp := requestTemplateRenewal(t, handler, "template-lease-token", "   ")
	if rr.Code != http.StatusBadRequest || resp.Error != "template_id is required" {
		t.Fatalf("blank template id: got %d body=%q", rr.Code, rr.Body.String())
	}

	rr, resp = requestTemplateRenewal(t, handler, "template-lease-token", "missing-template")
	if rr.Code != http.StatusNotFound || resp.Error != "unknown or expired template_id" {
		t.Fatalf("unknown template id: got %d body=%q", rr.Code, rr.Body.String())
	}
}

func TestRenewMiningTemplate_RefreshesEvictionRecency(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 17, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = 24 * time.Hour

	tip := chain.TemplateParams()
	block := &Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}}

	renewedID, _ := s.rememberMiningTemplateLease(block)
	now = now.Add(time.Second)
	inactiveID, _ := s.rememberMiningTemplateLease(block)
	for i := 2; i < maxMiningTemplateCacheEntries; i++ {
		now = now.Add(time.Second)
		s.rememberMiningTemplate(block)
	}

	now = now.Add(time.Second)
	renewedAt := now
	renewedExpiry, err := s.renewMiningTemplate(renewedID)
	if err != nil {
		t.Fatalf("renew active template: %v", err)
	}

	// Adding one more template exceeds the cache cap. The inactive template is
	// now the least recently touched entry; the renewed template must remain
	// cached for the lease deadline that was just advertised.
	now = now.Add(time.Second)
	newID := s.rememberMiningTemplate(block)
	if len(s.templateCache) != maxMiningTemplateCacheEntries {
		t.Fatalf("cache size mismatch: got %d want %d", len(s.templateCache), maxMiningTemplateCacheEntries)
	}
	if _, ok := s.getMiningTemplate(renewedID); !ok {
		t.Fatal("actively renewed template was evicted before advertised expiry")
	}
	if _, ok := s.getMiningTemplate(inactiveID); ok {
		t.Fatal("least-recently-touched inactive template was not evicted")
	}
	if _, ok := s.getMiningTemplate(newID); !ok {
		t.Fatal("new template was not retained after cache-pressure eviction")
	}
	if !now.Before(renewedExpiry) {
		t.Fatalf("test advanced past renewed expiry: now=%s expiry=%s", now, renewedExpiry)
	}

	s.templateMu.Lock()
	renewed := s.templateCache[renewedID]
	s.templateMu.Unlock()
	if !renewed.lastTouchedAt.Equal(renewedAt) {
		t.Fatalf("renewal recency mismatch: got %s want %s", renewed.lastTouchedAt, renewedAt)
	}
}

func TestMiningTemplateLease_AdvertisedUnixMillisIsPruningBoundary(t *testing.T) {
	// time.Now carries Go's process-local monotonic reading on supported
	// platforms. Also force a sub-millisecond wall-clock component so this test
	// covers both forms of precision that cannot be represented on the wire.
	rawNow := time.Now()
	subMillisecond := time.Duration(rawNow.Nanosecond() % int(time.Millisecond))
	rawNow = rawNow.Add(137*time.Microsecond - subMillisecond)

	s := &APIServer{
		templateCache: make(map[string]cachedMiningTemplate),
		templateNow:   func() time.Time { return rawNow },
		templateTTL:   1500 * time.Millisecond,
	}
	normalized := s.miningTemplateNow()
	if normalized != normalized.Round(0) {
		t.Fatal("mining-template clock retained a monotonic component")
	}
	if got := normalized.UnixNano() % int64(time.Millisecond); got != 0 {
		t.Fatalf("mining-template clock retained sub-millisecond precision: %d ns", got)
	}

	templateID, expiry := s.rememberMiningTemplateLease(&Block{})
	advertisedExpiryMillis := expiry.UnixMilli()
	if expiry.UnixNano() != advertisedExpiryMillis*int64(time.Millisecond) {
		t.Fatalf("stored expiry differs from advertised millisecond: stored=%s advertised=%d", expiry, advertisedExpiryMillis)
	}

	rawNow = time.UnixMilli(advertisedExpiryMillis - 1)
	if _, ok := s.getMiningTemplate(templateID); !ok {
		t.Fatal("template pruned before advertised expiry")
	}
	rawNow = time.UnixMilli(advertisedExpiryMillis)
	if _, ok := s.getMiningTemplate(templateID); ok {
		t.Fatal("template remained cached at advertised expiry")
	}
}
