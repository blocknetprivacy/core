package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type openAPIDoc struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schemas"`
	} `json:"components"`
}

func loadOpenAPIDoc(t *testing.T) openAPIDoc {
	t.Helper()
	raw, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc openAPIDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func schemaProps(t *testing.T, doc openAPIDoc, name string) map[string]bool {
	t.Helper()
	s, ok := doc.Components.Schemas[name]
	if !ok || len(s.Properties) == 0 {
		t.Fatalf("schema %s missing or empty", name)
	}
	out := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func checkShape(t *testing.T, doc openAPIDoc, schema string, responses ...map[string]any) {
	t.Helper()
	props := schemaProps(t, doc, schema)
	seen := map[string]bool{}
	for _, r := range responses {
		for k := range r {
			seen[k] = true
			if !props[k] {
				t.Errorf("%s: handler returns %q, which the spec does not document", schema, k)
			}
		}
	}
	for _, k := range sortedKeys(props) {
		if !seen[k] {
			t.Errorf("%s: spec documents %q, which no call returned (seen %v)", schema, k, sortedKeys(seen))
		}
	}
}

func checkRequestDocumented(t *testing.T, doc openAPIDoc, schema string, fields ...string) {
	t.Helper()
	props := schemaProps(t, doc, schema)
	for _, f := range fields {
		if !props[f] {
			t.Errorf("%s: handler accepts %q, which the spec does not document", schema, f)
		}
	}
	if len(props) != len(fields) {
		t.Errorf("%s: spec documents %v, handler accepts %v", schema, sortedKeys(props), fields)
	}
}

func TestOpenAPIFeeAndBumpShapesMatchHandlers(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	f := newBumpFixture(t, 30_000_000, 30_000_000)

	checkRequestDocumented(t, doc, "SendRequest", "recipients", "dry_run", "extra_fee_per_byte")
	checkRequestDocumented(t, doc, "SendAdvancedRequest", "recipients", "inputs", "dry_run", "change_split", "extra_fee_per_byte")
	checkRequestDocumented(t, doc, "BumpRequest", "txid", "extra_fee_per_byte", "dry_run")

	rr := httptest.NewRecorder()
	f.api.handleMempool(rr, httptest.NewRequest(http.MethodGet, "/api/mempool", nil))
	var mempool map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mempool)
	checkShape(t, doc, "MempoolStats", mempool)

	body := `{"recipients":[{"address":"` + f.w.Address() + `","amount":1000000}],"extra_fee_per_byte":10`
	_, sendDry := f.post(t, "/api/wallet/send", body+`,"dry_run":true}`)
	code, sendReal := f.post(t, "/api/wallet/send", body+`}`)
	if code != http.StatusOK {
		t.Fatalf("send: %d %v", code, sendReal)
	}
	checkShape(t, doc, "SendResponse", sendDry, sendReal)

	outs := f.w.SpendableOutputs()
	if len(outs) == 0 {
		t.Fatal("no spendable output left for the advanced send")
	}
	in := `"inputs":[{"txid":"` + hex.EncodeToString(outs[0].TxID[:]) + `","output_index":` + strconv.Itoa(outs[0].OutputIndex) + `}]`
	adv := `{"recipients":[{"address":"` + f.w.Address() + `","amount":1000000}],"extra_fee_per_byte":10,` + in
	advDry := f.postAdvanced(t, adv+`,"dry_run":true}`)
	advReal := f.postAdvanced(t, adv+`}`)
	checkShape(t, doc, "SendAdvancedResponse", advDry, advReal)

	txid := sendReal["txid"].(string)
	_, bumpDry := f.bump(t, txid, "10", true)
	code, bumpReal := f.bump(t, txid, "10", false)
	if code != http.StatusOK {
		t.Fatalf("bump: %d %v", code, bumpReal)
	}
	checkShape(t, doc, "BumpResponse", bumpDry, bumpReal)

	rr = httptest.NewRecorder()
	f.api.handleSends(rr, httptest.NewRequest(http.MethodGet, "/api/wallet/sends", nil))
	var list struct {
		Sends []map[string]any `json:"sends"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	checkShape(t, doc, "WalletSendEntry", list.Sends...)
}

func (f *bumpFixture) postAdvanced(t *testing.T, body string) map[string]any {
	t.Helper()
	f.n++
	req := httptest.NewRequest(http.MethodPost, "/api/wallet/send/advanced", strings.NewReader(body))
	req.RemoteAddr = "198.51.100." + strconv.Itoa(f.n) + ":1234"
	rr := httptest.NewRecorder()
	f.api.handleSendAdvanced(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("advanced send: %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func TestOpenAPIFeeAndBumpExamplesUseDocumentedFields(t *testing.T) {
	raw, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	doc := loadOpenAPIDoc(t)
	var full struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Examples map[string]struct {
						Value any `json:"value"`
					} `json:"examples"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]struct {
				Content map[string]struct {
					Example  any `json:"example"`
					Examples map[string]struct {
						Value any `json:"value"`
					} `json:"examples"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Example any `json:"example"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	check := func(where, schema string, v any) {
		value, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: example is not an object", where)
			return
		}
		props := schemaProps(t, doc, schema)
		for k := range value {
			if !props[k] {
				t.Errorf("%s: example uses %q, which %s does not document", where, k, schema)
			}
		}
	}
	for path, schemas := range map[string][2]string{
		"/api/wallet/send":          {"SendRequest", "SendResponse"},
		"/api/wallet/send/advanced": {"SendAdvancedRequest", "SendAdvancedResponse"},
		"/api/wallet/send/bump":     {"BumpRequest", "BumpResponse"},
		"/api/mempool":              {"", "MempoolStats"},
	} {
		for method, op := range full.Paths[path] {
			for _, c := range op.RequestBody.Content {
				for name, ex := range c.Examples {
					check(path+" "+method+" request "+name, schemas[0], ex.Value)
				}
			}
			for _, c := range op.Responses["200"].Content {
				if c.Example != nil {
					check(path+" "+method+" 200 example", schemas[1], c.Example)
				}
				for name, ex := range c.Examples {
					check(path+" "+method+" 200 "+name, schemas[1], ex.Value)
				}
			}
		}
	}
	for _, name := range []string{"SendRequest", "SendResponse", "SendAdvancedRequest", "SendAdvancedResponse", "BumpRequest", "BumpResponse", "MempoolStats", "WalletSendEntry"} {
		if ex := full.Components.Schemas[name].Example; ex != nil {
			check(name+" schema example", name, ex)
		}
	}
}

func TestOpenAPIBlockShapeMatchesHandler(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	f := newBumpFixture(t, 30_000_000)
	block := f.d.chain.GetBlockByHeight(0)
	if block == nil {
		t.Fatal("no genesis block")
	}
	out := blockToJSON(block, f.d.chain.Height())
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	checkShape(t, doc, "Block", got)
}
