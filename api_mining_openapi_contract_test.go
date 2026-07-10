package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestOpenAPIMiningTemplateLeaseAndCompactSubmitContract(t *testing.T) {
	specBytes, err := os.ReadFile("api_openapi.json")
	if err != nil {
		t.Fatalf("failed to read api_openapi.json: %v", err)
	}

	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("failed to parse api_openapi.json: %v", err)
	}

	components := mustGetMapAny(t, spec, "components")
	schemas := mustGetMapAny(t, components, "schemas")

	blockTemplate := mustGetMapAny(t, schemas, "BlockTemplate")
	blockTemplateProps := mustGetMapAny(t, blockTemplate, "properties")
	if _, ok := blockTemplateProps["template_id"]; !ok {
		t.Fatal("BlockTemplate schema missing template_id")
	}
	if _, ok := blockTemplateProps["template_expires_at_unix_ms"]; !ok {
		t.Fatal("BlockTemplate schema missing template_expires_at_unix_ms")
	}

	renewRequest := mustGetMapAny(t, schemas, "RenewBlockTemplateRequest")
	renewRequestProps := mustGetMapAny(t, renewRequest, "properties")
	if _, ok := renewRequestProps["template_id"]; !ok {
		t.Fatal("RenewBlockTemplateRequest missing template_id")
	}
	renewResponse := mustGetMapAny(t, schemas, "RenewBlockTemplateResponse")
	renewResponseProps := mustGetMapAny(t, renewResponse, "properties")
	if _, ok := renewResponseProps["template_expires_at_unix_ms"]; !ok {
		t.Fatal("RenewBlockTemplateResponse missing template_expires_at_unix_ms")
	}

	compactSubmit := mustGetMapAny(t, schemas, "CompactSubmitBlock")
	compactProps := mustGetMapAny(t, compactSubmit, "properties")
	if _, ok := compactProps["template_id"]; !ok {
		t.Fatal("CompactSubmitBlock missing template_id")
	}
	if _, ok := compactProps["nonce"]; !ok {
		t.Fatal("CompactSubmitBlock missing nonce")
	}

	paths := mustGetMapAny(t, spec, "paths")
	blockTemplatePath := mustGetMapAny(t, paths, "/api/mining/blocktemplate")
	blockTemplateGet := mustGetMapAny(t, blockTemplatePath, "get")
	blockTemplateResponses := mustGetMapAny(t, blockTemplateGet, "responses")
	blockTemplateUnavailable := mustGetMapAny(t, blockTemplateResponses, "503")
	unavailableContent := mustGetMapAny(t, blockTemplateUnavailable, "content")
	unavailableJSON := mustGetMapAny(t, unavailableContent, "application/json")
	unavailableExamples := mustGetMapAny(t, unavailableJSON, "examples")
	if _, ok := unavailableExamples["lease_capacity"]; !ok {
		t.Fatal("blocktemplate 503 contract missing lease_capacity example")
	}

	renewPath := mustGetMapAny(t, paths, "/api/mining/renewtemplate")
	renewPost := mustGetMapAny(t, renewPath, "post")
	renewBody := mustGetMapAny(t, renewPost, "requestBody")
	renewContent := mustGetMapAny(t, renewBody, "content")
	renewJSON := mustGetMapAny(t, renewContent, "application/json")
	renewSchema := mustGetMapAny(t, renewJSON, "schema")
	if got := renewSchema["$ref"]; got != "#/components/schemas/RenewBlockTemplateRequest" {
		t.Fatalf("renewtemplate request schema mismatch: got %#v", got)
	}
	renewResponses := mustGetMapAny(t, renewPost, "responses")
	for _, status := range []string{"200", "400", "401", "404", "409"} {
		if _, ok := renewResponses[status]; !ok {
			t.Fatalf("renewtemplate responses missing status %s", status)
		}
	}

	submitPath := mustGetMapAny(t, paths, "/api/mining/submitblock")
	submitPost := mustGetMapAny(t, submitPath, "post")
	requestBody := mustGetMapAny(t, submitPost, "requestBody")
	content := mustGetMapAny(t, requestBody, "content")
	appJSON := mustGetMapAny(t, content, "application/json")
	reqSchema := mustGetMapAny(t, appJSON, "schema")

	oneOf, ok := reqSchema["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("submitblock request schema must be oneOf with 2 options, got %#v", reqSchema["oneOf"])
	}
}

func mustGetMapAny(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in OpenAPI object", key)
	}
	out, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("key %q is not an object in OpenAPI", key)
	}
	return out
}
