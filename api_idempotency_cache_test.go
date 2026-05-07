package main

import (
	"testing"
	"time"
)

func TestIdempotencyCapNeverEvictsInFlight(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newIdempotencyCache(10*time.Minute, 1, "")

	var h1, h2 [32]byte
	h1[0] = 1
	h2[0] = 2

	state, _ := c.getOrStart(now, "k1", h1)
	if state != "start" {
		t.Fatalf("expected start for k1, got %q", state)
	}
	state, _ = c.getOrStart(now.Add(1*time.Second), "k2", h2)
	if state != "start" {
		t.Fatalf("expected start for k2, got %q", state)
	}

	c.mu.Lock()
	if len(c.entries) != 2 {
		c.mu.Unlock()
		t.Fatalf("expected 2 entries (over cap due to in-flight protection), got %d", len(c.entries))
	}
	if c.entries["k1"].state != idempotencyStateInProgress || c.entries["k2"].state != idempotencyStateInProgress {
		c.mu.Unlock()
		t.Fatalf("expected both entries to be in-flight")
	}
	c.mu.Unlock()

	// Completing one request makes it eligible for eviction; cap should drop back to 1.
	c.complete(now.Add(2*time.Second), "k1", h1, 200, []byte(`{"ok":true}`))

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 1 {
		t.Fatalf("expected 1 entry after completion + cap enforcement, got %d", len(c.entries))
	}
	if _, ok := c.entries["k2"]; !ok || c.entries["k2"].state != idempotencyStateInProgress {
		t.Fatalf("expected remaining entry to be k2 in-flight")
	}
}

func TestIdempotencyPruneSkipsInFlight(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newIdempotencyCache(1*time.Second, 100, "")

	var h [32]byte
	h[0] = 9

	_, _ = c.getOrStart(now, "k", h)

	// Advance beyond TTL; prune should not remove in-flight entry.
	c.mu.Lock()
	c.pruneLocked(now.Add(10 * time.Second))
	_, ok := c.entries["k"]
	c.mu.Unlock()

	if !ok {
		t.Fatal("expected in-flight entry to survive prune")
	}
}

func TestIdempotencyCachePersistsCompletedEntries(t *testing.T) {
	now := time.Now()
	path := t.TempDir() + "/send-idempotency.json"
	c := newIdempotencyCache(24*time.Hour, 100, path)

	var h [32]byte
	h[0] = 7

	state, _ := c.getOrStart(now, "k", h)
	if state != "start" {
		t.Fatalf("expected start, got %q", state)
	}

	body := []byte("{\"ok\":true}\n")
	c.complete(now.Add(time.Second), "k", h, 200, body)

	reloaded := newIdempotencyCache(24*time.Hour, 100, path)
	state, res := reloaded.getOrStart(now.Add(2*time.Second), "k", h)
	if state != "replay" {
		t.Fatalf("expected replay after reload, got %q", state)
	}
	if res.status != 200 || string(res.body) != string(body) {
		t.Fatalf("unexpected replay result: status=%d body=%q", res.status, string(res.body))
	}
}

func TestIdempotencyCachePersistsAcceptedEntriesAcrossRestart(t *testing.T) {
	now := time.Now()
	path := t.TempDir() + "/send-idempotency.json"
	c := newIdempotencyCache(24*time.Hour, 100, path)

	var h [32]byte
	h[0] = 8

	state, _ := c.getOrStart(now, "k", h)
	if state != "start" {
		t.Fatalf("expected start, got %q", state)
	}

	reloaded := newIdempotencyCache(24*time.Hour, 100, path)
	lookup, ok := reloaded.lookup(now.Add(time.Second), "k")
	if !ok {
		t.Fatal("expected accepted entry after reload")
	}
	if lookup.State != idempotencyStateAccepted {
		t.Fatalf("expected accepted state after reload, got %q", lookup.State)
	}

	state, _ = reloaded.getOrStart(now.Add(2*time.Second), "k", h)
	if state != "inflight" {
		t.Fatalf("expected inflight after reload, got %q", state)
	}
}
