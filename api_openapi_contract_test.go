package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blocknet/wallet"
)

func TestOpenAPIAndHandlerMemoContractParity(t *testing.T) {
	// --- Handler-side behavior (real mux, real auth middleware, real wallet) ---
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	// We only need a "current height" that makes our synthetic output mature.
	chain.mu.Lock()
	chain.height = 100
	chain.mu.Unlock()

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	recipient, err := wallet.NewWallet(filepath.Join(t.TempDir(), "recipient.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create recipient wallet: %v", err)
	}

	// Give the wallet one mature output so handleSend reaches memo validation
	// before failing later in the expensive build path.
	w.AddOutput(&wallet.OwnedOutput{
		TxID:        [32]byte{0x01},
		OutputIndex: 0,
		Amount:      10_000_000,
		BlockHeight: 0,
		IsCoinbase:  false,
		Spent:       false,
	})
	// And one output with a decrypted memo so handleHistory includes memo_hex.
	w.AddOutput(&wallet.OwnedOutput{
		TxID:        [32]byte{0x02},
		OutputIndex: 1,
		Amount:      1,
		BlockHeight: 0,
		IsCoinbase:  false,
		Spent:       false,
		Memo:        []byte{0x01, 0x02, 0x03},
	})

	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)

	// Mirror production handler chain.
	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	authz := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}

	doReq := func(method, path string, body []byte, headers map[string]string, remoteAddr string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = remoteAddr
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	// Mutual exclusion: memo_text + memo_hex must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipient.Address() + `","amount":1,"memo_text":"hi","memo_hex":"00"}]}`)
		rr := doReq("POST", "/api/wallet/send", body, authz, "198.51.100.10:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for memo mutual exclusion, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "provide either memo_text or memo_hex") {
			t.Fatalf("unexpected error body for mutual exclusion: %s", rr.Body.String())
		}
	}

	// Hex validation: odd-length or non-hex memo_hex must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipient.Address() + `","amount":1,"memo_hex":"0"}]}`)
		rr := doReq("POST", "/api/wallet/send", body, authz, "198.51.100.11:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid memo_hex, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "invalid memo_hex") {
			t.Fatalf("unexpected error body for invalid memo_hex: %s", rr.Body.String())
		}
	}

	// Length bound: memo payload must be <= 124 bytes.
	{
		long := strings.Repeat("a", wallet.MemoSize-3) // 125 bytes; over wallet limit (124)
		body := []byte(`{"recipients":[{"address":"` + recipient.Address() + `","amount":1,"memo_text":"` + long + `"}]}`)
		rr := doReq("POST", "/api/wallet/send", body, authz, "198.51.100.12:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for too-long memo_text, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "memo too long") {
			t.Fatalf("unexpected error body for too-long memo_text: %s", rr.Body.String())
		}
	}

	// History response should surface memo_hex (hex) when memo payload is present.
	{
		rr := doReq("GET", "/api/wallet/history", nil, authz, "198.51.100.13:1234")
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 from history, got %d: %s", rr.Code, rr.Body.String())
		}
		var got struct {
			Count   int                      `json:"count"`
			Outputs []map[string]interface{} `json:"outputs"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode history JSON: %v", err)
		}
		if got.Count == 0 || len(got.Outputs) == 0 {
			t.Fatalf("expected history outputs, got count=%d len(outputs)=%d", got.Count, len(got.Outputs))
		}
		foundMemo := false
		for _, out := range got.Outputs {
			if v, ok := out["memo_hex"]; ok {
				s, ok := v.(string)
				if !ok {
					t.Fatalf("memo_hex is not string: %#v", v)
				}
				if s != "010203" {
					// We inserted a known memo; ensure it round-trips as hex.
					t.Fatalf("unexpected memo_hex: got %q want %q", s, "010203")
				}
				foundMemo = true
			}
			// Ensure legacy field name does not reappear.
			if _, ok := out["payment_id"]; ok {
				t.Fatalf("unexpected payment_id field in history output: %#v", out)
			}
		}
		if !foundMemo {
			t.Fatal("expected at least one output with memo_hex")
		}
	}

	// --- OpenAPI-side contract (schema reflects handler behavior) ---
	specBytes, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatalf("failed to read api_openapi.json: %v", err)
	}

	var spec map[string]interface{}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("failed to parse api_openapi.json: %v", err)
	}

	// Validate SendRequest has recipients and SendRecipient has memo fields.
	components := mustGetMap(t, spec, "components")
	allSchemas := mustGetMap(t, components, "schemas")
	sendReq := mustGetMap(t, allSchemas, "SendRequest")
	sendReqProps := mustGetMap(t, sendReq, "properties")

	if _, ok := sendReqProps["recipients"]; !ok {
		t.Fatal("SendRequest missing recipients in OpenAPI")
	}
	if _, ok := sendReqProps["dry_run"]; !ok {
		t.Fatal("SendRequest missing dry_run in OpenAPI")
	}

	sendResp := mustGetMap(t, allSchemas, "SendResponse")
	sendRespProps := mustGetMap(t, sendResp, "properties")
	for _, field := range []string{"txid", "fee", "change", "dry_run", "input_total", "input_count", "recipients"} {
		if _, ok := sendRespProps[field]; !ok {
			t.Fatalf("SendResponse missing %q in OpenAPI", field)
		}
	}

	// Memo fields live in the shared SendRecipient schema.
	recipientSchema := mustGetMap(t, allSchemas, "SendRecipient")
	props := mustGetMap(t, recipientSchema, "properties")

	if _, ok := props["memo_text"]; !ok {
		t.Fatal("SendRecipient missing memo_text in OpenAPI")
	}
	if _, ok := props["memo_hex"]; !ok {
		t.Fatal("SendRecipient missing memo_hex in OpenAPI")
	}
	if _, ok := props["payment_id"]; ok {
		t.Fatal("SendRecipient unexpectedly includes legacy payment_id in OpenAPI")
	}

	// Ensure schema encodes the same bounds we enforce in handlers.
	memoText := mustGetMap(t, props, "memo_text")
	if got, ok := memoText["maxLength"].(float64); !ok || int(got) != 124 {
		t.Fatalf("SendRecipient.memo_text maxLength mismatch: got %#v want 124", memoText["maxLength"])
	}
	memoHex := mustGetMap(t, props, "memo_hex")
	if got, ok := memoHex["maxLength"].(float64); !ok || int(got) != 248 {
		t.Fatalf("SendRecipient.memo_hex maxLength mismatch: got %#v want 248", memoHex["maxLength"])
	}
	if pat, ok := memoHex["pattern"].(string); !ok || !strings.Contains(pat, "{2}") {
		t.Fatalf("SendRecipient.memo_hex pattern missing even-length constraint: got %#v", memoHex["pattern"])
	}

	// Ensure `/api/wallet/send` documents idempotency header support.
	paths := mustGetMap(t, spec, "paths")
	sendPath := mustGetMap(t, paths, "/api/wallet/send")
	sendPost := mustGetMap(t, sendPath, "post")
	rawParams, ok := sendPost["parameters"]
	if !ok {
		t.Fatal("/api/wallet/send post missing parameters")
	}
	params, ok := rawParams.([]interface{})
	if !ok {
		t.Fatal("/api/wallet/send post parameters is not an array")
	}
	foundIdem := false
	for _, p := range params {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := pm["name"].(string); strings.EqualFold(name, "Idempotency-Key") {
			foundIdem = true
			break
		}
	}
	if !foundIdem {
		t.Fatal("/api/wallet/send post missing Idempotency-Key parameter")
	}
}

func TestOpenAPISendAdvancedContractParity(t *testing.T) {
	// --- Handler-side behavior ---
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	chain.height = 100
	chain.mu.Unlock()

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	walletFile := filepath.Join(t.TempDir(), "wallet.dat")
	w, err := wallet.NewWallet(walletFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	// Give the wallet a mature output so ReserveSpecificInputs can find it.
	var knownTxID [32]byte
	knownTxID[0] = 0xAA
	w.AddOutput(&wallet.OwnedOutput{
		TxID:        knownTxID,
		OutputIndex: 0,
		Amount:      10_000_000,
		BlockHeight: 1,
		IsCoinbase:  false,
		Spent:       false,
	})

	// Second wallet for a valid non-self recipient address.
	recipientFile := filepath.Join(t.TempDir(), "recipient.dat")
	recipientWallet, err := wallet.NewWallet(recipientFile, []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create recipient wallet: %v", err)
	}
	recipientAddr := recipientWallet.Address()

	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)

	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	authz := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}

	doReq := func(body []byte, remoteAddr string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/wallet/send/advanced", bytes.NewReader(body))
		req.RemoteAddr = remoteAddr
		for k, v := range authz {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	knownTxIDHex := fmt.Sprintf("%x", knownTxID)

	// Missing inputs array must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1}]}`)
		rr := doReq(body, "198.51.100.20:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing inputs, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "inputs array is required") {
			t.Fatalf("unexpected error for missing inputs: %s", rr.Body.String())
		}
	}

	// Empty inputs array must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1}],"inputs":[]}`)
		rr := doReq(body, "198.51.100.21:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty inputs, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "inputs array is required") {
			t.Fatalf("unexpected error for empty inputs: %s", rr.Body.String())
		}
	}

	// Bad txid length must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1}],"inputs":[{"txid":"abcd","output_index":0}]}`)
		rr := doReq(body, "198.51.100.22:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad txid, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "txid must be 64 hex") {
			t.Fatalf("unexpected error for bad txid: %s", rr.Body.String())
		}
	}

	// Memo mutual exclusion must fail.
	{
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1,"memo_text":"hi","memo_hex":"00"}],"inputs":[{"txid":"` + knownTxIDHex + `","output_index":0}]}`)
		rr := doReq(body, "198.51.100.23:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for memo mutual exclusion, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "provide either memo_text or memo_hex") {
			t.Fatalf("unexpected error for memo exclusion: %s", rr.Body.String())
		}
	}

	// Memo too long must fail.
	{
		long := strings.Repeat("a", wallet.MemoSize-3)
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1,"memo_text":"` + long + `"}],"inputs":[{"txid":"` + knownTxIDHex + `","output_index":0}]}`)
		rr := doReq(body, "198.51.100.24:1234")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for memo too long, got %d: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "memo too long") {
			t.Fatalf("unexpected error for memo length: %s", rr.Body.String())
		}
	}

	// Dry run with valid inputs must return 200 with fee/change/input_total.
	{
		body := []byte(`{"recipients":[{"address":"` + recipientAddr + `","amount":1000}],"dry_run":true,"inputs":[{"txid":"` + knownTxIDHex + `","output_index":0}]}`)
		rr := doReq(body, "198.51.100.25:1234")
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for dry run, got %d: %s", rr.Code, rr.Body.String())
		}
		var got map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("failed to decode dry run response: %v", err)
		}
		if dryRun, ok := got["dry_run"].(bool); !ok || !dryRun {
			t.Fatalf("expected dry_run=true, got %#v", got["dry_run"])
		}
		for _, required := range []string{"fee", "change", "input_total", "input_count", "recipients"} {
			if _, ok := got[required]; !ok {
				t.Fatalf("dry run response missing %q", required)
			}
		}
		if _, ok := got["txid"]; ok {
			t.Fatal("dry run response should not include txid")
		}
		if total, ok := got["input_total"].(float64); !ok || int(total) != 10_000_000 {
			t.Fatalf("expected input_total=10000000, got %#v", got["input_total"])
		}
	}

	// --- OpenAPI-side contract ---
	specBytes, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatalf("failed to read api_openapi.json: %v", err)
	}
	var spec map[string]interface{}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("failed to parse api_openapi.json: %v", err)
	}

	components := mustGetMap(t, spec, "components")
	allSchemas := mustGetMap(t, components, "schemas")

	// --- SendAdvancedRequest schema ---
	advReq := mustGetMap(t, allSchemas, "SendAdvancedRequest")
	advProps := mustGetMap(t, advReq, "properties")

	for _, field := range []string{"recipients", "inputs", "dry_run", "change_split"} {
		if _, ok := advProps[field]; !ok {
			t.Fatalf("SendAdvancedRequest missing %q in OpenAPI", field)
		}
	}

	// inputs must have minItems and maxItems.
	inputsProp := mustGetMap(t, advProps, "inputs")
	if got, ok := inputsProp["minItems"].(float64); !ok || int(got) != 1 {
		t.Fatalf("inputs.minItems mismatch: got %#v want 1", inputsProp["minItems"])
	}
	if got, ok := inputsProp["maxItems"].(float64); !ok || int(got) != 256 {
		t.Fatalf("inputs.maxItems mismatch: got %#v want 256", inputsProp["maxItems"])
	}

	// change_split must have minimum and maximum.
	changeSplitProp := mustGetMap(t, advProps, "change_split")
	if got, ok := changeSplitProp["minimum"].(float64); !ok || int(got) != 1 {
		t.Fatalf("change_split.minimum mismatch: got %#v want 1", changeSplitProp["minimum"])
	}
	if got, ok := changeSplitProp["maximum"].(float64); !ok || int(got) != 4 {
		t.Fatalf("change_split.maximum mismatch: got %#v want 4", changeSplitProp["maximum"])
	}

	// --- SendAdvancedResponse schema ---
	advResp := mustGetMap(t, allSchemas, "SendAdvancedResponse")
	respProps := mustGetMap(t, advResp, "properties")

	for _, field := range []string{"txid", "fee", "change", "change_split", "input_total", "input_count", "dry_run", "recipients"} {
		if _, ok := respProps[field]; !ok {
			t.Fatalf("SendAdvancedResponse missing %q in OpenAPI", field)
		}
	}

	// --- Endpoint path ---
	paths := mustGetMap(t, spec, "paths")
	advPath := mustGetMap(t, paths, "/api/wallet/send/advanced")
	advPost := mustGetMap(t, advPath, "post")

	// Must document Idempotency-Key header.
	rawParams, ok := advPost["parameters"]
	if !ok {
		t.Fatal("/api/wallet/send/advanced missing parameters")
	}
	params, ok := rawParams.([]interface{})
	if !ok {
		t.Fatal("/api/wallet/send/advanced parameters is not an array")
	}
	foundIdem := false
	for _, p := range params {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := pm["name"].(string); strings.EqualFold(name, "Idempotency-Key") {
			foundIdem = true
			break
		}
	}
	if !foundIdem {
		t.Fatal("/api/wallet/send/advanced missing Idempotency-Key parameter")
	}

	// Must document 429 response.
	responses := mustGetMap(t, advPost, "responses")
	if _, ok := responses["429"]; !ok {
		t.Fatal("/api/wallet/send/advanced missing 429 response in OpenAPI")
	}

	// Must document 409 response.
	if _, ok := responses["409"]; !ok {
		t.Fatal("/api/wallet/send/advanced missing 409 response in OpenAPI")
	}
}

func mustGetMap(t *testing.T, m map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in OpenAPI object", key)
	}
	out, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("key %q is not an object in OpenAPI", key)
	}
	return out
}
