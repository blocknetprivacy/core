package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultHandleResolverBaseURL = "https://blocknet.id"
	handleResolverTimeout        = 3 * time.Second
	signingPubkeyCacheTTL        = 10 * time.Minute
)

type resolvedHandle struct {
	Handle   string
	Address  string
	Verified bool
}

type handleResolveResponse struct {
	Address   string `json:"address"`
	Handle    string `json:"handle"`
	Payload   string `json:"payload"`
	Sig       string `json:"sig"`
	TTL       int    `json:"ttl"`
	UpdatedAt int64  `json:"updated_at"`
}

type handleSignedPayload struct {
	Handle        string `json:"handle"`
	Address       string `json:"address"`
	UpdatedAtUnix int64  `json:"updated_at_unix"`
}

type wellKnownResponse struct {
	SigningPubKey string `json:"signing_pubkey"`
}

var handleResolverPubkeyCache struct {
	mu        sync.RWMutex
	pub       ed25519.PublicKey
	fetchedAt time.Time
}

func resolveRecipientAddress(recipient string) (resolvedAddress string, info *resolvedHandle, err error) {
	if !isHandleRecipient(recipient) {
		return recipient, nil, nil
	}

	handle, err := normalizeHandleRecipient(recipient)
	if err != nil {
		return "", nil, err
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BLOCKNET_ID_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = defaultHandleResolverBaseURL
	}

	client := &http.Client{Timeout: handleResolverTimeout}
	resolveURL := fmt.Sprintf("%s/api/v1/resolve/%s", baseURL, handle)
	resp, err := client.Get(resolveURL)
	if err != nil {
		return "", nil, fmt.Errorf("handle resolver unavailable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil, fmt.Errorf("handle not found")
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("handle resolver returned status %d", resp.StatusCode)
	}

	var out handleResolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("invalid resolver response")
	}

	if strings.TrimSpace(out.Address) == "" || strings.TrimSpace(out.Payload) == "" || strings.TrimSpace(out.Sig) == "" {
		return "", nil, fmt.Errorf("incomplete resolver response")
	}

	pub, err := getHandleResolverPublicKey(baseURL, client)
	if err != nil {
		return "", nil, err
	}

	if err := verifyResolvedHandle(pub, handle, &out); err != nil {
		return "", nil, err
	}

	return out.Address, &resolvedHandle{
		Handle:   handle,
		Address:  out.Address,
		Verified: true,
	}, nil
}

func getHandleResolverPublicKey(baseURL string, client *http.Client) (ed25519.PublicKey, error) {
	if envPub := strings.TrimSpace(os.Getenv("BLOCKNET_ID_PUBKEY")); envPub != "" {
		decoded, err := base64.StdEncoding.DecodeString(envPub)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid BLOCKNET_ID_PUBKEY")
		}
		return ed25519.PublicKey(decoded), nil
	}

	handleResolverPubkeyCache.mu.RLock()
	if len(handleResolverPubkeyCache.pub) == ed25519.PublicKeySize &&
		time.Since(handleResolverPubkeyCache.fetchedAt) < signingPubkeyCacheTTL {
		pub := append(ed25519.PublicKey(nil), handleResolverPubkeyCache.pub...)
		handleResolverPubkeyCache.mu.RUnlock()
		return pub, nil
	}
	handleResolverPubkeyCache.mu.RUnlock()

	wellKnownURL := baseURL + "/.well-known/blocknet-id.json"
	resp, err := client.Get(wellKnownURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch resolver signing key")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch resolver signing key")
	}

	var out wellKnownResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid resolver signing key response")
	}

	keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.SigningPubKey))
	if err != nil || len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid resolver signing key")
	}
	pub := ed25519.PublicKey(keyBytes)

	handleResolverPubkeyCache.mu.Lock()
	handleResolverPubkeyCache.pub = append(ed25519.PublicKey(nil), pub...)
	handleResolverPubkeyCache.fetchedAt = time.Now()
	handleResolverPubkeyCache.mu.Unlock()

	return pub, nil
}

func verifyResolvedHandle(pub ed25519.PublicKey, requestedHandle string, resolved *handleResolveResponse) error {
	sigBytes, err := base64.StdEncoding.DecodeString(resolved.Sig)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("invalid resolver signature")
	}

	payloadBytes := []byte(resolved.Payload)
	if !ed25519.Verify(pub, payloadBytes, sigBytes) {
		return fmt.Errorf("resolver signature verification failed")
	}

	var signed handleSignedPayload
	if err := json.Unmarshal(payloadBytes, &signed); err != nil {
		return fmt.Errorf("invalid resolver payload")
	}

	if !strings.EqualFold(strings.TrimSpace(signed.Handle), strings.TrimSpace(requestedHandle)) {
		return fmt.Errorf("resolver payload handle mismatch")
	}
	if strings.TrimSpace(signed.Address) != strings.TrimSpace(resolved.Address) {
		return fmt.Errorf("resolver payload address mismatch")
	}
	if resolved.UpdatedAt > 0 && signed.UpdatedAtUnix != resolved.UpdatedAt {
		return fmt.Errorf("resolver payload timestamp mismatch")
	}
	if resolved.Handle != "" && !strings.EqualFold(strings.TrimSpace(signed.Handle), strings.TrimSpace(resolved.Handle)) {
		return fmt.Errorf("resolver response handle mismatch")
	}

	return nil
}

func isHandleRecipient(s string) bool {
	if s == "" {
		return false
	}
	return s[0] == '@' || s[0] == '$'
}

func normalizeHandleRecipient(s string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(s))
	h = strings.TrimLeft(h, "@$")
	if h == "" {
		return "", fmt.Errorf("empty handle")
	}
	if len(h) > 64 {
		return "", fmt.Errorf("handle too long")
	}
	for _, c := range h {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return "", fmt.Errorf("invalid handle")
		}
	}
	return h, nil
}
