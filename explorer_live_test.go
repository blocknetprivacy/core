package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The explorer serves its own event stream so pages update in place. A block
// arriving on the daemon's subscription must reach a connected client.
func TestExplorerEventsStreamsNewBlocks(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	// Go through the real handler chain and a server that actually sets a write
	// deadline. A short one stands in for the production WriteTimeout: if the
	// handler did not clear it, the stream would die before the block arrives.
	explorer := NewExplorer(daemon)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	server := explorer.httpServer("")
	server.WriteTimeout = 300 * time.Millisecond
	go server.Serve(listener)
	defer server.Close()

	baseURL := "http://" + listener.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/events", nil)
	if err != nil {
		t.Fatalf("failed to build events request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to open event stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events returned %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("GET /events Content-Type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readEvent := func(want string) string {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("failed reading %s event: %v", want, err)
			}
			if strings.TrimSpace(line) != "event: "+want {
				continue
			}
			data, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("failed reading %s payload: %v", want, err)
			}
			return strings.TrimSpace(data)
		}
	}

	readEvent("connected")

	// Past the write deadline the handler had to clear.
	time.Sleep(600 * time.Millisecond)

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
	daemon.notifyBlock(block)

	payload := readEvent("new_block")
	if !strings.Contains(payload, `"height":1`) {
		t.Fatalf("new_block payload %q missing height 1", payload)
	}
	// The pages render the next target, so the event must carry that and not
	// the mined block's own difficulty, or the tile gets a different number.
	if !strings.Contains(payload, `"next_difficulty":`) {
		t.Fatalf("new_block payload %q is missing next_difficulty", payload)
	}
	if strings.Contains(payload, `"difficulty":`) {
		t.Fatalf("new_block payload %q still carries the block's own difficulty", payload)
	}
}

// The live script and its DOM hooks must actually reach the browser, or the
// stream above updates nothing.
func TestExplorerPagesShipLiveUpdateHooks(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	explorer := NewExplorer(daemon)
	rr := mustMakeHTTPJSONRequest(t, explorer, http.MethodGet, "/", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", rr.Code)
	}

	body := rr.Body.String()
	for _, hook := range []string{"new EventSource('/events')", `id="stat-height"`, `id="stat-difficulty"`, `class="recent-blocks"`} {
		if !strings.Contains(body, hook) {
			t.Fatalf("index page is missing live-update hook %q", hook)
		}
	}

	// The stats page shows the same height and difficulty; it must update too.
	stats := mustMakeHTTPJSONRequest(t, explorer, http.MethodGet, "/stats", nil, nil)
	if stats.Code != http.StatusOK {
		t.Fatalf("GET /stats returned %d, want 200", stats.Code)
	}
	statsBody := stats.Body.String()
	for _, hook := range []string{"new EventSource('/events')", `id="stat-height"`, `id="stat-difficulty"`} {
		if !strings.Contains(statsBody, hook) {
			t.Fatalf("stats page is missing live-update hook %q", hook)
		}
	}
}
