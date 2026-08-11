package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// detectPublicIP talks to third parties. The reply must be read under a cap:
// an unbounded read lets a hostile or broken service exhaust node memory at
// startup, inside the client timeout.
func TestDetectPublicIPCapsTheResponse(t *testing.T) {
	const flood = 4 << 20

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("9", 64<<10)
		for written := 0; written < flood; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	original := publicIPServices
	publicIPServices = []string{server.URL}
	defer func() { publicIPServices = original }()

	ip, err := detectPublicIP()
	if err != nil {
		t.Fatalf("detectPublicIP failed: %v", err)
	}
	if len(ip) > publicIPMaxResponseBytes {
		t.Fatalf("read %d bytes from the service; the response is not capped at %d", len(ip), publicIPMaxResponseBytes)
	}
}
