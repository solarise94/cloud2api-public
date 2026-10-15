package agentapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// expectedBasicOps is the literal expected basic surface: 12 operations over
// 11 paths. Deliberately not derived from isBasicRoute or routeTable so the
// tests cannot pass by construction.
var expectedBasicOps = [][2]string{
	{"GET", "/v1/me"},
	{"GET", "/v1/drives/{drive}/list"},
	{"GET", "/v1/drives/{drive}/stat"},
	{"GET", "/v1/drives/{drive}/files"},
	{"PUT", "/v1/drives/{drive}/files"},
	{"POST", "/v1/drives/{drive}/mkdir"},
	{"POST", "/v1/drives/{drive}/delete"},
	{"POST", "/v1/drives/{drive}/move"},
	{"POST", "/v1/transfers"},
	{"GET", "/v1/jobs/{id}"},
	{"POST", "/v1/links"},
	{"GET", "/l/{token}"},
}

// collectDocOps extracts the (METHOD, path) operation set of an OpenAPI
// document body.
func collectDocOps(t *testing.T, raw []byte) map[[2]string]bool {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[[2]string]bool{}
	for p, item := range doc.Paths {
		for m := range item {
			switch m {
			case "get", "put", "post", "delete", "patch", "head", "options":
				ops[[2]string{strings.ToUpper(m), p}] = true
			}
		}
	}
	return ops
}

// TestBasicRouteSelectorFailsClosed pins isBasicRoute to the exact basic set:
// advanced, compat, diagnostic, wrong methods and unknown routes all reject.
func TestBasicRouteSelectorFailsClosed(t *testing.T) {
	for _, op := range expectedBasicOps {
		if !isBasicRoute(op[0], op[1]) {
			t.Errorf("isBasicRoute(%q, %q) = false, want true", op[0], op[1])
		}
	}
	for _, op := range [][2]string{
		// Discovery meta routes are not part of the basic operation set.
		{"GET", "/v1"}, {"GET", "/v1/drives"},
		// Default-drive aliases stay compat-only.
		{"GET", "/v1/list"}, {"GET", "/v1/stat"}, {"GET", "/v1/files"},
		{"PUT", "/v1/files"}, {"POST", "/v1/mkdir"}, {"POST", "/v1/delete"}, {"POST", "/v1/move"},
		// Advanced autonomy, browsing, jobs and delegation.
		{"POST", "/v1/selftest"}, {"GET", "/v1/tree"}, {"GET", "/v1/search"},
		{"GET", "/v1/drives/{drive}/tree"}, {"GET", "/v1/drives/{drive}/search"},
		{"POST", "/v1/fetch"}, {"POST", "/v1/batch"}, {"GET", "/v1/jobs"},
		{"POST", "/v1/jobs/{id}/cancel"}, {"GET", "/v1/keys"}, {"POST", "/v1/keys"},
		{"DELETE", "/v1/keys/{id}"}, {"GET", "/v1/activity"},
		{"GET", "/v1/trash"}, {"POST", "/v1/trash/restore"}, {"POST", "/v1/trash/purge"},
		// Gokapi compat layer.
		{"POST", "/api/files/add"}, {"GET", "/api/files/list"}, {"GET", "/h/{hotlinkId}"},
		{"GET", "/downloadFile"}, {"GET", "/d"},
		// Assets and health.
		{"GET", "/healthz"}, {"GET", "/readyz"},
		{"GET", "/custom/agent-instructions.txt"}, {"GET", "/apidocumentation/openapi.json"},
		// Wrong methods on basic paths fail closed, including casing games.
		{"POST", "/v1/me"}, {"PATCH", "/v1/me"}, {"DELETE", "/v1/me"}, {"get", "/v1/me"},
		{"GET", "/v1/links"}, {"PUT", "/v1/transfers"}, {"POST", "/l/{token}"},
		{"GET", "/v1/drives/{drive}/files/extra"},
		// Unknown/future routes and empty method.
		{"GET", "/v1/future-thing"}, {"POST", "/v1/whatever"}, {"", "/v1/me"},
	} {
		if isBasicRoute(op[0], op[1]) {
			t.Errorf("isBasicRoute(%q, %q) = true, want false", op[0], op[1])
		}
	}
}

// TestServedOpenAPIIsBasicExactSet proves /apidocumentation/openapi.json
// serves exactly the 12 basic operations over 11 paths — and nothing else.
func TestServedOpenAPIIsBasicExactSet(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/apidocumentation/openapi.json", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi = %d", rec.Code)
	}
	body := rec.Body.Bytes()

	ops := collectDocOps(t, body)
	want := map[[2]string]bool{}
	for _, op := range expectedBasicOps {
		want[op] = true
	}
	if len(ops) != len(expectedBasicOps) {
		t.Errorf("served openapi has %d operations, want %d: %v", len(ops), len(expectedBasicOps), ops)
	}
	for op := range want {
		if !ops[op] {
			t.Errorf("served openapi lacks basic op %s %s", op[0], op[1])
		}
	}
	for op := range ops {
		if !want[op] {
			t.Errorf("served openapi exposes non-basic op %s %s", op[0], op[1])
		}
	}

	// 11 distinct paths (files is one path with two methods).
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) != 11 {
		t.Errorf("served openapi has %d paths, want 11", len(doc.Paths))
	}

	// The basic profile must not mention advanced APIs or diagnostic hooks.
	lower := strings.ToLower(string(body))
	for _, banned := range []string{"selftest", "gokapi", "/v1/trash", "/v1/tree", "/v1/search", "/v1/fetch", "/v1/batch", "/v1/keys", "/api/files", "healthz"} {
		if strings.Contains(lower, banned) {
			t.Errorf("served basic openapi mentions %q", banned)
		}
	}

	// The served document is byte-identical to the basic builder output.
	if wantBytes := marshalOpenAPIDoc(buildOpenAPI("http://example.com")); !bytes.Equal(wantBytes, body) {
		t.Error("served openapi differs from buildOpenAPI output")
	}
}

// TestV1IndexListsBasicOnly proves the /v1 entry index advertises exactly the
// basic operation set (without the index itself) and a quickstart free of
// advanced routes.
func TestV1IndexListsBasicOnly(t *testing.T) {
	h := newHarness(t)
	rec, doc := h.do("GET", "/v1", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("index = %d", rec.Code)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "gokapi") {
		t.Error("index mentions gokapi")
	}

	want := map[[2]string]bool{}
	for _, op := range expectedBasicOps {
		want[op] = true
	}
	got := map[[2]string]bool{}
	endpoints, _ := doc["endpoints"].([]any)
	if len(endpoints) != len(expectedBasicOps) {
		t.Errorf("index endpoints = %d, want %d: %v", len(endpoints), len(expectedBasicOps), endpoints)
	}
	for _, e := range endpoints {
		em := e.(map[string]any)
		method, _ := em["method"].(string)
		path, _ := em["path"].(string)
		if em["summary"] == "" {
			t.Errorf("endpoint %s %s lacks summary", method, path)
		}
		got[[2]string{method, path}] = true
		if path == "/v1" {
			t.Error("index lists itself under endpoints")
		}
	}
	for op := range want {
		if !got[op] {
			t.Errorf("index lacks basic endpoint %s %s", op[0], op[1])
		}
	}
	for op := range got {
		if !want[op] {
			t.Errorf("index advertises non-basic endpoint %s %s", op[0], op[1])
		}
	}

	quickstart, _ := doc["quickstart"].([]any)
	if len(quickstart) == 0 {
		t.Fatal("quickstart missing")
	}
	joined := ""
	for _, q := range quickstart {
		joined += q.(string) + "\n"
	}
	if !strings.Contains(joined, "/v1/me") {
		t.Error("quickstart lacks /v1/me")
	}
	for _, banned := range []string{"selftest", "trash/restore", "gokapi", "/v1/fetch", "/v1/batch", "/v1/tree", "/v1/search", "/v1/keys"} {
		if strings.Contains(joined, banned) {
			t.Errorf("quickstart mentions advanced route %q", banned)
		}
	}
}

// TestV1MeDiscoveryKeepsTruthAndHidesAdvanced proves /v1/me still reports the
// real grants, permissions, limits, relay metadata, hop capability and
// selftest timestamp, while next_steps and the relay view no longer point at
// advanced or Gokapi-compat endpoints.
func TestV1MeDiscoveryKeepsTruthAndHidesAdvanced(t *testing.T) {
	h := newHarness(t)
	if err := h.st.TouchKeySelftest("chroot"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("apikey", chrootPlain)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("me = %d", rec.Code)
	}
	raw := rec.Body.String()
	for _, banned := range []string{"/v1/selftest", "/v1/trash/restore", "gokapi-agent-instructions"} {
		if strings.Contains(raw, banned) {
			t.Errorf("/v1/me advertises advanced route %q", banned)
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// Key truth: identity, default drive and the recorded selftest timestamp.
	key := doc["key"].(map[string]any)
	if key["id"] != "chroot" || key["default_drive"] != "d1" {
		t.Errorf("key = %v", key)
	}
	if key["last_selftest_at"] == nil || key["last_selftest_at"] == "" {
		t.Errorf("last_selftest_at lost: %v", key)
	}
	// Grants and per-drive permissions survive verbatim.
	views, _ := doc["drives"].([]any)
	if len(views) != 1 {
		t.Fatalf("drives = %v", views)
	}
	d0 := views[0].(map[string]any)
	if d0["id"] != "d1" || d0["root"] != "/agents/x" || d0["health"] != "healthy" || d0["default"] != true {
		t.Errorf("drive view = %v", d0)
	}
	perms := map[string]bool{}
	for _, p := range d0["perms"].([]any) {
		perms[p.(string)] = true
	}
	for _, want := range []string{"list", "read", "write", "delete", "link"} {
		if !perms[want] {
			t.Errorf("drive perms lack %q: %v", want, d0["perms"])
		}
	}
	gp := map[string]bool{}
	for _, p := range doc["global_perms"].([]any) {
		gp[p.(string)] = true
	}
	if !gp["link"] {
		t.Errorf("global_perms = %v", doc["global_perms"])
	}
	// Limits keep all three keys (null when unset).
	limits := doc["limits"].(map[string]any)
	for _, k := range []string{"max_upload_bytes", "requests_per_minute", "concurrent_jobs"} {
		if _, ok := limits[k]; !ok {
			t.Errorf("limits lack %q: %v", k, limits)
		}
	}
	// Relay metadata: usage and settings, no Gokapi onboarding URL.
	relay := doc["relay"].(map[string]any)
	for _, k := range []string{"enabled", "drive", "quota_bytes", "max_age_days", "used_bytes", "files"} {
		if _, ok := relay[k]; !ok {
			t.Errorf("relay lacks %q: %v", k, relay)
		}
	}
	if relay["enabled"] != false {
		t.Errorf("relay enabled = %v, want false for a key without relay grant", relay["enabled"])
	}
	if _, ok := relay["instructions"]; ok {
		t.Error("relay still advertises the Gokapi onboarding URL")
	}
	// GoAPI peers require the hop capability.
	if doc["relay_hop_limit"] != float64(hopLimit) {
		t.Errorf("relay_hop_limit = %v, want %d", doc["relay_hop_limit"], hopLimit)
	}
	if _, ok := doc["policy_text"].(string); !ok {
		t.Error("policy_text missing")
	}
	steps, _ := doc["next_steps"].([]any)
	if len(steps) == 0 {
		t.Fatal("next_steps empty")
	}
}

// TestAdvancedRoutesStillDispatch proves the layering is documentation-only:
// every route hidden from the basic surface still answers under the same
// authentication, unchanged.
func TestAdvancedRoutesStillDispatch(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/docs/a.txt", "x")
	for _, probe := range []struct{ method, target string }{
		{"GET", "/v1/drives"},
		{"GET", "/v1/list?path=/"},
		{"GET", "/v1/tree?path=/"},
		{"GET", "/v1/search?q=a&path=/"},
		{"GET", "/v1/drives/d1/tree?path=/"},
		{"GET", "/v1/drives/d1/search?q=a"},
		{"GET", "/v1/jobs"},
		{"GET", "/v1/trash"},
		{"GET", "/v1/activity?limit=5"},
		{"GET", "/api/info/version"},
		{"POST", "/v1/selftest"},
	} {
		rec, _ := h.do(probe.method, probe.target, chrootPlain, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200 (advanced routes must keep dispatching)", probe.method, probe.target, rec.Code)
		}
	}
	// The per-drive tree/search routes behave like their default-drive aliases.
	rec, doc := h.do("GET", "/v1/drives/d1/search?q=a", chrootPlain, nil, nil)
	if rec.Code != 200 || doc["engine"] != "walk" {
		t.Fatalf("per-drive search = %d %v", rec.Code, doc)
	}
}

// TestFullOpenAPIDocumentation keeps docs/openapi.full.json byte-identical to
// buildFullOpenAPI("https://cloud2api.example"). Regenerate the fixture with:
//
//	C2A_UPDATE_API_DOCS=1 go test ./internal/agentapi -run '^TestFullOpenAPIDocumentation$'
//
// The full protocol ships only as this project-docs artifact; it is never
// served at a public URL.
func TestFullOpenAPIDocumentation(t *testing.T) {
	fixture := filepath.Join("..", "..", "docs", "openapi.full.json")
	raw := marshalOpenAPIDoc(buildFullOpenAPI("https://cloud2api.example"))

	if os.Getenv("C2A_UPDATE_API_DOCS") == "1" {
		if err := os.WriteFile(fixture, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", fixture)
	}

	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with C2A_UPDATE_API_DOCS=1 go test ./internal/agentapi -run '^TestFullOpenAPIDocumentation$')", fixture, err)
	}
	if !bytes.Equal(want, raw) {
		t.Fatalf("%s is stale; regenerate with C2A_UPDATE_API_DOCS=1 go test ./internal/agentapi -run '^TestFullOpenAPIDocumentation$'", fixture)
	}

	// Coverage of the checked-in full document: 55 paths / 58 operations
	// (53/56 before per-drive tree/search joined the table).
	ops := collectDocOps(t, want)
	if len(ops) != 58 {
		t.Errorf("full doc has %d operations, want 58", len(ops))
	}
	var doc struct {
		Paths   map[string]json.RawMessage `json:"paths"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if err := json.Unmarshal(want, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) != 55 {
		t.Errorf("full doc has %d paths, want 55", len(doc.Paths))
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://cloud2api.example" {
		t.Errorf("full doc servers = %v", doc.Servers)
	}
	if !strings.Contains(doc.Info.Title, "full protocol") {
		t.Errorf("full doc title = %q", doc.Info.Title)
	}
	// Every routeTable row is covered by the fixture.
	for _, r := range routeTable {
		if !ops[[2]string{r.Method, r.Path}] {
			t.Errorf("full doc lacks %s %s", r.Method, r.Path)
		}
	}
	// The basic surface is a subset of the full document.
	for _, op := range expectedBasicOps {
		if !ops[op] {
			t.Errorf("full doc lacks basic op %s %s", op[0], op[1])
		}
	}
	// No runtime credentials or private addresses in the shipped artifact.
	lower := strings.ToLower(string(want))
	for _, banned := range []string{"sk-", "apikey=", "127.0.0.1", "localhost", "0.0.0.0", "192.168.", "10.0."} {
		if strings.Contains(lower, banned) {
			t.Errorf("full doc contains %q", banned)
		}
	}
}
