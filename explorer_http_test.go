package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustBuildChainToHeight(t *testing.T, chain *Chain, storage *Storage, top uint64) {
	t.Helper()

	prev := chain.GetBlockByHeight(0)
	if prev == nil {
		t.Fatal("expected genesis block")
	}
	for height := uint64(1); height <= top; height++ {
		pub, err := GenerateRistrettoKeypair()
		if err != nil {
			t.Fatalf("failed to generate key at height %d: %v", height, err)
		}
		block := makeOutputOnlyTestBlock(height, prev.Hash(), prev.Header.Timestamp+BlockIntervalSec, []TxOutput{
			{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
		})
		commitMainChainBlockForTest(t, chain, storage, block, height+1)
		prev = block
	}
}

// Only pages that can no longer change may be cached. A block past hard
// finality is safe; a recent one, the index, and the stats page are not.
func TestExplorerCachesOnlyFinalPages(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	const top = uint64(MaxReorgDepth) + 5
	mustBuildChainToHeight(t, chain, storage, top)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()
	explorer := NewExplorer(daemon)

	cacheHeader := func(path string) string {
		t.Helper()
		rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodGet, path, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d, want 200", path, rr.Code)
		}
		return rr.Header().Get("Cache-Control")
	}

	if got := cacheHeader("/block/1"); got != explorerImmutableCache {
		t.Fatalf("final block page Cache-Control = %q, want %q", got, explorerImmutableCache)
	}
	if got := cacheHeader(fmt.Sprintf("/block/%d", top)); got != "no-store" {
		t.Fatalf("tip block page Cache-Control = %q, want no-store", got)
	}
	if got := cacheHeader("/"); got != "no-store" {
		t.Fatalf("index Cache-Control = %q, want no-store", got)
	}
	if got := cacheHeader("/stats"); got != "no-store" {
		t.Fatalf("stats Cache-Control = %q, want no-store", got)
	}

	finalBlock := chain.GetBlockByHeight(1)
	if finalBlock == nil {
		t.Fatal("expected block at height 1")
	}
	txID, err := finalBlock.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash tx: %v", err)
	}
	if got := cacheHeader("/tx/" + fmt.Sprintf("%x", txID)); got != explorerImmutableCache {
		t.Fatalf("final tx page Cache-Control = %q, want %q", got, explorerImmutableCache)
	}
}

// HTML is compressed; the event stream must not be, or it would buffer and the
// handler would lose the flusher it needs.
func TestExplorerGzipsPagesButNotEventStream(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	handler := gzipMiddleware(NewExplorer(daemon))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("index Content-Encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("index Vary = %q, want it to include Accept-Encoding", got)
	}

	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("index body is not valid gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("failed to decompress index: %v", err)
	}
	if !strings.Contains(string(body), "blocknet explorer") {
		t.Fatal("decompressed index is missing expected content")
	}

	// The stream handler blocks, so exercise the middleware's routing decision
	// against a stub rather than the real one.
	stub := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "stream"); err != nil {
			t.Errorf("stub write failed: %v", err)
		}
	}))

	eventsReq := httptest.NewRequest(http.MethodGet, "/events", nil)
	eventsReq.Header.Set("Accept-Encoding", "gzip")
	eventsRR := httptest.NewRecorder()
	stub.ServeHTTP(eventsRR, eventsReq)

	if got := eventsRR.Header().Get("Content-Encoding"); got == "gzip" {
		t.Fatal("event stream must not be gzip encoded")
	}
	if got := eventsRR.Body.String(); got != "stream" {
		t.Fatalf("event stream body = %q, want it passed through uncompressed", got)
	}
}
