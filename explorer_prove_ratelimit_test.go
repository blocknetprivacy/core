package main

import (
	"net/http"
	"testing"
)

// /prove and /prove-send do a chain read plus a per-output ECDH, and the
// explorer is unauthenticated. They must share the same per-IP budget the API
// already applies to POST /api/verify-proof.
func TestExplorerProveRoutesShareAPerIPRateLimit(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	explorer := NewExplorer(daemon)
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	body := []byte("tx=&spend_pub=&view_priv=")

	first := mustMakeHTTPJSONRequest(t, explorer, http.MethodPost, "/prove", body, headers)
	if first.Code == http.StatusTooManyRequests {
		t.Fatal("first /prove request was rate limited; burst should allow it")
	}

	limited := false
	for i := 0; i < 20; i++ {
		rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodPost, "/prove", body, headers)
		if rr.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("/prove never returned 429 under a burst; route is unlimited")
	}

	rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodPost, "/prove-send", body, headers)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("/prove-send returned %d after /prove exhausted the budget; want 429 from the shared limiter", rr.Code)
	}
}
