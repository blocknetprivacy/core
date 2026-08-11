package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Every explorer page must render from its pre-parsed template. Templates are
// now parsed once at init, so a broken one panics at startup instead of
// surfacing as a 500 here — either way this catches it.
func TestExplorerPagesRender(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	genesis := chain.GetBlockByHeight(0)
	if genesis == nil {
		t.Fatal("expected genesis block")
	}

	pub, err := GenerateRistrettoKeypair()
	if err != nil {
		t.Fatalf("failed to generate output key: %v", err)
	}
	block := makeOutputOnlyTestBlock(1, genesis.Hash(), genesis.Header.Timestamp+BlockIntervalSec, []TxOutput{
		{PublicKey: pub.PublicKey, Commitment: pub.PublicKey},
	})
	commitMainChainBlockForTest(t, chain, storage, block, 2)

	txID, err := block.Transactions[0].TxID()
	if err != nil {
		t.Fatalf("failed to hash block transaction: %v", err)
	}

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	explorer := NewExplorer(daemon)

	pages := []struct {
		path string
		want string
	}{
		{"/", "blocknet explorer"},
		{"/block/0", fmt.Sprintf("%x", genesis.Hash())},
		{"/tx/" + fmt.Sprintf("%x", txID), fmt.Sprintf("%x", txID)},
		{"/stats", "blocknet"},
	}

	for _, page := range pages {
		rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodGet, page.path, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d, want 200", page.path, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET %s returned Content-Type %q, want text/html", page.path, ct)
		}
		if !strings.Contains(rr.Body.String(), page.want) {
			t.Fatalf("GET %s body missing %q", page.path, page.want)
		}
	}
}

// The recent-blocks loop used to stop at height 1, so genesis never appeared.
// It is visible on any chain shorter than the list limit — a fresh testnet.
func TestIndexListsGenesisBlock(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	mustBuildChainToHeight(t, chain, storage, 2)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	rr := mustMakeHTTPJSONRequest(t, NewExplorer(daemon), http.MethodGet, "/", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", rr.Code)
	}

	body := rr.Body.String()
	for _, height := range []string{"0", "1", "2"} {
		if !strings.Contains(body, `href="/block/`+height+`"`) {
			t.Fatalf("index block list is missing height %s", height)
		}
	}
}
