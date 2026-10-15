package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAPICoversV1Routes asserts every registered /v1 route of the route
// table appears in the full OpenAPI document (UPGRADE §4.6). The full
// protocol is only shipped as docs/openapi.full.json; the served document is
// the basic profile (discovery_test.go).
func TestOpenAPICoversV1Routes(t *testing.T) {
	doc := buildFullOpenAPI("http://example.com")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range routeTable {
		if !strings.HasPrefix(r.Path, "/v1") {
			continue
		}
		item, ok := parsed.Paths[r.Path]
		if !ok {
			t.Errorf("path %s missing from openapi.json", r.Path)
			continue
		}
		method := strings.ToLower(r.Method)
		if method == "" {
			method = "get"
		}
		if _, ok := item[method]; !ok {
			t.Errorf("%s %s missing from openapi.json", r.Method, r.Path)
			continue
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no /v1 routes found")
	}
	// The default-drive aliases must be documented too.
	for _, want := range []string{"/v1/files", "/v1/list", "/v1/trash", "/l/{token}"} {
		if _, ok := parsed.Paths[want]; !ok {
			t.Errorf("path %s missing from openapi.json", want)
		}
	}
	// Phase 1b endpoints must be documented (UPGRADE §4.10).
	for _, want := range []string{
		"/v1/selftest", "/v1/tree", "/v1/search",
		"/v1/transfers", "/v1/fetch", "/v1/batch",
		"/v1/jobs", "/v1/jobs/{id}", "/v1/jobs/{id}/cancel",
		"/v1/keys", "/v1/keys/{id}",
		"/v1/activity",
	} {
		if _, ok := parsed.Paths[want]; !ok {
			t.Errorf("phase 1b path %s missing from openapi.json", want)
		}
	}
	// The removed approval endpoints must stay gone.
	for _, gone := range []string{"/v1/requests", "/v1/requests/{id}"} {
		if _, ok := parsed.Paths[gone]; ok {
			t.Errorf("removed path %s still documented", gone)
		}
	}
	// Servers carries the base URL.
	servers := doc["servers"].([]map[string]string)
	if len(servers) != 1 || servers[0]["url"] != "http://example.com" {
		t.Errorf("servers = %v", servers)
	}
}

// TestOpenAPIHandlerServesJSON checks the endpoint wiring.
func TestOpenAPIHandlerServesJSON(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/apidocumentation/openapi.json", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"/v1/me"`) {
		t.Error("openapi body lacks /v1/me")
	}
}

// TestInstructionsAssets verifies the onboarding assets render with the
// request host and never contain a key.
func TestInstructionsAssets(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{"/custom/agent-instructions.txt", "/custom/c2a.py", "/custom/SKILL.md"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "example.com"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", target, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, chrootPlain) || strings.Contains(body, legacyPlain) {
			t.Errorf("%s leaks a key", target)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/custom/agent-instructions.txt", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	for _, want := range []string{
		"http://example.com/v1/me",
		"CRC-64/XZ",
		"11051210869376104954",
		"Idempotency-Key",
		"c2a-trash",
		"Autonomy boundaries",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("instructions lack %q", want)
		}
	}
}
