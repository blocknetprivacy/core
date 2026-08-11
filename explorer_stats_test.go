package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// buildStatsSnapshot walks up to explorerStatsMaxTraversal blocks. No request
// may wait for it: the first hit after a restart serves cheap placeholders and
// the real numbers arrive from the background build.
func TestStatsAreNeverBuiltInsideARequest(t *testing.T) {
	chain, storage, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)
	mustBuildChainToHeight(t, chain, storage, 5)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	// Built directly, so no precompute has run and the snapshot is cold.
	explorer := &Explorer{daemon: daemon}

	cold := explorer.statsForRequest()
	if cold.Hashrate != "—" || cold.AvgBlockTime != "—" {
		t.Fatalf("cold request returned computed stats (hashrate %q, block time %q); it blocked on the traversal",
			cold.Hashrate, cold.AvgBlockTime)
	}
	if cold.Height != chain.Height() {
		t.Fatalf("cold snapshot height = %d, want %d", cold.Height, chain.Height())
	}
	if string(cold.DataJSON) != "[]" {
		t.Fatalf("cold snapshot chart data = %q, want an empty series", cold.DataJSON)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if snapshot, ready := explorer.getStatsSnapshot(); ready {
			if snapshot.Hashrate == "—" {
				t.Fatal("background build stored the placeholder snapshot")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background stats build never completed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if warmed := explorer.statsForRequest(); warmed.Hashrate == "—" {
		t.Fatal("request still served placeholders after the background build landed")
	}
}

// The stats page must render on a cold snapshot rather than erroring or hanging.
func TestStatsPageRendersWhileWarming(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	explorer := &Explorer{daemon: daemon, mux: http.NewServeMux()}
	explorer.mux.HandleFunc("/stats", explorer.handleStats)

	rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodGet, "/stats", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /stats returned %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "blocknet") {
		t.Fatal("warming stats page is missing expected content")
	}
}
