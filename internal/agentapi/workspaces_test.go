package agentapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/store"
)

// ---- workspace harness helpers (real HTTP + cookiejar) ----

type wsFixture struct {
	t      *testing.T
	h      *harness
	base   string // workspace handler server base URL
	client *http.Client
}

// newWSFixture serves only the workspace handler on a real HTTP server with a
// real cookie jar, the way the browser talks to it.
func newWSFixture(t *testing.T) *wsFixture {
	h := newHarness(t)
	ts := httptest.NewServer(h.api.WorkspaceHandler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &wsFixture{t: t, h: h, base: ts.URL, client: &http.Client{Jar: jar}}
}

// do performs one workspace API request. hdr values override defaults;
// pass hdr["X-C2A-Workspace"] = "" to drop the header. A nil client skips
// the cookie jar (bare request).
func (w *wsFixture) do(method, path string, body []byte, hdr map[string]string) (int, map[string]any, *http.Response) {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, w.base+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, ok := hdr["X-C2A-Workspace"]; !ok {
		req.Header.Set("X-C2A-Workspace", "1")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return resp.StatusCode, doc, resp
}

func (w *wsFixture) login(apiKey, workspaceID string, hdr map[string]string) (int, map[string]any, *http.Response) {
	w.t.Helper()
	payload := map[string]string{"api_key": apiKey}
	if workspaceID != "" {
		payload["workspace_id"] = workspaceID
	}
	b, _ := json.Marshal(payload)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		h[k] = v
	}
	return w.do("POST", "/files/api/login", b, h)
}

func errCodeOf(doc map[string]any) string {
	if doc == nil {
		return ""
	}
	if e, ok := doc["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func (w *wsFixture) mustLogin(apiKey string) string {
	w.t.Helper()
	code, doc, _ := w.login(apiKey, "", nil)
	if code != http.StatusOK {
		w.t.Fatalf("login = %d %v", code, doc)
	}
	id, _ := doc["workspace_id"].(string)
	if id == "" {
		w.t.Fatalf("no workspace_id: %v", doc)
	}
	return id
}

// browserCookie returns the raw c2a_files cookie value the jar currently
// holds (for hash-only storage assertions). The URL must carry the cookie's
// /files path or the jar filters it out.
func (w *wsFixture) browserCookie() string {
	w.t.Helper()
	u, _ := url.Parse(w.base + "/files/")
	for _, c := range w.client.Jar.Cookies(u) {
		if c.Name == filesCookieName {
			return c.Value
		}
	}
	return ""
}

// ---- login / list / v1 flow ----

func TestWorkspaceLoginListAndV1Flow(t *testing.T) {
	w := newWSFixture(t)
	h := w.h

	code, doc, resp := w.login(chrootPlain, "", nil)
	if code != http.StatusOK {
		t.Fatalf("login = %d %v", code, doc)
	}
	wsID, _ := doc["workspace_id"].(string)
	wsURL, _ := doc["url"].(string)
	if wsID == "" || wsURL != "/files/w/"+wsID+"/" {
		t.Fatalf("login response = %v", doc)
	}
	expStr, _ := doc["expires_at"].(string)
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("expires_at = %v", doc["expires_at"])
	}
	if d := time.Until(exp); d < 29*24*time.Hour || d > 30*24*time.Hour {
		t.Fatalf("expires_at not ~30d: %v", d)
	}

	// Cookie attributes: Path=/files, HttpOnly, SameSite=Strict, 30d, plain
	// HTTP keeps Secure off.
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == filesCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no c2a_files cookie set")
	}
	if cookie.Path != "/files" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes = %+v", cookie)
	}
	if cookie.MaxAge != int(workspaceTTL/time.Second) || cookie.Secure {
		t.Fatalf("cookie maxage/secure = %+v", cookie)
	}
	if len(cookie.Value) != 43 {
		t.Fatalf("token not 256-bit base64url: %q", cookie.Value)
	}

	// Only the SHA-256 of the token is stored, bound to the key id.
	hash := sha256.Sum256([]byte(cookie.Value))
	rows, err := h.st.ListWebSessions(hex.EncodeToString(hash[:]), store.SessionKindFiles)
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored rows = %d %v", len(rows), err)
	}
	if rows[0].TokenHash == cookie.Value || len(rows[0].TokenHash) != 64 {
		t.Fatal("raw token material reached the database")
	}
	if rows[0].SubjectID != "chroot" || rows[0].WorkspaceID != wsID {
		t.Fatalf("row binding = %+v", rows[0])
	}

	// Listing shows exactly this entry.
	code, doc, _ = w.do("GET", "/files/api/workspaces", nil, nil)
	if code != 200 {
		t.Fatalf("workspaces = %d %v", code, doc)
	}
	list := doc["workspaces"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != wsID {
		t.Fatalf("workspaces = %v", doc)
	}

	// The v1 adapter serves me, listing, upload (raw + zero byte), download,
	// mkdir, links and download-ticket — same protocol as /v1.
	code, doc, _ = w.do("GET", "/files/w/"+wsID+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("ws me = %d %v", code, doc)
	}
	if key := doc["key"].(map[string]any); key["id"] != "chroot" {
		t.Fatalf("ws me key = %v", doc)
	}

	code, doc, _ = w.do("GET", "/files/w/"+wsID+"/api/v1/drives", nil, nil)
	if code != 200 || len(doc["drives"].([]any)) != 1 {
		t.Fatalf("ws drives = %d %v", code, doc)
	}

	content := []byte("workspace-upload-payload")
	sha := fmt.Sprintf("%x", sha256.Sum256(content))
	ct := map[string]string{"Content-Type": "application/octet-stream"}
	code, doc, _ = w.do("PUT", "/files/w/"+wsID+"/api/v1/files?path=/up/raw.bin", content, ct)
	if code != 200 || doc["sha256"] != sha || doc["size"] != float64(len(content)) {
		t.Fatalf("raw upload = %d %v", code, doc)
	}
	// Zero-byte upload is legitimate via multipart (raw bodies require a
	// positive Content-Length, as on /v1).
	var mbuf bytes.Buffer
	mw := multipart.NewWriter(&mbuf)
	if _, err := mw.CreateFormFile("file", "empty.bin"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	code, doc, _ = w.do("PUT", "/files/w/"+wsID+"/api/v1/files?path=/up/empty.bin&on_conflict=overwrite",
		mbuf.Bytes(), map[string]string{"Content-Type": mw.FormDataContentType()})
	if code != 200 || doc["size"] != float64(0) {
		t.Fatalf("zero-byte upload = %d %v", code, doc)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsID+"/api/v1/files?path=/up/raw.bin", nil, nil)
	if code != 200 {
		t.Fatalf("ws download = %d", code)
	}
	code, doc, _ = w.do("POST", "/files/w/"+wsID+"/api/v1/mkdir", []byte(`{"path":"/ws-dir"}`), jsonCT())
	if code != 200 || doc["created"] != true {
		t.Fatalf("ws mkdir = %d %v", code, doc)
	}
	code, doc, _ = w.do("POST", "/files/w/"+wsID+"/api/v1/links", []byte(`{"path":"/up/raw.bin","expires_in":600}`), jsonCT())
	if code != 200 {
		t.Fatalf("ws links = %d %v", code, doc)
	}
	if u, _ := doc["url"].(string); !strings.Contains(u, "/l/") {
		t.Fatalf("ws link url = %v", doc)
	}
	code, doc, _ = w.do("POST", "/files/w/"+wsID+"/api/v1/drives/d1/download-ticket", []byte(`{"path":"/up/raw.bin"}`), jsonCT())
	if code != 200 {
		t.Fatalf("ws download-ticket = %d %v", code, doc)
	}
	if u, _ := doc["url"].(string); !strings.HasPrefix(u, "/download/") {
		t.Fatalf("ticket url = %v", doc)
	}
}

func TestWorkspaceRestartRecovery(t *testing.T) {
	w := newWSFixture(t)
	wsID := w.mustLogin(chrootPlain)

	// "Restart": a fresh server process over the same store; the browser
	// keeps its cookie and recovers the workspace.
	ts2 := httptest.NewServer(w.h.api.WorkspaceHandler())
	defer ts2.Close()
	w.base = ts2.URL

	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	if code != 200 {
		t.Fatalf("workspaces after restart = %d %v", code, doc)
	}
	list := doc["workspaces"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != wsID {
		t.Fatalf("workspaces after restart = %v", doc)
	}
	code, doc, _ = w.do("GET", "/files/w/"+wsID+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("me after restart = %d %v", code, doc)
	}
}

// ---- login validation & generic failures ----

func TestWorkspaceLoginValidation(t *testing.T) {
	w := newWSFixture(t)
	other := w.mustLogin(legacyPlain) // workspace owned by the legacy key

	// Malformed bodies first: they answer 400 without feeding the login
	// failure limiter.
	code, doc, _ := w.do("POST", "/files/api/login", []byte("not-json"), map[string]string{"Content-Type": "application/json"})
	if code != 400 || errCodeOf(doc) != CodeInvalidRequest {
		t.Fatalf("bad json = %d %v", code, doc)
	}
	code, doc, _ = w.do("POST", "/files/api/login", []byte(`{"api_key":"  "}`), map[string]string{"Content-Type": "application/json"})
	if code != 400 {
		t.Fatalf("empty key = %d %v", code, doc)
	}
	big := `{"api_key":"` + strings.Repeat("x", wsLoginBodyLimit+10) + `"}`
	code, _, _ = w.do("POST", "/files/api/login", []byte(big), map[string]string{"Content-Type": "application/json"})
	if code != 400 {
		t.Fatalf("oversized body = %d", code)
	}

	cases := []struct {
		name    string
		apiKey  string
		wsID    string
		want    int
		wantErr string
	}{
		{"wrong secret", "sk-c2a-ab12cd34-wrongwrongwrongwrong", "", 401, CodeUnauthorized},
		{"unknown key id", "sk-c2a-zz00zz99-nope", "", 401, CodeUnauthorized},
		{"other key's workspace", chrootPlain, other, 403, CodeForbidden},
		{"unknown workspace", legacyPlain, "AAAAAAAAAAAAAAAAAAAAAA", 403, CodeForbidden},
	}
	for _, tc := range cases {
		code, doc, _ := w.login(tc.apiKey, tc.wsID, nil)
		if code != tc.want || errCodeOf(doc) != tc.wantErr {
			t.Errorf("%s = %d %v, want %d %s", tc.name, code, doc, tc.want, tc.wantErr)
		}
	}
	// The mismatch and unknown-entry cases share one generic message (no
	// entry-existence oracle). Fresh fixture: the check must not be fed
	// into this fixture's already-warmed login limiter.
	w2 := newWSFixture(t)
	_, d1, _ := w2.login(chrootPlain, other, nil)
	_, d2, _ := w2.login(legacyPlain, "AAAAAAAAAAAAAAAAAAAAAA", nil)
	if d1["error"] == nil || fmt.Sprint(d1["error"]) != fmt.Sprint(d2["error"]) {
		t.Fatalf("mismatch vs unknown leak existence: %v vs %v", d1, d2)
	}

	// Revoked key.
	h := w.h
	if err := h.keys.Revoke("chroot"); err != nil {
		t.Fatal(err)
	}
	code, doc, _ = w.login(chrootPlain, "", nil)
	if code != 401 || errCodeOf(doc) != CodeUnauthorized {
		t.Fatalf("revoked login = %d %v", code, doc)
	}

	// Audit rows never contain the presented key material.
	entries, _, err := h.st.QueryAudit(store.AuditFilter{Op: "ws_login"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Error, chrootPlain) || strings.Contains(e.Error, legacyPlain) || e.KeyID == chrootPlain && e.Status != 200 {
			t.Fatalf("login audit leaks credentials: %+v", e)
		}
	}
}

func TestWorkspaceLoginRateLimit(t *testing.T) {
	w := newWSFixture(t)
	for i := 0; i < wsLoginMaxFailures; i++ {
		code, _, _ := w.login("sk-c2a-ab12cd34-badbadbadbadbad0", "", nil)
		if code != 401 {
			t.Fatalf("failure %d = %d", i, code)
		}
	}
	code, doc, resp := w.login(chrootPlain, "", nil)
	if code != http.StatusTooManyRequests || errCodeOf(doc) != CodeRateLimited {
		t.Fatalf("after %d failures = %d %v", wsLoginMaxFailures, code, doc)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After on limited login")
	}
}

// ---- cookie hygiene: fixation, malformed values, admin-cookie isolation ----

// wsLoginRaw runs one login through the handler with a hand-built Cookie
// header (no jar) and returns the recorder plus the minted cookie value.
func wsLoginRaw(h *harness, apiKey, cookie string) (*httptest.ResponseRecorder, string) {
	t := h.t
	req, err := http.NewRequest("POST", "http://ws.test/files/api/login",
		strings.NewReader(`{"api_key":"`+apiKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-C2A-Workspace", "1")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.api.WorkspaceHandler().ServeHTTP(rec, req)
	var minted string
	for _, c := range rec.Result().Cookies() {
		if c.Name == filesCookieName {
			minted = c.Value
		}
	}
	return rec, minted
}

func rowsForToken(h *harness, token string) []store.WebSession {
	h.t.Helper()
	sum := sha256.Sum256([]byte(token))
	rows, err := h.st.ListWebSessions(hex.EncodeToString(sum[:]), store.SessionKindFiles)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func TestWorkspaceCookieHygiene(t *testing.T) {
	h := newHarness(t)

	// A client-chosen token (even a perfectly well-formed one) is never
	// adopted: the server mints a fresh random token instead (anti-fixation).
	chosen := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	rec, minted := wsLoginRaw(h, chrootPlain, filesCookieName+"="+chosen)
	if rec.Code != 200 {
		t.Fatalf("login with chosen token = %d %s", rec.Code, rec.Body.String())
	}
	if minted == "" || minted == chosen {
		t.Fatalf("client-chosen session id adopted: %q", minted)
	}
	chosenSum := sha256.Sum256([]byte(chosen))
	if rows, _ := h.st.ListWebSessions(hex.EncodeToString(chosenSum[:]), store.SessionKindFiles); len(rows) != 0 {
		t.Fatal("rows created under the client-chosen token")
	}
	if rows := rowsForToken(h, minted); len(rows) != 1 {
		t.Fatalf("minted token rows = %d", len(rows))
	}

	// A malformed cookie is replaced the same way.
	_, minted2 := wsLoginRaw(h, chrootPlain, filesCookieName+"=short-and-broken")
	if minted2 == "" || !validSessionToken(minted2) || minted2 == "short-and-broken" {
		t.Fatalf("malformed cookie not replaced: %q", minted2)
	}

	// An admin-kind cookie is ignored (three-way isolation).
	rec, minted3 := wsLoginRaw(h, chrootPlain, "session=admintokenvalue")
	if rec.Code != 200 || minted3 == "" || minted3 == "admintokenvalue" {
		t.Fatalf("admin cookie login = %d %q", rec.Code, minted3)
	}

	// A known (server-minted) token is reused: a second login under the same
	// token adds a member and never touches the first one's fixed expiry.
	rec2, reused := wsLoginRaw(h, chrootPlain, filesCookieName+"="+minted)
	if rec2.Code != 200 || reused != minted {
		t.Fatalf("known token not reused: %q (rec %d)", reused, rec2.Code)
	}
	before := rowsForToken(h, minted)
	wsB := func() string {
		req, _ := http.NewRequest("POST", "http://ws.test/files/api/login",
			strings.NewReader(`{"api_key":"`+legacyPlain+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-C2A-Workspace", "1")
		req.Header.Set("Cookie", filesCookieName+"="+minted)
		rec := httptest.NewRecorder()
		h.api.WorkspaceHandler().ServeHTTP(rec, req)
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		id, _ := doc["workspace_id"].(string)
		if rec.Code != 200 || id == "" {
			t.Fatalf("second member login = %d %v", rec.Code, doc)
		}
		return id
	}()
	after := rowsForToken(h, minted)
	if len(after) != 2 {
		t.Fatalf("members after second login = %d (%v)", len(after), after)
	}
	for _, a := range after {
		for _, b := range before {
			if a.WorkspaceID == b.WorkspaceID && !a.ExpiresAt.Equal(b.ExpiresAt) {
				t.Fatalf("second login refreshed another member: %v -> %v", b.ExpiresAt, a.ExpiresAt)
			}
		}
	}
	if wsB == "" {
		t.Fatal("no second workspace")
	}
}

// ---- multi-key isolation ----

func TestWorkspaceMultiKeyIsolation(t *testing.T) {
	w := newWSFixture(t)
	wsA := w.mustLogin(chrootPlain)
	wsB := w.mustLogin(legacyPlain)

	// Same browser: both entries listed, both usable.
	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	ids := map[string]bool{}
	for _, it := range doc["workspaces"].([]any) {
		ids[it.(map[string]any)["id"].(string)] = true
	}
	if !ids[wsA] || !ids[wsB] || len(ids) != 2 {
		t.Fatalf("workspaces = %v (want %s and %s)", ids, wsA, wsB)
	}

	// Cross-entry access with the same cookie is refused: the URL id alone
	// grants nothing beyond the browser's own members... but both ARE this
	// browser's members, so instead verify another browser cannot use them.
	jar, _ := cookiejar.New(nil)
	anon := &wsFixture{t: t, h: w.h, base: w.base, client: &http.Client{Jar: jar}}
	code, _, _ = anon.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("anonymous use of foreign entry URL = %d", code)
	}
	code, doc, _ = anon.do("GET", "/files/api/workspaces", nil, nil)
	if len(doc["workspaces"].([]any)) != 0 {
		t.Fatalf("anon listing = %v", doc)
	}

	// Second browser logs in the SAME key: same stable entry id, separate
	// session row; logging out one browser keeps the other.
	jar2, _ := cookiejar.New(nil)
	b2 := &wsFixture{t: t, h: w.h, base: w.base, client: &http.Client{Jar: jar2}}
	wsA2 := b2.mustLogin(chrootPlain)
	if wsA2 != wsA {
		t.Fatalf("entry not stable per key: %s vs %s", wsA2, wsA)
	}
	code, _, _ = b2.do("POST", "/files/w/"+wsA+"/api/logout", nil, nil)
	if code != 200 {
		t.Fatalf("b2 logout = %d", code)
	}
	code, _, _ = b2.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("b2 after logout = %d", code)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("browser 1 lost its session after another browser logged out: %d", code)
	}

	// A new login in one browser must not refresh the other member's fixed
	// server-side expiry.
	hash := sha256.Sum256([]byte(w.browserCookie()))
	before, _ := w.h.st.ListWebSessions(hex.EncodeToString(hash[:]), store.SessionKindFiles)
	w.mustLogin(chrootPlain) // explicit re-login of wsA in browser 1
	after, _ := w.h.st.ListWebSessions(hex.EncodeToString(hash[:]), store.SessionKindFiles)
	for _, a := range after {
		for _, b := range before {
			if a.WorkspaceID == b.WorkspaceID && a.WorkspaceID != wsA && !a.ExpiresAt.Equal(b.ExpiresAt) {
				t.Fatalf("re-login moved another member's expiry: %v -> %v", b.ExpiresAt, a.ExpiresAt)
			}
		}
	}
}

// ---- 16 workspace cap per browser ----

func TestWorkspaceMemberCap(t *testing.T) {
	w := newWSFixture(t)
	h := w.h

	// 15 extra keys beyond the harness's three.
	plaintexts := []string{chrootPlain, legacyPlain, adminPlain}
	for i := 0; i < 15; i++ {
		plain := fmt.Sprintf("sk-c2a-cap%02dx%02d-secret%016d", i, i, i)
		plaintexts = append(plaintexts, plain)
		if err := h.st.InsertKey(store.APIKey{
			ID:        fmt.Sprintf("cap%02d", i),
			Hash:      hashOf(plain),
			CreatedAt: time.Now().UTC(),
			Drives:    []store.KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}

	for i, plain := range plaintexts[:maxBrowserWorkspaces] {
		code, doc, _ := w.login(plain, "", nil)
		if code != 200 {
			t.Fatalf("login %d = %d %v", i, code, doc)
		}
	}
	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	if got := len(doc["workspaces"].([]any)); got != maxBrowserWorkspaces {
		t.Fatalf("listed = %d, want %d", got, maxBrowserWorkspaces)
	}

	// The 17th login is refused without touching existing members.
	plain17 := "sk-c2a-cap17x17-secret0000000000000017"
	if err := h.st.InsertKey(store.APIKey{
		ID: "cap17", Hash: hashOf(plain17), CreatedAt: time.Now().UTC(),
		Drives: []store.KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	code, doc, _ = w.login(plain17, "", nil)
	if code != http.StatusForbidden || errCodeOf(doc) != CodeForbidden {
		t.Fatalf("17th login = %d %v", code, doc)
	}
	code, doc, _ = w.do("GET", "/files/api/workspaces", nil, nil)
	if got := len(doc["workspaces"].([]any)); got != maxBrowserWorkspaces {
		t.Fatalf("after rejection listed = %d", got)
	}

	// Re-login of an existing member stays allowed at the cap.
	code, doc, _ = w.login(chrootPlain, "", nil)
	if code != 200 {
		t.Fatalf("re-login at cap = %d %v", code, doc)
	}
}

// ---- key lifecycle invalidation ----

func TestWorkspaceKeyLifecycle(t *testing.T) {
	w := newWSFixture(t)
	h := w.h
	wsA := w.mustLogin(chrootPlain)
	wsB := w.mustLogin(legacyPlain)

	// Rotation invalidates exactly the rotated key's member.
	_, newPlain, err := h.keys.Rotate("chroot")
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := w.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("rotated member = %d", code)
	}
	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	list := doc["workspaces"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != wsB {
		t.Fatalf("listing after rotation = %v", doc)
	}
	// The old plaintext can no longer log in; the new one re-binds the same
	// stable entry.
	code, _, _ = w.login(chrootPlain, wsA, nil)
	if code != 401 {
		t.Fatalf("old plaintext login = %d", code)
	}
	code, doc, _ = w.login(newPlain, wsA, nil)
	if code != 200 || doc["workspace_id"] != wsA {
		t.Fatalf("rotated re-login = %d %v", code, doc)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("member after rotation re-login = %d", code)
	}

	// Revocation invalidates the member and does not touch the other one.
	if err := h.keys.Revoke("legacy"); err != nil {
		t.Fatal(err)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsB+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("revoked member = %d", code)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("sibling member after revoke = %d", code)
	}

	// Key expiry invalidates the member.
	plainExp := "sk-c2a-exp0exp01-expsecret0000000000exp01"
	if err := h.st.InsertKey(store.APIKey{
		ID: "expiring", Hash: hashOf(plainExp), CreatedAt: time.Now().UTC(),
		ExpiresAt: func() *time.Time { t := time.Now().UTC().Add(time.Hour); return &t }(),
		Drives:    []store.KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	wsE := w.mustLogin(plainExp)
	past := time.Now().UTC().Add(-time.Second)
	if _, err := h.keys.Patch("expiring", func(k *store.APIKey) error { k.ExpiresAt = &past; return nil }); err != nil {
		t.Fatal(err)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsE+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("expired member = %d", code)
	}
	code, _, _ = w.login(plainExp, "", nil)
	if code != 401 {
		t.Fatalf("expired key login = %d", code)
	}

	// Key deletion invalidates the member (and the entry lookup).
	wsC := w.mustLogin(adminPlain)
	if err := h.keys.Revoke("admin"); err != nil { // revoke
		t.Fatal(err)
	}
	if err := h.keys.Revoke("admin"); err != nil { // second revoke = hard delete
		t.Fatal(err)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsC+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("deleted-key member = %d", code)
	}
}

func TestWorkspaceSessionExpiryAndCleanup(t *testing.T) {
	w := newWSFixture(t)
	h := w.h
	wsID := w.mustLogin(chrootPlain)

	// Force the member's fixed expiry into the past: the session is dead
	// even though the cookie is still sent.
	token := w.browserCookie()
	hash := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte(token)); return s[:] }())
	rows, _ := h.st.ListWebSessions(hash, store.SessionKindFiles)
	expired := rows[0]
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := h.st.PutWebSession(expired); err != nil {
		t.Fatal(err)
	}
	code, _, _ := w.do("GET", "/files/w/"+wsID+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("expired session = %d", code)
	}
	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	if len(doc["workspaces"].([]any)) != 0 {
		t.Fatalf("expired member still listed: %v", doc)
	}
	// Global cleanup removes expired rows of any browser.
	if err := h.st.DeleteExpiredWebSessions(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if rows, _ := h.st.ListWebSessions(hash, store.SessionKindFiles); len(rows) != 0 {
		t.Fatal("expired row survived DeleteExpiredWebSessions")
	}
}

// ---- request security: header, origin, fetch-site ----

func TestWorkspaceSecurityHeaders(t *testing.T) {
	w := newWSFixture(t)
	wsID := w.mustLogin(chrootPlain)
	base := strings.TrimPrefix(w.base, "http://")

	cases := []struct {
		name    string
		method  string
		path    string
		body    []byte
		hdr     map[string]string
		want    int
		wantErr string
	}{
		{"missing workspace header", "GET", "/files/api/workspaces", nil,
			map[string]string{"X-C2A-Workspace": ""}, 403, CodeForbidden},
		{"wrong workspace header", "GET", "/files/api/workspaces", nil,
			map[string]string{"X-C2A-Workspace": "2"}, 403, CodeForbidden},
		{"cross origin", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://evil.example"}, 403, CodeForbidden},
		{"null origin", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "null"}, 403, CodeForbidden},
		{"origin with path", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://" + base + "/app"}, 403, CodeForbidden},
		{"origin with userinfo", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://user:pw@" + base}, 403, CodeForbidden},
		{"origin with query", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://" + base + "?x=1"}, 403, CodeForbidden},
		{"origin with fragment", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://" + base + "#f"}, 403, CodeForbidden},
		{"https origin over http", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "https://" + base}, 403, CodeForbidden},
		{"wrong port origin", "GET", "/files/api/workspaces", nil,
			map[string]string{"Origin": "http://" + base + ":9999"}, 403, CodeForbidden},
		{"sec-fetch cross-site", "GET", "/files/api/workspaces", nil,
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403, CodeForbidden},
		{"sec-fetch same-site", "GET", "/files/api/workspaces", nil,
			map[string]string{"Sec-Fetch-Site": "same-site"}, 403, CodeForbidden},
		{"missing header on v1 adapter", "GET", "/files/w/" + wsID + "/api/v1/me", nil,
			map[string]string{"X-C2A-Workspace": ""}, 403, CodeForbidden},
		{"cross origin on v1 adapter", "GET", "/files/w/" + wsID + "/api/v1/me", nil,
			map[string]string{"Origin": "http://evil.example"}, 403, CodeForbidden},
		{"cross origin on logout", "POST", "/files/w/" + wsID + "/api/logout", nil,
			map[string]string{"Origin": "http://evil.example"}, 403, CodeForbidden},
	}
	for _, tc := range cases {
		code, doc, _ := w.do(tc.method, tc.path, tc.body, tc.hdr)
		if code != tc.want || (tc.wantErr != "" && errCodeOf(doc) != tc.wantErr) {
			t.Errorf("%s = %d %v, want %d %s", tc.name, code, doc, tc.want, tc.wantErr)
		}
	}

	// Accepted same-origin variants.
	okays := []struct {
		name string
		hdr  map[string]string
	}{
		{"matching origin", map[string]string{"Origin": "http://" + base}},
		{"matching origin default port", map[string]string{"Origin": "http://" + base + ":80"}},
		{"sec-fetch none no origin", map[string]string{"Sec-Fetch-Site": "none"}},
		{"sec-fetch same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"no origin no fetch-site", map[string]string{}},
	}
	for _, tc := range okays {
		code, _, _ := w.do("GET", "/files/api/workspaces", nil, tc.hdr)
		if code != 200 {
			t.Errorf("%s = %d", tc.name, code)
		}
	}

	// Repeated headers need a raw request (Header.Set collapses duplicates).
	req, _ := http.NewRequest("GET", w.base+"/files/api/workspaces", nil)
	req.Header.Set("X-C2A-Workspace", "1")
	req.Header.Add("Origin", "http://"+base)
	req.Header.Add("Origin", "http://"+base)
	rec := httptest.NewRecorder()
	w.h.api.WorkspaceHandler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("repeated origin = %d %s", rec.Code, rec.Body.String())
	}

	// Rejections carry the no-store security header.
	code, _, resp := w.do("GET", "/files/api/workspaces", nil, map[string]string{"Origin": "http://evil.example"})
	if code == 403 && resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("403 without no-store")
	}
}

// ---- trusted HTTPS configuration ----

func TestWorkspaceSecureFromPublicBaseURL(t *testing.T) {
	w := newWSFixture(t)
	h := w.h
	if err := h.st.SetSetting("public_base_url", "https://files.example.com"); err != nil {
		t.Fatal(err)
	}

	// A trusted https public_base_url marks the cookie Secure and accepts
	// the matching https origin even though this connection is plain HTTP.
	req, _ := http.NewRequest("POST", "https://files.example.com/files/api/login",
		strings.NewReader(`{"api_key":"`+chrootPlain+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-C2A-Workspace", "1")
	req.Header.Set("Origin", "https://files.example.com")
	rec := httptest.NewRecorder()
	h.api.WorkspaceHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login over trusted https origin = %d %s", rec.Code, rec.Body.String())
	}
	var secure bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == filesCookieName {
			secure = c.Secure
		}
	}
	if !secure {
		t.Fatal("cookie not Secure under trusted https public_base_url")
	}

	// X-Forwarded-Proto is never trusted for either decision.
	w2 := newWSFixture(t)
	code, doc, resp := w2.do("POST", "/files/api/login", []byte(`{"api_key":"`+chrootPlain+`"}`),
		map[string]string{"Content-Type": "application/json", "X-Forwarded-Proto": "https"})
	if code != 200 {
		t.Fatalf("xfp login = %d %v", code, doc)
	}
	for _, c := range resp.Cookies() {
		if c.Name == filesCookieName && c.Secure {
			t.Fatal("X-Forwarded-Proto produced a Secure cookie")
		}
	}
	code, _, _ = w2.do("GET", "/files/api/workspaces", nil,
		map[string]string{"Origin": "https://" + strings.TrimPrefix(w2.base, "http://"), "X-Forwarded-Proto": "https"})
	if code != 403 {
		t.Fatalf("https origin accepted via X-Forwarded-Proto: %d", code)
	}
}

// ---- adapter surface whitelist ----

func TestWorkspaceAdapterWhitelist(t *testing.T) {
	w := newWSFixture(t)
	wsID := w.mustLogin(chrootPlain)
	pre := "/files/w/" + wsID + "/api/v1"

	// Seed the chroot root directory plus a file so stat/tree resolve (their
	// ordinary path_not_found 404s would otherwise be indistinguishable from
	// the adapter's surface 404).
	w.h.fake.mu.Lock()
	w.h.fake.entries["/m1/agents/x"] = &fakeEntry{name: "x", isDir: true, modified: "2026-10-12T00:00:00Z"}
	w.h.fake.entries["/m1/agents/x/seed.txt"] = &fakeEntry{name: "seed.txt", content: []byte("s"), modified: "2026-10-12T00:00:00Z"}
	w.h.fake.mu.Unlock()

	// Non-UI endpoints answer 404 even with a perfectly valid session.
	forbidden := []string{
		"/keys", "/jobs", "/transfers", "/fetch", "/batch", "/selftest",
		"/activity", "/trash", "/search", "",
		"/../../api/v1/keys", "/drives/d1/trash",
	}
	for _, suffix := range forbidden {
		code, doc, _ := w.do("GET", pre+suffix, nil, nil)
		if code != 404 || errCodeOf(doc) != CodeNotFound {
			t.Errorf("GET %s = %d %v, want 404 %s", suffix, code, doc, CodeNotFound)
		}
	}
	code, _, _ := w.do("POST", pre+"/keys", []byte(`{}`), jsonCT())
	if code != 404 {
		t.Errorf("POST keys = %d", code)
	}
	code, _, _ = w.do("POST", pre+"/transfers", []byte(`{}`), jsonCT())
	if code != 404 {
		t.Errorf("POST transfers = %d", code)
	}
	code, _, _ = w.do("POST", pre+"/selftest", []byte(`{}`), jsonCT())
	if code != 404 {
		t.Errorf("POST selftest = %d", code)
	}

	// The UI surface works (drives-form and alias-form of the same op).
	for _, suffix := range []string{"/me", "/drives", "/drives/d1/list?path=/", "/list?path=/", "/drives/d1/stat?path=/", "/tree?path=/"} {
		code, _, _ = w.do("GET", pre+suffix, nil, nil)
		if code != 200 {
			t.Errorf("GET %s = %d", suffix, code)
		}
	}
}

// ---- machine surface never accepts the cookie ----

func TestWorkspaceCookieNotAcceptedOnV1(t *testing.T) {
	w := newWSFixture(t)
	h := w.h
	_ = w.mustLogin(chrootPlain)
	token := w.browserCookie()
	if token == "" {
		t.Fatal("no cookie in jar")
	}

	// The full agentapi handler (which owns /v1) ignores the cookie.
	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Cookie", filesCookieName+"="+token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("/v1/me with workspace cookie = %d %s", rec.Code, rec.Body.String())
	}
	// Not even as a bearer token.
	req = httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("/v1/me with token as bearer = %d", rec.Code)
	}
}

// ---- logout ----

func TestWorkspaceLogout(t *testing.T) {
	w := newWSFixture(t)
	wsA := w.mustLogin(chrootPlain)
	wsB := w.mustLogin(legacyPlain)

	code, doc, _ := w.do("POST", "/files/w/"+wsA+"/api/logout", nil, nil)
	if code != 200 || doc["ok"] != true {
		t.Fatalf("logout = %d %v", code, doc)
	}
	code, _, _ = w.do("GET", "/files/w/"+wsA+"/api/v1/me", nil, nil)
	if code != 401 {
		t.Fatalf("after logout = %d", code)
	}
	// Only this member: the sibling keeps working.
	code, _, _ = w.do("GET", "/files/w/"+wsB+"/api/v1/me", nil, nil)
	if code != 200 {
		t.Fatalf("sibling after logout = %d", code)
	}
	// Logout is idempotent and works without any cookie.
	code, doc, _ = w.do("POST", "/files/w/"+wsA+"/api/logout", nil, nil)
	if code != 200 {
		t.Fatalf("idempotent logout = %d %v", code, doc)
	}
	jar, _ := cookiejar.New(nil)
	anon := &wsFixture{t: t, h: w.h, base: w.base, client: &http.Client{Jar: jar}}
	code, _, _ = anon.do("POST", "/files/w/"+wsA+"/api/logout", nil, nil)
	if code != 200 {
		t.Fatalf("cookieless logout = %d", code)
	}

	// After logout the entry can be recovered with a fresh login only.
	code, doc, _ = w.login(chrootPlain, wsA, nil)
	if code != 200 || doc["workspace_id"] != wsA {
		t.Fatalf("re-login after logout = %d %v", code, doc)
	}
}

// ---- routing details ----

func TestWorkspaceRoutingDetails(t *testing.T) {
	w := newWSFixture(t)
	wsID := w.mustLogin(chrootPlain)

	// Wrong methods.
	code, _, resp := w.do("GET", "/files/api/login", nil, nil)
	if code != 405 || resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET login = %d allow=%q", code, resp.Header.Get("Allow"))
	}
	if code, _, _ = w.do("POST", "/files/api/workspaces", nil, nil); code != 405 {
		t.Fatalf("POST workspaces = %d", code)
	}
	if code, _, _ = w.do("GET", "/files/w/"+wsID+"/api/logout", nil, nil); code != 405 {
		t.Fatalf("GET logout = %d", code)
	}

	// Paths outside the API families are not served here (the portal owns
	// the pages): fail closed 404.
	for _, p := range []string{
		"/files/", "/files/assets/files.js", "/files/w/", "/files/w/" + wsID + "/",
		"/files/w/short/api/v1/me", "/files/w/" + strings.Repeat("x", 30) + "/api/other",
		"/files/api/unknown", "/v1/me", "/files/w/" + wsID + "/api/v1/../../api/v1/keys",
	} {
		code, _, _ = w.do("GET", p, nil, nil)
		if code != 404 {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}

	// Login response is a plain object, not the admin {code,message,data}
	// envelope.
	code, doc, _ := w.login(chrootPlain, "", nil)
	if code != 200 || doc["code"] != nil || doc["data"] != nil || doc["message"] != nil {
		t.Fatalf("login response shape = %v", doc)
	}
}

// ---- storage failure: fail closed, no internal detail to the client ----

func TestWorkspaceStorageFailureFailClosed(t *testing.T) {
	w := newWSFixture(t)
	h := w.h
	wsID := w.mustLogin(chrootPlain)
	dbPath := h.st.Path()

	// Synthetic storage outage: every store operation now fails.
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}

	checkLeak := func(stage string, code int, doc map[string]any) {
		t.Helper()
		if code < 500 && code != 401 {
			t.Fatalf("%s = %d %v, want fail-closed 5xx (or 401 on the adapter)", stage, code, doc)
		}
		if raw, err := json.Marshal(doc); err == nil {
			body := string(raw)
			for _, secret := range []string{dbPath, "sql:", "database is closed", chrootPlain} {
				if strings.Contains(body, secret) {
					t.Fatalf("%s leaks internal detail %q: %s", stage, secret, body)
				}
			}
		}
	}

	// Listing fails closed with the fixed client text only.
	code, doc, _ := w.do("GET", "/files/api/workspaces", nil, nil)
	checkLeak("list", code, doc)
	if msg, _ := doc["error"].(map[string]any)["message"].(string); msg != "the workspace service is temporarily unavailable; try again" {
		t.Fatalf("list message = %q", msg)
	}

	// Login fails closed before any session mutation; no cookie is issued.
	code, doc, resp := w.login(chrootPlain, "", nil)
	checkLeak("login", code, doc)
	for _, c := range resp.Cookies() {
		if c.Name == filesCookieName {
			t.Fatal("failed login still set a session cookie")
		}
	}

	// A presented workspace id whose lookup fails answers the same fixed
	// storage failure — not the mismatch message, not a DB error.
	code, doc, _ = w.login(chrootPlain, wsID, nil)
	checkLeak("login-lookup", code, doc)
	if errCodeOf(doc) != CodeUpstreamError {
		t.Fatalf("login-lookup code = %v", doc)
	}

	// Logout fails closed.
	code, doc, _ = w.do("POST", "/files/w/"+wsID+"/api/logout", nil, nil)
	checkLeak("logout", code, doc)

	// The data-plane adapter fails closed to a generic 401 (the session
	// cannot be verified), never a 200 on unverified state.
	code, doc, _ = w.do("GET", "/files/w/"+wsID+"/api/v1/me", nil, nil)
	if code != 401 || errCodeOf(doc) != CodeUnauthorized {
		t.Fatalf("adapter under storage failure = %d %v", code, doc)
	}
	checkLeak("adapter", code, doc)
}

// ---- routing/path helper ----

func TestWorkspaceAPIPathHelper(t *testing.T) {
	yes := []string{
		"/files/api/login", "/files/api/workspaces",
		"/files/w/abcdefghijklmnopqrstuvwxyz/api/logout",
		"/files/w/abcdefghijklmnopqrstuvwxyz/api/v1",
		"/files/w/abcdefghijklmnopqrstuvwxyz/api/v1/me",
	}
	no := []string{
		"/files/", "/files/assets/files.js", "/files/w/", "/files/w/abc",
		"/files/w/abcdefghijklmnopqrstuvwxyz/", "/files/api", "/files/apiX/login",
		"/files/w/../api/v1/me", "/v1/me",
	}
	for _, p := range yes {
		if !WorkspaceAPIPath(p) {
			t.Errorf("WorkspaceAPIPath(%q) = false", p)
		}
	}
	for _, p := range no {
		if WorkspaceAPIPath(p) {
			t.Errorf("WorkspaceAPIPath(%q) = true", p)
		}
	}
}
