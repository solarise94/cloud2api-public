package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestKeysAdminAPI covers the §8.2 phase-1a key endpoints end to end: list,
// create (plaintext returned once + onboarding prompt), rotate, patch,
// revoke and the static-key 409.
func TestKeysAdminAPI(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	// A static key exists (installed by the harness).
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/keys", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	m := decodePortal(t, rec)
	keys := m["data"].(map[string]any)["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if keys[0].(map[string]any)["static"] != true {
		t.Error("static flag missing")
	}
	if _, ok := keys[0].(map[string]any)["trash"].(map[string]any); !ok {
		t.Error("trash settings missing")
	}

	// Static keys are read-only.
	rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/keys/k1", cookie, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("revoke static = %d", rec.Code)
	}

	// Create a key.
	body := map[string]any{
		"id":            "agent-key",
		"default_drive": "static1",
		"drives": []map[string]any{
			{"drive": "static1", "root": "/agents", "perms": []string{"list", "read", "write", "delete"}},
		},
		"global_perms": []string{"link"},
		"limits":       map[string]any{"max_upload_bytes": 1024, "requests_per_minute": 30},
	}
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	m = decodePortal(t, rec)
	data := m["data"].(map[string]any)
	plaintext, _ := data["plaintext"].(string)
	if !strings.HasPrefix(plaintext, "sk-c2a-") {
		t.Errorf("plaintext = %q", plaintext)
	}
	prompt, _ := data["onboarding_prompt"].(string)
	for _, want := range []string{"Base URL:", "API key: " + plaintext, "apikey HTTP header", "agent-instructions.txt", "openapi.json", "Drives: static1 (read/write, root /agents)", "Policy:", "c2a.py", "basic file API", "First call GET /v1/me", "do not attempt deletion without delete permission"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("onboarding prompt lacks %q:\n%s", want, prompt)
		}
	}

	for _, hidden := range []string{"/v1/selftest", "/v1/keys", "/v1/fetch", "/v1/batch", "/v1/trash", "/admin/", "/@connect", "/api/files/", "gokapi-agent-instructions", "/mcp"} {
		if strings.Contains(prompt, hidden) {
			t.Errorf("basic onboarding prompt advertises %q", hidden)
		}
	}

	// The created key authenticates through the shared key manager.
	if k, out := h.keys.Authenticate(plaintext); out != 0 || k == nil || k.ID != "agent-key" {
		t.Fatalf("manager authenticate = %v %v", k, out)
	}

	// PATCH: change the root and add an expiry-free trash setting.
	rec = doPortal(t, h.handler, http.MethodPatch, "/admin/api/keys/agent-key", cookie, map[string]any{
		"drives": []map[string]any{
			{"drive": "static1", "root": "/deeper", "perms": []string{"list", "read"}},
		},
		"trash": map[string]any{"enabled": false, "days": 3},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	m = decodePortal(t, rec)
	row := m["data"].(map[string]any)
	if row["drives"].([]any)[0].(map[string]any)["root"] != "/deeper" {
		t.Errorf("patch did not stick: %v", row["drives"])
	}
	if row["trash"].(map[string]any)["enabled"] != false {
		t.Errorf("trash patch = %v", row["trash"])
	}

	// Validation: unknown drive.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "bad", "drives": []map[string]any{{"drive": "ghost", "perms": []string{"list"}}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown drive = %d", rec.Code)
	}

	// Rotate returns a new plaintext once.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys/agent-key/rotate", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body.String())
	}
	m = decodePortal(t, rec)
	if p2, _ := m["data"].(map[string]any)["plaintext"].(string); p2 == plaintext || !strings.HasPrefix(p2, "sk-c2a-") {
		t.Errorf("rotated plaintext = %q", p2)
	}

	// Revoke -> 200; the revoked key no longer authenticates.
	rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/keys/agent-key", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if k, _ := h.keys.Get("agent-key"); !k.Revoked {
		t.Error("key not revoked in store")
	}
}

// TestSettingsAndAuditAPI covers GET/PUT settings and the audit filters.
func TestSettingsAndAuditAPI(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/settings", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings = %d", rec.Code)
	}
	m := decodePortal(t, rec)
	data := m["data"].(map[string]any)
	if _, still := data["oplist_api_base"]; still {
		t.Errorf("settings still expose oplist_api_base: %v", data)
	}

	rec = doPortal(t, h.handler, http.MethodPut, "/admin/api/settings", cookie, map[string]any{
		"public_base_url": "http://198.51.100.20:8080",
		"policy_text":     "test policy",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings = %d %s", rec.Code, rec.Body.String())
	}
	m = decodePortal(t, rec)
	if m["data"].(map[string]any)["public_base_url"] != "http://198.51.100.20:8080" {
		t.Errorf("settings after put = %v", m["data"])
	}
	if v, ok := h.st.GetSetting("public_base_url"); !ok || v != "http://198.51.100.20:8080" {
		t.Error("setting not persisted")
	}
	// Unknown fields are rejected.
	rec = doPortal(t, h.handler, http.MethodPut, "/admin/api/settings", cookie, map[string]any{"evil": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown setting = %d", rec.Code)
	}

	// Produce an audit row through the data plane (the fake upstream does
	// not implement fs/list, so the response is an upstream error — the
	// audit row is written either way).
	probe := httptest.NewRequest(http.MethodPost, "/api/storage/static1/list", strings.NewReader(`{"path":"/"}`))
	probe.Header.Set("Authorization", "Bearer "+dataPlaneKey)
	prec := httptest.NewRecorder()
	h.handler.ServeHTTP(prec, probe)
	if prec.Code == 0 {
		t.Fatal("probe produced no response")
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/audit?limit=10", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	m = decodePortal(t, rec)
	data = m["data"].(map[string]any)
	entries, _ := data["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("no audit entries")
	}
	var sawList bool
	for _, raw := range entries {
		e := raw.(map[string]any)
		for _, field := range []string{"ts", "key_id", "drive", "op", "path", "bytes", "status", "duration_ms", "error"} {
			if _, ok := e[field]; !ok {
				t.Errorf("audit entry lacks %q: %v", field, e)
			}
		}
		if e["op"] == "list" && e["drive"] == "static1" && e["key_id"] == "k1" {
			sawList = true
		}
	}
	if !sawList {
		t.Error("no list audit entry for the probe")
	}
}

// TestOverviewAndProviders covers the remaining §8.2 1a rows.
func TestOverviewAndProviders(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}
	m := decodePortal(t, rec)
	data := m["data"].(map[string]any)
	ol := data["openlist"].(map[string]any)
	if ol["reachable"] != true || ol["token_ok"] != true {
		t.Errorf("openlist = %v", ol)
	}
	counts := data["counts"].(map[string]any)
	if counts["drives"] != float64(1) || counts["keys"] != float64(1) {
		t.Errorf("counts = %v", counts)
	}
	if _, ok := data["warnings"].([]any); !ok {
		t.Error("warnings missing")
	}

	// Drives listing (§8.2 shape; step 2: enabled + storage_disabled instead
	// of customized_fields, /admin/api/providers is gone).
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives = %d", rec.Code)
	}
	m = decodePortal(t, rec)
	drives := m["data"].(map[string]any)["drives"].([]any)
	d := drives[0].(map[string]any)
	for _, field := range []string{"id", "provider", "driver", "mount_path", "storage_id", "enabled", "state", "status_text", "storage_disabled", "key_count", "created_at", "static"} {
		if _, ok := d[field]; !ok {
			t.Errorf("drive row lacks %q: %v", d, field)
		}
	}
	if _, still := d["customized_fields"]; still {
		t.Errorf("drive row still carries customized_fields: %v", d)
	}
}

// TestSPAServingRules covers the §8.1 page contract.
func TestSPAServingRules(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	// /admin/ and arbitrary SPA paths serve app.html.
	for _, p := range []string{"/admin/", "/admin/anything/else"} {
		rec := doPortal(t, h.handler, http.MethodGet, p, cookie, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d (%s)", p, rec.Code, firstBytes(rec))
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s content-type = %q", p, ct)
		}
	}
	// The old accounts page redirects into the SPA.
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/accounts.html", cookie, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/#/drives" {
		t.Errorf("accounts.html = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Assets serve with a content type.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/assets/app.css", cookie, nil)
	if rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
			t.Errorf("css content type = %q", ct)
		}
	}
	// Unknown asset -> 404.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/assets/nope.js", cookie, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d", rec.Code)
	}
	// No session -> page redirect.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/", "", nil)
	if rec.Code != http.StatusFound {
		t.Errorf("unauthenticated page = %d", rec.Code)
	}
}

func firstBytes(rec *httptest.ResponseRecorder) string {
	s := rec.Body.String()
	if len(s) > 80 {
		return s[:80]
	}
	return s
}
