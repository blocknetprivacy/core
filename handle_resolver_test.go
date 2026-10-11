package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveRecipientAddressPassThrough(t *testing.T) {
	got, info, err := resolveRecipientAddress("TCpDTBpjwaHcjWEXJwoT5MHrFpoi9cTuftzNLN7bNoBJjCSJEH5ZGXwNcUauhjgoV2ocF9RJWm8Ui2k1QAkAYrwMgG8Wk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info != nil {
		t.Fatalf("expected nil info for plain address")
	}
	if got == "" {
		t.Fatal("resolved address is empty")
	}
}

func TestResolveRecipientAddressVerifiesSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const (
		handle    = "cubecabin"
		address   = "TCpDTBpjwaHcjWEXJwoT5MHrFpoi9cTuftzNLN7bNoBJjCSJEH5ZGXwNcUauhjgoV2ocF9RJWm8Ui2k1QAkAYrwMgG8Wk"
		updatedAt = int64(1771631224)
	)

	payload, sig := mustSignHandlePayload(t, priv, handle, address, updatedAt)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/blocknet-id.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"signing_pubkey": base64.StdEncoding.EncodeToString(pub),
			})
		case "/api/v1/resolve/cubecabin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"address":    address,
				"handle":     handle,
				"payload":    payload,
				"sig":        sig,
				"ttl":        60,
				"updated_at": updatedAt,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldBaseURL := handleResolverBaseURL
	handleResolverBaseURL = srv.URL
	t.Cleanup(func() { handleResolverBaseURL = oldBaseURL })
	resetHandleResolverPubkeyCache()

	gotAddr, info, err := resolveRecipientAddress("@CubeCabin")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if gotAddr != address {
		t.Fatalf("address mismatch: got %q want %q", gotAddr, address)
	}
	if info == nil || !info.Verified || info.Handle != handle {
		t.Fatalf("unexpected resolve info: %#v", info)
	}
}

func TestResolveRecipientAddressRejectsInvalidSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const (
		handle    = "cubecabin"
		address   = "TCpDTBpjwaHcjWEXJwoT5MHrFpoi9cTuftzNLN7bNoBJjCSJEH5ZGXwNcUauhjgoV2ocF9RJWm8Ui2k1QAkAYrwMgG8Wk"
		updatedAt = int64(1771631224)
	)

	payload, _ := mustSignHandlePayload(t, priv, handle, address, updatedAt)
	badSig := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/blocknet-id.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"signing_pubkey": base64.StdEncoding.EncodeToString(pub),
			})
		case "/api/v1/resolve/cubecabin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"address":    address,
				"handle":     handle,
				"payload":    payload,
				"sig":        badSig,
				"ttl":        60,
				"updated_at": updatedAt,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldBaseURL := handleResolverBaseURL
	handleResolverBaseURL = srv.URL
	t.Cleanup(func() { handleResolverBaseURL = oldBaseURL })
	resetHandleResolverPubkeyCache()

	if _, _, err := resolveRecipientAddress("$cubecabin"); err == nil {
		t.Fatal("expected signature verification error")
	}
}

func mustSignHandlePayload(t *testing.T, priv ed25519.PrivateKey, handle, address string, updatedAt int64) (string, string) {
	t.Helper()
	p := handleSignedPayload{
		Handle:        handle,
		Address:       address,
		UpdatedAtUnix: updatedAt,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(raw), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))
}

func resetHandleResolverPubkeyCache() {
	handleResolverPubkeyCache.mu.Lock()
	defer handleResolverPubkeyCache.mu.Unlock()
	handleResolverPubkeyCache.pub = nil
	handleResolverPubkeyCache.fetchedAt = time.Time{}
}
