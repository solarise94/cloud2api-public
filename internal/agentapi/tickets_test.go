package agentapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/store"
)

// ---- helpers ----

// mintTicketForPath mints a ticket token as the server would for (drive, path)
// under keyID, via the real HTTP route, and returns the parsed response url,
// expires_at and raw token.
func mintTicket(t *testing.T, h *harness, key, drive, path string) (url, expiresAt string) {
	t.Helper()
	rec, doc := h.do("POST", "/v1/drives/"+drive+"/download-ticket", key,
		[]byte(`{"path":"`+path+`"}`), jsonCT())
	if rec.Code != http.StatusOK {
		t.Fatalf("download-ticket %s %s = %d %v", drive, path, rec.Code, doc)
	}
	u, _ := doc["url"].(string)
	exp, _ := doc["expires_at"].(string)
	if u == "" || exp == "" {
		t.Fatalf("download-ticket response missing fields: %v", doc)
	}
	return u, exp
}

// redeem performs GET|HEAD on a minted ticket url (same-origin path + token).
func redeem(h *harness, method, url string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func ticketErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		return ""
	}
	return errCode(doc)
}

// ---- contract ----

// TestDownloadTicketContract pins the requested frontend contract: POST
// /v1/drives/{drive}/download-ticket with {"path":...} under the apikey header
// returns raw JSON {url, expires_at}; the url is a relative same-origin
// /download/{token} (no query/hash/credentials) and the expiry is fixed ~120s.
func TestDownloadTicketContract(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/big.bin", "hello world")

	before := time.Now()
	url, exp := mintTicket(t, h, chrootPlain, "d1", "/dl/big.bin")
	after := time.Now()

	if !strings.HasPrefix(url, "/download/") {
		t.Fatalf("url = %q, want relative /download/<token>", url)
	}
	if strings.ContainsAny(url, "?#") {
		t.Fatalf("url %q must not carry a query or fragment", url)
	}
	token := strings.TrimPrefix(url, "/download/")
	if strings.Contains(token, "apikey") || strings.Contains(token, "sk-") {
		t.Fatalf("token shape leaks a credential: %q", token)
	}
	// Accepted shape: b64url payload '.' b64url signature (no other chars).
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token %q is not <payload>.<signature>", token)
	}
	for _, part := range strings.Split(token, ".") {
		if part == "" {
			t.Fatalf("token %q has an empty segment", token)
		}
		for _, r := range part {
			if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
				t.Fatalf("token %q has a non-url-safe character %q", token, r)
			}
		}
	}
	ts, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", exp, err)
	}
	if ts.Before(before.Add(ticketTTL-time.Second)) || ts.After(after.Add(ticketTTL+time.Second)) {
		t.Fatalf("expires_at %v is not ~%s ahead", ts, ticketTTL)
	}

	// The ticket endpoint must be a UI-only route: hidden from the documented
	// surface (routeTable), the basic selector and the served OpenAPI.
	if isBasicRoute(http.MethodPost, "/v1/drives/{drive}/download-ticket") {
		t.Error("download-ticket must not be a basic route")
	}
	for _, r := range routeTable {
		if r.Path == "/v1/drives/{drive}/download-ticket" || strings.HasPrefix(r.Path, "/download/") {
			t.Errorf("download-ticket leaked into routeTable: %s", r.Path)
		}
	}
	raw := marshalOpenAPIDoc(buildOpenAPI("http://example.com"))
	if strings.Contains(string(raw), "download-ticket") || strings.Contains(string(raw), `"/download/`) {
		t.Error("served openapi mentions the ticket route")
	}
}

// TestDownloadTicketRequiresReadNotLink proves read is required and link alone
// is not sufficient.
func TestDownloadTicketRequiresReadNotLink(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")
	// A key with link but no read on d1.
	if err := h.st.InsertKey(store.APIKey{
		ID: "linkonly", Hash: hashOf("sk-c2a-lk11lk11-linkonlysecretlinkonlysecret"), CreatedAt: time.Now().UTC(),
		Drives: []store.KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list", "link"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	rec, doc := h.do("POST", "/v1/drives/d1/download-ticket", "sk-c2a-lk11lk11-linkonlysecretlinkonlysecret",
		[]byte(`{"path":"/dl/f.txt"}`), jsonCT())
	if rec.Code != http.StatusForbidden || errCode(doc) != "forbidden" {
		t.Fatalf("link-only key = %d %v, want 403 forbidden", rec.Code, doc)
	}
	// Read on the key succeeds.
	mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
}

// TestDownloadTicketFailClosedInputs covers malformed paths, directories,
// missing body/path, wrong drive and wrong method on the create route.
func TestDownloadTicketFailClosedInputs(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")
	h.do("POST", "/v1/mkdir", chrootPlain, []byte(`{"path":"/dl"}`), jsonCT())

	cases := []struct {
		name   string
		target string
		body   string
		code   int
		err    string
	}{
		{"missing path", "/v1/drives/d1/download-ticket", `{}`, 400, "invalid_request"},
		{"empty path", "/v1/drives/d1/download-ticket", `{"path":""}`, 400, "invalid_request"},
		{"bad json", "/v1/drives/d1/download-ticket", `not json`, 400, "invalid_request"},
		{"traversal", "/v1/drives/d1/download-ticket", `{"path":"/../etc"}`, 400, "path_invalid"},
		{"backslash", "/v1/drives/d1/download-ticket", `{"path":"/a\\b"}`, 400, "path_invalid"},
		{"directory", "/v1/drives/d1/download-ticket", `{"path":"/dl"}`, 400, "invalid_request"},
		{"missing file", "/v1/drives/d1/download-ticket", `{"path":"/dl/nope"}`, 404, "path_not_found"},
		{"unknown drive", "/v1/drives/ghost/download-ticket", `{"path":"/dl/f.txt"}`, 404, "drive_not_found"},
		{"ungranted drive", "/v1/drives/d2/download-ticket", `{"path":"/dl/f.txt"}`, 403, "forbidden"},
	}
	for _, tc := range cases {
		rec, doc := h.do("POST", tc.target, chrootPlain, []byte(tc.body), jsonCT())
		if rec.Code != tc.code || errCode(doc) != tc.err {
			t.Errorf("%s = %d %v, want %d %s", tc.name, rec.Code, doc, tc.code, tc.err)
		}
	}
	// Wrong method on the create route.
	rec, _ := h.do("GET", "/v1/drives/d1/download-ticket?path=/dl/f.txt", chrootPlain, nil, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET download-ticket = %d, want 405", rec.Code)
	}
}

// TestTicketDownloadStreamsAndHeaders proves a redeemed ticket streams the
// real bytes with an attachment disposition and no-store/referrer safeguards.
func TestTicketDownloadStreamsAndHeaders(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/greeting.txt", "hello ticket")
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/greeting.txt")

	rec := redeem(h, http.MethodGet, url, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello ticket" {
		t.Errorf("body = %q, want %q", got, "hello ticket")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rp := rec.Header().Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", rp)
	}
	if xo := rec.Header().Get("X-Content-Type-Options"); xo != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", xo)
	}
	cd := rec.Header().Get("Content-Disposition")
	mt, params, err := mime.ParseMediaType(cd)
	if err != nil || mt != "attachment" {
		t.Fatalf("Content-Disposition %q unparseable: %v", cd, err)
	}
	if params["filename"] != "greeting.txt" {
		t.Errorf("filename = %q, want greeting.txt", params["filename"])
	}
}

// TestTicketDownloadHeadAndRange proves HEAD sends headers without a body and
// Range is forwarded as a 206 partial response (so the browser streams large
// files without buffering them in page memory).
func TestTicketDownloadHeadAndRange(t *testing.T) {
	h := newHarness(t)
	body := strings.Repeat("abcdefgh", 1024) // 8 KiB
	put(t, h, "/dl/eight.bin", body)
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/eight.bin")

	head := redeem(h, http.MethodHead, url, nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes, want 0", head.Body.Len())
	}
	if cl := head.Header().Get("Content-Length"); cl != "8192" {
		t.Errorf("HEAD Content-Length = %q, want 8192", cl)
	}
	if ar := head.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("HEAD Accept-Ranges = %q, want bytes", ar)
	}

	partial := redeem(h, http.MethodGet, url, map[string]string{"Range": "bytes=8-15"})
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("Range = %d, want 206", partial.Code)
	}
	if got := partial.Body.String(); got != "abcdefgh" {
		t.Errorf("Range body = %q", got)
	}
	if cr := partial.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 8-15/8192") {
		t.Errorf("Content-Range = %q", cr)
	}
}

// TestTicketDownloadFailClosedOnStateChange covers every identity/authorization
// change that must invalidate an outstanding ticket: tampering, expiry, key
// revocation, key expiry, credential rotation, read-grant removal, root
// change, drive removal and storage-identity change.
func TestTicketDownloadFailClosedOnStateChange(t *testing.T) {
	// Tampered signature (mutate the first signature character so the decoded
	// MAC bytes are guaranteed to differ).
	t.Run("tamper", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		sigStart := strings.LastIndexByte(url, '.') + 1
		b := []byte(url)
		if b[sigStart] == 'A' {
			b[sigStart] = 'B'
		} else {
			b[sigStart] = 'A'
		}
		rec := redeem(h, http.MethodGet, string(b), nil)
		if rec.Code != http.StatusGone || ticketErrCode(t, rec) != "ticket_invalid" {
			t.Fatalf("tampered = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Expiry before request start.
	t.Run("expired", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		row, err := h.st.GetKey("chroot")
		if err != nil {
			t.Fatal(err)
		}
		tok, err := h.api.mintTicketToken(ticketPayload{
			Kind: "tkt", Drive: "d1", Path: "/agents/x/dl/f.txt", Root: "/agents/x",
			KeyID: "chroot", Cred: h.api.keyCredTag(row.Hash), Exp: time.Now().Add(-time.Second).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, "/download/"+tok, nil)
		if rec.Code != http.StatusGone || ticketErrCode(t, rec) != "ticket_invalid" {
			t.Fatalf("expired = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Key revocation.
	t.Run("revoked", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		if err := h.keys.Revoke("chroot"); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("revoked = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Key expiry.
	t.Run("key_expired", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		past := time.Now().Add(-time.Hour)
		if err := h.st.Exec(`UPDATE api_keys SET expires_at=? WHERE id='chroot'`, past.UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			t.Fatal(err)
		}
		if err := h.keys.Reload(); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("key expired = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Credential rotation under the same key id.
	t.Run("rotated", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		if _, _, err := h.keys.Rotate("chroot"); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("rotated = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Read grant removed.
	t.Run("read_removed", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		if _, err := h.keys.Patch("chroot", func(k *store.APIKey) error {
			k.Drives[0].Perms = []string{"list", "write", "delete", "link"}
			k.GlobalPerms = nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("read removed = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Chroot root changed.
	t.Run("root_changed", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		if _, err := h.keys.Patch("chroot", func(k *store.APIKey) error {
			k.Drives[0].Root = "/elsewhere"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("root changed = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Drive removed from the store and registry.
	t.Run("drive_removed", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		if err := h.st.DeleteDrive("d1"); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("drive removed = %d %s", rec.Code, rec.Body.String())
		}
	})

	// Storage identity changed (a new storage bound under the same mount).
	t.Run("storage_replaced", func(t *testing.T) {
		h := newHarness(t)
		put(t, h, "/dl/f.txt", "x")
		url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
		newID := h.fake.addStorage("/m1", "Local")
		if err := h.st.Exec(`UPDATE drives SET storage_id=? WHERE id='d1'`, newID); err != nil {
			t.Fatal(err)
		}
		rec := redeem(h, http.MethodGet, url, nil)
		if rec.Code != http.StatusGone {
			t.Fatalf("storage replaced = %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestTicketDomainSeparation proves tickets and public links are not
// interchangeable: a link token does not redeem as a ticket and a ticket token
// does not verify as a link.
func TestTicketDomainSeparation(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")

	// A real /v1/links token cannot be used as a ticket.
	rec, doc := h.do("POST", "/v1/links", chrootPlain, []byte(`{"path":"/dl/f.txt","expires_in":3600}`), jsonCT())
	if rec.Code != http.StatusOK {
		t.Fatalf("link create = %d %v", rec.Code, doc)
	}
	linkURL, _ := doc["url"].(string)
	linkToken := strings.TrimPrefix(linkURL, "/l/")
	if _, err := h.api.verifyTicketToken(linkToken); err == nil {
		t.Fatal("a link token verified as a ticket")
	}
	rec = redeem(h, http.MethodGet, "/download/"+linkToken, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("link token as ticket = %d %s", rec.Code, rec.Body.String())
	}

	// A ticket token cannot be used as a public link.
	ticketURL, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")
	ticketToken := strings.TrimPrefix(ticketURL, "/download/")
	if _, err := h.api.verifyLinkToken(ticketToken); err == nil {
		t.Fatal("a ticket token verified as a link")
	}
	rec = redeem(h, http.MethodGet, "/l/"+ticketToken, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("ticket token as link = %d %s", rec.Code, rec.Body.String())
	}
}

// TestTicketGlobalReadUnion proves the global+drive read union applies at
// redemption: a global read permission authorizes a ticket even without a
// per-drive read grant, and dropping the global read invalidates it.
func TestTicketGlobalReadUnion(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")
	if err := h.st.InsertKey(store.APIKey{
		ID: "globread", Hash: hashOf("sk-c2a-gl11gl11-globreadsecretglobreadsecret"), CreatedAt: time.Now().UTC(),
		Drives:      []store.KeyDrive{{Drive: "d1", Root: "/agents/x", Perms: []string{"list"}}},
		GlobalPerms: []string{"read"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	const key = "sk-c2a-gl11gl11-globreadsecretglobreadsecret"
	url, _ := mintTicket(t, h, key, "d1", "/dl/f.txt")
	if rec := redeem(h, http.MethodGet, url, nil); rec.Code != http.StatusOK {
		t.Fatalf("global read ticket = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.keys.Patch("globread", func(k *store.APIKey) error {
		k.GlobalPerms = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := redeem(h, http.MethodGet, url, nil); rec.Code != http.StatusGone {
		t.Fatalf("after global read removal = %d %s", rec.Code, rec.Body.String())
	}
}

// TestTicketRateLimited proves redemption consumes the issuing key's normal
// request budget (rpm=1: the mint uses the token, the immediate redeem is 429).
func TestTicketRateLimited(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")
	one := 1
	if err := h.st.InsertKey(store.APIKey{
		ID: "slow", Hash: hashOf("sk-c2a-sl11sl11-slowsecretkeysecret12ab"), CreatedAt: time.Now().UTC(),
		Drives: []store.KeyDrive{{Drive: "d1", Root: "/agents/x", Perms: []string{"read"}}},
		Limits: store.Limits{RequestsPerMinute: &one},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	const key = "sk-c2a-sl11sl11-slowsecretkeysecret12ab"
	url, _ := mintTicket(t, h, key, "d1", "/dl/f.txt")
	rec := redeem(h, http.MethodGet, url, nil)
	if rec.Code != http.StatusTooManyRequests || ticketErrCode(t, rec) != "rate_limited" {
		t.Fatalf("redeem after mint = %d %s, want 429 rate_limited", rec.Code, rec.Body.String())
	}
}

// TestTicketWrongMethodRedemption proves non-GET/HEAD redemption is 405 and
// malformed tokens do not panic.
func TestTicketWrongMethodRedemption(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/f.txt", "x")
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.txt")

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := redeem(h, m, url, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s redeem = %d, want 405", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s Allow = %q, want GET, HEAD", m, allow)
		}
	}
	for _, bad := range []string{"/download/", "/download/nodot", "/download/a.b.c", "/download/.sig", "/download/payload."} {
		rec := redeem(h, http.MethodGet, bad, nil)
		if rec.Code != http.StatusGone {
			t.Errorf("%s = %d, want 410", bad, rec.Code)
		}
	}
}

// TestTicketContentDispositionSafety proves the header is built safely for
// malicious and non-ASCII filenames (no header injection, no unparseable
// value) — via mime.FormatMediaType, with a sanitized fallback.
func TestTicketContentDispositionSafety(t *testing.T) {
	for _, name := range []string{
		`evil"; filename="x.txt`,
		"crlf\r\nX-Injected: 1",
		"nul\x00name",
		"back\\slash",
		"报告 2024.pdf",
		"",
	} {
		cd := contentDisposition(name, "/a/"+name)
		if strings.ContainsAny(cd, "\r\n\x00") {
			t.Errorf("disposition %q for name %q carries control bytes", cd, name)
		}
		mt, params, err := mime.ParseMediaType(cd)
		if err != nil || mt != "attachment" {
			t.Errorf("disposition %q for name %q unparseable: %v", cd, name, err)
			continue
		}
		if params["filename"] == "" {
			t.Errorf("disposition %q for name %q lacks a filename", cd, name)
		}
	}
}

// TestTicketLargeFileStreamsWithoutBuffering proves a large file is streamed
// through and a Range request returns only the requested slice (the transport
// never needs the whole file in memory).
func TestTicketLargeFileStreamsWithoutBuffering(t *testing.T) {
	h := newHarness(t)
	const size = 3 << 20 // 3 MiB
	content := strings.Repeat("Z", size)
	h.fake.mu.Lock()
	h.fake.entries["/m1/agents/x/dl/big.bin"] = &fakeEntry{
		name: "big.bin", content: []byte(content), modified: "2026-10-12T00:00:00Z",
	}
	h.fake.mu.Unlock()
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/big.bin")

	sl := redeem(h, http.MethodGet, url, map[string]string{"Range": "bytes=0-15"})
	if sl.Code != http.StatusPartialContent {
		t.Fatalf("slice = %d, want 206", sl.Code)
	}
	if sl.Body.Len() != 16 {
		t.Errorf("slice length = %d, want 16", sl.Body.Len())
	}
	if cr := sl.Header().Get("Content-Range"); cr != "bytes 0-15/3145728" {
		t.Errorf("Content-Range = %q", cr)
	}

	full := redeem(h, http.MethodGet, url, nil)
	if full.Code != http.StatusOK {
		t.Fatalf("full = %d", full.Code)
	}
	if full.Body.Len() != size {
		t.Errorf("full length = %d, want %d", full.Body.Len(), size)
	}
}

// TestTicketUnsatisfiableRangePreserved proves a range past EOF keeps the
// upstream 416 and its `Content-Range: bytes */N` (no 502 mapping, no raw
// upstream error body leaked).
func TestTicketUnsatisfiableRangePreserved(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.entries["/m1/agents/x/dl/small.bin"] = &fakeEntry{
		name: "small.bin", content: []byte("0123456789"), modified: "2026-10-12T00:00:00Z",
	}
	h.fake.mu.Unlock()
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/small.bin")

	rec := redeem(h, http.MethodGet, url, map[string]string{"Range": "bytes=100-200"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past EOF = %d %s, want 416", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("Content-Range = %q, want bytes */10", cr)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	// No raw upstream error text and no JSON error envelope on a 416.
	if strings.Contains(rec.Body.String(), "upstream") || strings.Contains(rec.Body.String(), "object not found") {
		t.Errorf("416 body leaks upstream detail: %q", rec.Body.String())
	}
}

// TestTicketTruncatedUpstreamAborts proves a short/truncated upstream body is
// never delivered as a complete native download: the client observes a read
// error, not a clean EOF, and the failure is recorded in the audit log.
func TestTicketTruncatedUpstreamAborts(t *testing.T) {
	cases := []struct {
		name    string
		chunked bool
		rangeHd string
	}{
		{"declared_length_short_write", false, ""},
		{"chunked_no_content_length", true, ""},
		{"partial_206_short_write", false, "bytes=0-999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			const size = 4096
			h.fake.mu.Lock()
			h.fake.entries["/m1/agents/x/dl/trunc.bin"] = &fakeEntry{
				name: "trunc.bin", content: bytes.Repeat([]byte("Q"), size), modified: "2026-10-12T00:00:00Z",
			}
			h.fake.pTruncateTo = 128
			h.fake.pChunked = tc.chunked
			h.fake.mu.Unlock()

			url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/trunc.bin")

			// A real server so the client sees the transport abort (a
			// ResponseRecorder cannot model an interrupted connection).
			srv := httptest.NewServer(h.handler)
			defer srv.Close()

			hdr := map[string]string{}
			if tc.rangeHd != "" {
				hdr["Range"] = tc.rangeHd
			}
			req, err := http.NewRequest(http.MethodGet, srv.URL+url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			// The aborted transfer may surface either while reading the body or
			// as a transport error from Do; both mean the client did NOT see a
			// clean complete download.
			resp, doErr := http.DefaultClient.Do(req)
			if doErr == nil {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil {
					t.Fatalf("client read succeeded (%d bytes) despite truncated upstream, want error", len(body))
				}
				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
					t.Fatalf("status = %d, want 200/206", resp.StatusCode)
				}
				if len(body) >= size {
					t.Errorf("client received %d bytes, want < %d", len(body), size)
				}
			}

			// The audit row records the interruption for this key.
			entries, _, err := h.st.QueryAudit(store.AuditFilter{KeyExact: "chroot", Op: "ticket_download", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range entries {
				if strings.Contains(e.Error, "stream_interrupted") {
					found = true
				}
			}
			if !found {
				t.Errorf("audit lacks a stream_interrupted row: %+v", entries)
			}
		})
	}
}

// TestTicketCompleteTransferNotAborted is the counterpart: an intact upstream
// body of exactly the declared length streams through cleanly and audits no
// interruption.
func TestTicketCompleteTransferNotAborted(t *testing.T) {
	h := newHarness(t)
	const size = 2048
	h.fake.mu.Lock()
	h.fake.entries["/m1/agents/x/dl/ok.bin"] = &fakeEntry{
		name: "ok.bin", content: bytes.Repeat([]byte("K"), size), modified: "2026-10-12T00:00:00Z",
	}
	h.fake.mu.Unlock()
	url, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/ok.bin")

	rec := redeem(h, http.MethodGet, url, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != size {
		t.Fatalf("intact stream = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	entries, _, err := h.st.QueryAudit(store.AuditFilter{KeyExact: "chroot", Op: "ticket_download", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Error, "stream_interrupted") {
			t.Errorf("clean transfer audited as interrupted: %+v", e)
		}
	}
}
