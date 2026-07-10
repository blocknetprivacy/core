package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	templateID, initialExpiry, err := s.rememberMiningTemplateLease(&Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}})
	if err != nil {
		t.Fatalf("remember template: %v", err)
	}
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
	templateID, initialExpiry, err := s.rememberMiningTemplateLease(&Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}})
	if err != nil {
		t.Fatalf("remember template: %v", err)
	}

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

func TestRememberMiningTemplateLease_RejectsSustainedSameTipChurnWithoutEviction(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 17, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = time.Hour

	tip := chain.TemplateParams()
	block := &Block{Header: BlockHeader{
		Version:  1,
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}}

	ids := make([]string, 0, maxMiningTemplateCacheEntries)
	var advertisedExpiry time.Time
	for i := 0; i < maxMiningTemplateCacheEntries; i++ {
		templateID, expiry, err := s.rememberMiningTemplateLease(block)
		if err != nil {
			t.Fatalf("fill cache at entry %d: %v", i, err)
		}
		ids = append(ids, templateID)
		advertisedExpiry = expiry
	}
	now = now.Add(30 * time.Minute)
	renewedExpiry, err := s.renewMiningTemplate(ids[0])
	if err != nil {
		t.Fatalf("renew template in full cache: %v", err)
	}
	if !renewedExpiry.After(advertisedExpiry) {
		t.Fatalf("renewal did not extend expiry: got %s initial %s", renewedExpiry, advertisedExpiry)
	}

	// Sustained same-tip churn must fail closed rather than silently evicting any
	// lease whose expiry has already been advertised to a miner.
	for i := 0; i < 2*maxMiningTemplateCacheEntries; i++ {
		now = now.Add(time.Millisecond)
		templateID, expiry, err := s.rememberMiningTemplateLease(block)
		if !errors.Is(err, errMiningTemplateCacheFull) {
			t.Fatalf("overflow attempt %d: got err %v, want cache-full", i, err)
		}
		if templateID != "" || !expiry.IsZero() {
			t.Fatalf("overflow attempt %d returned a lease: id=%q expiry=%s", i, templateID, expiry)
		}
	}
	if len(s.templateCache) != maxMiningTemplateCacheEntries {
		t.Fatalf("cache grew past bound: got %d want %d", len(s.templateCache), maxMiningTemplateCacheEntries)
	}
	for i, templateID := range ids {
		if _, ok := s.getMiningTemplate(templateID); !ok {
			t.Fatalf("advertised lease %d was evicted by same-tip churn", i)
		}
	}

	// Once the inactive leases reach their advertised boundary, insertion prunes
	// them and capacity becomes available. The actively renewed lease remains.
	now = advertisedExpiry
	newID, _, err := s.rememberMiningTemplateLease(block)
	if err != nil {
		t.Fatalf("remember template after expiry: %v", err)
	}
	if len(s.templateCache) != 2 {
		t.Fatalf("expired cache was not reclaimed: got %d entries", len(s.templateCache))
	}
	if _, ok := s.getMiningTemplate(ids[0]); !ok {
		t.Fatal("renewed lease was pruned at the inactive leases' expiry")
	}
	if _, ok := s.getMiningTemplate(ids[1]); ok {
		t.Fatal("inactive lease remained after its advertised expiry")
	}
	if _, ok := s.getMiningTemplate(newID); !ok {
		t.Fatal("new template missing after expired leases were pruned")
	}
}

func TestRememberMiningTemplateLease_TipChangePrunesOldCapacity(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 18, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = time.Hour

	oldTip := chain.TemplateParams()
	oldBlock := &Block{Header: BlockHeader{Version: 1, Height: oldTip.Height, PrevHash: oldTip.PrevHash}}
	oldIDs := make([]string, 0, maxMiningTemplateCacheEntries)
	for i := 0; i < maxMiningTemplateCacheEntries; i++ {
		templateID, _, err := s.rememberMiningTemplateLease(oldBlock)
		if err != nil {
			t.Fatalf("fill old-tip cache at entry %d: %v", i, err)
		}
		oldIDs = append(oldIDs, templateID)
	}

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	newTipBlock := makeOutputOnlyTestBlock(oldTip.Height, oldTip.PrevHash, genesis.Header.Timestamp+BlockIntervalSec, nil)
	commitMainChainBlockForTest(t, chain, storage, newTipBlock, chain.TotalWork()+MinDifficulty)
	chain.mu.Lock()
	chain.updateTipSnapshotLocked()
	chain.mu.Unlock()

	newTip := chain.TemplateParams()
	newBlock := &Block{Header: BlockHeader{Version: 1, Height: newTip.Height, PrevHash: newTip.PrevHash}}
	newID, _, err := s.rememberMiningTemplateLease(newBlock)
	if err != nil {
		t.Fatalf("remember first new-tip template: %v", err)
	}
	if len(s.templateCache) != 1 {
		t.Fatalf("tip change did not reclaim old capacity: got %d entries", len(s.templateCache))
	}
	for i, templateID := range oldIDs {
		if _, ok := s.getMiningTemplate(templateID); ok {
			t.Fatalf("old-tip lease %d survived new-tip insertion", i)
		}
	}
	if _, ok := s.getMiningTemplate(newID); !ok {
		t.Fatal("new-tip template missing after old-tip pruning")
	}

	for i := 1; i < maxMiningTemplateCacheEntries; i++ {
		if _, _, err := s.rememberMiningTemplateLease(newBlock); err != nil {
			t.Fatalf("fill new-tip cache at entry %d: %v", i, err)
		}
	}
	if _, _, err := s.rememberMiningTemplateLease(newBlock); !errors.Is(err, errMiningTemplateCacheFull) {
		t.Fatalf("new-tip cache did not enforce bound: got %v", err)
	}
}

func TestRememberMiningTemplateLease_RejectsOldInflightTemplateWithoutPurgingNewTip(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	s := NewAPIServer(&Daemon{chain: chain}, nil, nil, t.TempDir(), nil)
	s.templateNow = func() time.Time { return now }
	s.templateTTL = time.Hour

	// Simulate request A taking its build snapshot and then stalling before it
	// reaches cache admission.
	tipA := chain.TemplateParams()
	templateA := &Block{Header: BlockHeader{Version: 1, Height: tipA.Height, PrevHash: tipA.PrevHash}}

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	tipBBlock := makeOutputOnlyTestBlock(tipA.Height, tipA.PrevHash, genesis.Header.Timestamp+BlockIntervalSec, nil)
	commitMainChainBlockForTest(t, chain, storage, tipBBlock, chain.TotalWork()+MinDifficulty)
	chain.mu.Lock()
	chain.updateTipSnapshotLocked()
	chain.mu.Unlock()

	// Request B completes first and advertises leases for the new canonical tip.
	tipB := chain.TemplateParams()
	templateB := &Block{Header: BlockHeader{Version: 1, Height: tipB.Height, PrevHash: tipB.PrevHash}}
	bIDs := make([]string, 4)
	for i := range bIDs {
		templateID, _, err := s.rememberMiningTemplateLease(templateB)
		if err != nil {
			t.Fatalf("remember tip-B template %d: %v", i, err)
		}
		bIDs[i] = templateID
	}

	// Request A now completes out of order. Cache admission must compare it to
	// canonical tip B before pruning, reject it, and leave every B lease intact.
	templateID, expiry, err := s.rememberMiningTemplateLease(templateA)
	if !errors.Is(err, errMiningTemplateStale) {
		t.Fatalf("old in-flight template: got err %v, want stale", err)
	}
	if templateID != "" || !expiry.IsZero() {
		t.Fatalf("stale in-flight request returned a lease: id=%q expiry=%s", templateID, expiry)
	}
	if len(s.templateCache) != len(bIDs) {
		t.Fatalf("stale in-flight request changed cache size: got %d want %d", len(s.templateCache), len(bIDs))
	}
	for i, id := range bIDs {
		cached, ok := s.getMiningTemplate(id)
		if !ok {
			t.Fatalf("tip-B lease %d was purged by stale request A", i)
		}
		if cached.Header.Height != tipB.Height || cached.Header.PrevHash != tipB.PrevHash {
			t.Fatalf("tip-B lease %d changed to stale tip A", i)
		}
	}
}

func TestMiningTemplateLease_AdvertisedUnixMillisIsPruningBoundary(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	// time.Now carries Go's process-local monotonic reading on supported
	// platforms. Also force a sub-millisecond wall-clock component so this test
	// covers both forms of precision that cannot be represented on the wire.
	rawNow := time.Now()
	subMillisecond := time.Duration(rawNow.Nanosecond() % int(time.Millisecond))
	rawNow = rawNow.Add(137*time.Microsecond - subMillisecond)

	s := &APIServer{
		daemon:        &Daemon{chain: chain},
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

	tip := chain.TemplateParams()
	templateID, expiry, err := s.rememberMiningTemplateLease(&Block{Header: BlockHeader{
		Height:   tip.Height,
		PrevHash: tip.PrevHash,
	}})
	if err != nil {
		t.Fatalf("remember template: %v", err)
	}
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
