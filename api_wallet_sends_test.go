package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"blocknet/wallet"
)

func TestHandleWalletSendsListsSendHistory(t *testing.T) {
	chain, _, cleanup := mustCreateTestChain(t)
	defer cleanup()
	mustAddGenesisBlock(t, chain)

	chain.mu.Lock()
	chain.height = 100
	chain.mu.Unlock()

	daemon, stopDaemon := mustStartTestDaemon(t, chain)
	defer stopDaemon()

	w, err := wallet.NewWallet(filepath.Join(t.TempDir(), "wallet.dat"), []byte("pw"), defaultWalletConfig())
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	var olderTxID [32]byte
	olderTxID[0] = 0x01
	w.RecordSend(&wallet.SendRecord{
		TxID:        olderTxID,
		Timestamp:   10,
		Fee:         100,
		BlockHeight: 8,
		Recipients: []wallet.SendRecipient{
			{Address: "older-recipient", Amount: 500},
		},
	})

	var newerTxID [32]byte
	newerTxID[0] = 0x02
	w.RecordSend(&wallet.SendRecord{
		TxID:        newerTxID,
		Timestamp:   20,
		Fee:         200,
		BlockHeight: 9,
		Recipients: []wallet.SendRecipient{
			{Address: "newer-recipient", Amount: 700, Memo: []byte{0xab, 0xcd}},
			{Address: "second-recipient", Amount: 300},
		},
	})

	api := NewAPIServer(daemon, w, nil, t.TempDir(), []byte("pw"))
	mux := http.NewServeMux()
	api.registerPublicRoutes(mux)
	api.registerPrivateRoutes(mux)

	token := "test-token"
	var handler http.Handler = mux
	handler = authMiddleware(token, handler)
	handler = maxBodySize(handler, maxRequestBodyBytes)

	doReq := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "198.51.100.40:1234"
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	type sendsResponse struct {
		Count  int    `json:"count"`
		Total  int    `json:"total"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		Order  string `json:"order"`
		Sends  []struct {
			TxID           string `json:"txid"`
			Timestamp      int64  `json:"timestamp"`
			RecordedHeight uint64 `json:"recorded_height"`
			ChainState     string `json:"chain_state"`
			Confirmations  uint64 `json:"confirmations"`
			InMempool      bool   `json:"in_mempool"`
			Fee            uint64 `json:"fee"`
			TotalAmount    uint64 `json:"total_amount"`
			Recipients     []struct {
				Address string `json:"address"`
				Amount  uint64 `json:"amount"`
				MemoHex string `json:"memo_hex"`
			} `json:"recipients"`
		} `json:"sends"`
	}

	rr := doReq("/api/wallet/sends?limit=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from sends, got %d: %s", rr.Code, rr.Body.String())
	}

	var got sendsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode sends JSON: %v", err)
	}
	if got.Count != 1 || got.Total != 2 || got.Limit != 1 || got.Offset != 0 || got.Order != "desc" {
		t.Fatalf("unexpected page metadata: %#v", got)
	}
	if len(got.Sends) != 1 {
		t.Fatalf("expected one send, got %d", len(got.Sends))
	}

	newer := got.Sends[0]
	if newer.TxID != fmt.Sprintf("%x", newerTxID) {
		t.Fatalf("unexpected first txid: got %s want %x", newer.TxID, newerTxID)
	}
	if newer.Timestamp != 20 || newer.RecordedHeight != 9 || newer.Fee != 200 || newer.TotalAmount != 1000 {
		t.Fatalf("unexpected newer send fields: %#v", newer)
	}
	if newer.ChainState != "not_found" || newer.Confirmations != 0 || newer.InMempool {
		t.Fatalf("unexpected chain state for synthetic tx: %#v", newer)
	}
	if len(newer.Recipients) != 2 {
		t.Fatalf("expected two recipients, got %d", len(newer.Recipients))
	}
	if newer.Recipients[0].Address != "newer-recipient" || newer.Recipients[0].Amount != 700 || newer.Recipients[0].MemoHex != "abcd" {
		t.Fatalf("unexpected first recipient: %#v", newer.Recipients[0])
	}

	rr = doReq("/api/wallet/sends?order=asc&limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 from ascending sends, got %d: %s", rr.Code, rr.Body.String())
	}
	got = sendsResponse{}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode ascending sends JSON: %v", err)
	}
	if got.Order != "asc" || len(got.Sends) != 2 || got.Sends[0].TxID != fmt.Sprintf("%x", olderTxID) {
		t.Fatalf("unexpected ascending order: %#v", got)
	}

	for _, path := range []string{
		"/api/wallet/sends?limit=-1",
		"/api/wallet/sends?offset=nope",
		"/api/wallet/sends?order=sideways",
	} {
		rr = doReq(path)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d: %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestOpenAPIWalletSendsContract(t *testing.T) {
	specBytes, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatalf("failed to read api_openapi.json: %v", err)
	}

	var spec map[string]interface{}
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("failed to parse api_openapi.json: %v", err)
	}

	paths := mustGetMap(t, spec, "paths")
	sendsPath := mustGetMap(t, paths, "/api/wallet/sends")
	sendsGet := mustGetMap(t, sendsPath, "get")
	rawParams, ok := sendsGet["parameters"].([]interface{})
	if !ok {
		t.Fatal("/api/wallet/sends get parameters is not an array")
	}

	foundParams := map[string]bool{}
	for _, p := range rawParams {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		foundParams[name] = true
	}
	for _, name := range []string{"limit", "offset", "order"} {
		if !foundParams[name] {
			t.Fatalf("/api/wallet/sends missing %q parameter", name)
		}
	}

	components := mustGetMap(t, spec, "components")
	schemas := mustGetMap(t, components, "schemas")
	response := mustGetMap(t, schemas, "WalletSendsResponse")
	responseProps := mustGetMap(t, response, "properties")
	for _, field := range []string{"count", "total", "limit", "offset", "order", "sends"} {
		if _, ok := responseProps[field]; !ok {
			t.Fatalf("WalletSendsResponse missing %q", field)
		}
	}

	entry := mustGetMap(t, schemas, "WalletSendEntry")
	entryProps := mustGetMap(t, entry, "properties")
	for _, field := range []string{"txid", "timestamp", "recorded_height", "chain_state", "confirmations", "in_mempool", "fee", "total_amount", "recipients"} {
		if _, ok := entryProps[field]; !ok {
			t.Fatalf("WalletSendEntry missing %q", field)
		}
	}

	chainState := mustGetMap(t, entryProps, "chain_state")
	rawEnum, ok := chainState["enum"].([]interface{})
	if !ok || len(rawEnum) != 3 {
		t.Fatalf("WalletSendEntry.chain_state enum mismatch: %#v", chainState["enum"])
	}

	recipient := mustGetMap(t, schemas, "WalletSendHistoryRecipient")
	recipientProps := mustGetMap(t, recipient, "properties")
	for _, field := range []string{"address", "amount", "memo_hex"} {
		if _, ok := recipientProps[field]; !ok {
			t.Fatalf("WalletSendHistoryRecipient missing %q", field)
		}
	}
}
