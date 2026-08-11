package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// FindTxByHashStr returns early once the tx index is ready, so a mempool
// transaction is never found there. The page must fall through to the mempool,
// and must never be cached — it is not final.
func TestTxPageServesMempoolTransactionUncached(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	if !storage.TxIndexReady() {
		t.Fatal("expected the tx index to be ready on a fresh chain")
	}

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	tx := mustBuildValidRingCTBindingTestTx(t)
	txID := mustSeedMempoolTx(t, daemon.mempool, tx)
	hashStr := fmt.Sprintf("%x", txID)

	if _, _, found := chain.FindTxByHashStr(hashStr); found {
		t.Fatal("a mempool transaction must not resolve through the chain index")
	}

	rr := mustMakeHTTPJSONRequest(t, NewExplorer(daemon), http.MethodGet, "/tx/"+hashStr, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /tx/%s returned %d, want 200", hashStr[:16], rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "Pending") {
		t.Fatal("mempool transaction page does not show it as pending")
	}
	if !strings.Contains(body, hashStr) {
		t.Fatal("mempool transaction page is missing the transaction id")
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("mempool transaction page Cache-Control = %q, want no-store", got)
	}
}
