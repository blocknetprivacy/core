package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"testing"
)

func TestHandleMiningThreads_RejectsAboveNumCPU(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	mempool := NewMempool(DefaultMempoolConfig(), chain.IsKeyImageSpent, chain.IsCanonicalRingMember)
	miner := NewMiner(chain, mempool, MinerConfig{Threads: 1})
	d := &Daemon{miner: miner}

	s := NewAPIServer(d, nil, nil, t.TempDir(), nil)

	maxThreads := runtime.NumCPU()
	if maxThreads < 1 {
		maxThreads = 1
	}

	// Over max: should be rejected and not mutate miner thread count.
	req := httptest.NewRequest(http.MethodPost, "/api/mining/threads", bytes.NewReader([]byte(`{"threads":999999}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handleMiningThreads(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if got := miner.Threads(); got != 1 {
		t.Fatalf("expected miner threads unchanged after reject: got %d want %d", got, 1)
	}

	// At max: should succeed.
	req2 := httptest.NewRequest(http.MethodPost, "/api/mining/threads", bytes.NewReader([]byte(`{"threads":`+strconv.Itoa(maxThreads)+`}`)))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	s.handleMiningThreads(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%q)", rr2.Code, rr2.Body.String())
	}
	if got := miner.Threads(); got != maxThreads {
		t.Fatalf("expected miner threads set to max: got %d want %d", got, maxThreads)
	}
}
