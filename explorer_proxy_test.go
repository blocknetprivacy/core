package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blocknet/protocol/params"
)

func mustProxyingExplorer(t *testing.T, upstream *httptest.Server) *Explorer {
	t.Helper()

	chain, _, cleanup := mustCreateTestChain(t)
	t.Cleanup(cleanup)
	mustAddGenesisBlock(t, chain)

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	t.Cleanup(stopDaemon)

	explorer := NewExplorer(daemon)
	explorer.testnetAddr = strings.TrimPrefix(upstream.URL, "http://")
	return explorer
}

func testnetCookieRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: "net", Value: "testnet"})
	return req
}

// A stalled testnet daemon must not pin a mainnet goroutine. The old bare
// http.Get had no timeout at all.
func TestProxyToTestnetGivesUpOnAStalledUpstream(t *testing.T) {
	if params.IsTestnet {
		t.Skip("proxy only runs on mainnet")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	blocked := make(chan struct{})
	stalled := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-blocked
		})},
	}
	stalled.Start()
	defer func() {
		close(blocked)
		stalled.Close()
	}()

	explorer := mustProxyingExplorer(t, stalled)
	explorerProxyClient.Timeout = 250 * time.Millisecond
	defer func() { explorerProxyClient.Timeout = explorerProxyTimeout }()

	done := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		explorer.ServeHTTP(rr, testnetCookieRequest("/"))
		done <- rr.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Fatalf("stalled upstream returned %d, want 502", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy never returned; the client has no timeout")
	}
}

// /events is an endless stream. Proxying it through the buffering path would
// never return, so it must be forwarded chunk by chunk.
func TestProxyStreamsEventsInsteadOfBuffering(t *testing.T) {
	if params.IsTestnet {
		t.Skip("proxy only runs on mainnet")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "event: connected\ndata: {}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	explorer := mustProxyingExplorer(t, upstream)
	server := httptest.NewServer(explorer)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/events", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "net", Value: "testnet"})

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxied event stream failed: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read no bytes from proxied stream: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "event: connected") {
		t.Fatalf("proxied stream first chunk = %q, want the connected event", buf[:n])
	}
}
