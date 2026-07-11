package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleSubmitBlock_CompactPayloadUsesTemplateCache(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	d := &Daemon{chain: chain}
	s := NewAPIServer(d, nil, nil, t.TempDir(), nil)

	tip := chain.TemplateParams()
	template := &Block{
		Header: BlockHeader{
			Version:  1,
			Height:   tip.Height,
			PrevHash: tip.PrevHash,
			Nonce:    0,
		},
	}
	templateID, _, err := s.rememberMiningTemplateLease(template)
	if err != nil {
		t.Fatalf("remember template: %v", err)
	}
	if templateID == "" {
		t.Fatal("expected non-empty template id")
	}

	// Advance the canonical tip after caching so the compact-submit path can
	// exercise a real stale template without bypassing cache admission rules.
	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}
	newTip := makeOutputOnlyTestBlock(tip.Height, tip.PrevHash, genesis.Header.Timestamp+BlockIntervalSec, nil)
	commitMainChainBlockForTest(t, chain, storage, newTip, chain.TotalWork()+MinDifficulty)
	chain.mu.Lock()
	chain.publishTipLocked()
	chain.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"template_id": templateID,
		"nonce":       uint64(7),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/mining/submitblock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleSubmitBlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%q", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp["error"]; got != "block rejected as stale" {
		t.Fatalf("unexpected response error %q body=%q", got, rr.Body.String())
	}
}

func TestHandleSubmitBlock_CompactPayloadUnknownTemplate(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	d := &Daemon{chain: chain}
	s := NewAPIServer(d, nil, nil, t.TempDir(), nil)

	body, err := json.Marshal(map[string]any{
		"template_id": "missing-template",
		"nonce":       uint64(7),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/mining/submitblock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleSubmitBlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%q", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp["error"]; got != "unknown or expired template_id" {
		t.Fatalf("unexpected response error %q body=%q", got, rr.Body.String())
	}
}
