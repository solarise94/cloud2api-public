package agentapi

// OpenList's /p endpoint can reject a storage whose /d endpoint provides a
// working direct link. Exercise both proxy and redirect downloads.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud2api/internal/store"
)

// requireDBytes asserts a 200 response carrying exactly the provider bytes.
func requireDBytes(t *testing.T, rec *httptest.ResponseRecorder, payload string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != payload {
		t.Errorf("downloaded %q, want %q", rec.Body.String(), payload)
	}
}

// assertNoPAndSignedD proves the flows used /d with the server-fetched sign
// and never the restricted /p route.
func assertNoPAndSignedD(t *testing.T, h *harness) {
	t.Helper()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if h.fake.pHits != 0 {
		t.Errorf("the restricted /p download route was hit %d times", h.fake.pHits)
	}
	if len(h.fake.dQueries) == 0 {
		t.Fatal("no /d download request reached the fake")
	}
	for _, q := range h.fake.dQueries {
		if !strings.Contains(q, "sign=sig%3A0") {
			t.Errorf("/d query %q lacks the server-fetched sign", q)
		}
	}
}

// downloadSurfaces runs every download flow over h and checks the bytes.
func downloadSurfaces(t *testing.T, h *harness, payload string) {
	t.Helper()

	// v1 files endpoint (chroot key).
	rec, doc := h.do(http.MethodGet, "/v1/files?path=/dl/f.bin", chrootPlain, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 download = %d %v", rec.Code, doc)
	}
	requireDBytes(t, rec, payload)

	// Legacy key (root "/") on the same route: it addresses the same object
	// by its full drive path.
	rec, doc = h.do(http.MethodGet, "/v1/files?path=/agents/x/dl/f.bin", legacyPlain, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy download = %d %v", rec.Code, doc)
	}
	requireDBytes(t, rec, payload)

	// Same-origin ticket.
	ticketURL, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/f.bin")
	if rec := redeem(h, http.MethodGet, ticketURL, nil); rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("ticket download = %d %q", rec.Code, rec.Body.String())
	}

	// Public link.
	rec, doc = h.do(http.MethodPost, "/v1/links", chrootPlain,
		[]byte(`{"path":"/dl/f.bin","expires_in":3600}`), jsonCT())
	if rec.Code != http.StatusOK {
		t.Fatalf("link create = %d %v", rec.Code, doc)
	}
	linkURL, _ := doc["url"].(string)
	if rec := redeem(h, http.MethodGet, linkURL, nil); rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("link download = %d %q", rec.Code, rec.Body.String())
	}

	// Gokapi relay public download.
	rec, doc = gokapiUpload(t, h, relayPlain, map[string]string{"allowedDownloads": "2", "expiryDays": "7"}, "relay.bin", []byte(payload), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("relay upload = %d %v", rec.Code, doc)
	}
	id, _ := fileInfoOf(t, doc)["Id"].(string)
	if id == "" {
		t.Fatalf("relay upload without id: %v", doc)
	}
	rec, _ = h.do(http.MethodGet, "/downloadFile?id="+id, "", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("relay download = %d %q", rec.Code, rec.Body.String())
	}

	// Small-object read-back (selftest getBytes round-trip).
	rec, doc = h.do(http.MethodPost, "/v1/selftest", chrootPlain, nil, nil)
	if rec.Code != http.StatusOK || doc["all_ok"] != true {
		t.Fatalf("selftest (getBytes small read) = %d %v", rec.Code, doc)
	}
}

// TestDownloadSurfacesUseDNotP proves the proxy-mode /d path: provider bytes
// stream through every surface, the upstream query carries the server-fetched
// sign, and the restricted /p route is never touched.
func TestDownloadSurfacesUseDNotP(t *testing.T) {
	h := newHarness(t)
	const payload = "same-origin provider bytes"
	put(t, h, "/dl/f.bin", payload)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	downloadSurfaces(t, h, payload)
	assertNoPAndSignedD(t, h)
}

// TestDownloadDRedirectFollowedServerSide proves a storage without MustProxy
// (a /d 302 to a direct link) still delivers the provider bytes through every
// same-origin surface: the redirect is followed server-side, never forwarded.
func TestDownloadDRedirectFollowedServerSide(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.dRedirect = true
	h.fake.mu.Unlock()
	const payload = "redirected provider bytes"
	put(t, h, "/dl/f.bin", payload)
	h.addRelayKey(t, "relay", relayPlain, store.RelaySettings{})

	downloadSurfaces(t, h, payload)
	assertNoPAndSignedD(t, h)

	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if h.fake.directHits == 0 {
		t.Error("the /d 302 was never followed to the direct link")
	}
}

// TestDownloadQueryWhitelistedServerSide proves the upstream /d query is a
// strict whitelist: the server-fetched sign only, plus the caller's own type
// selector when one was supplied. A client sign, key or arbitrary parameter
// never reaches OpenList (the fake answers 403 if they do).
func TestDownloadQueryWhitelistedServerSide(t *testing.T) {
	h := newHarness(t)
	const payload = "whitelisted bytes"
	put(t, h, "/dl/f.bin", payload)

	// Hostile query: a client sign plus junk parameters must be dropped.
	rec, _ := h.do(http.MethodGet,
		"/v1/files?path=/dl/f.bin&sign=evil&apikey=sk-c2a-forged&key=x&foo=bar", chrootPlain, nil, nil)
	requireDBytes(t, rec, payload)

	// A preexisting caller type selector is forwarded next to the sign.
	rec, _ = h.do(http.MethodGet, "/v1/files?path=/dl/f.bin&type=inline", chrootPlain, nil, nil)
	requireDBytes(t, rec, payload)

	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if len(h.fake.dQueries) != 2 {
		t.Fatalf("/d queries = %v, want exactly two downloads", h.fake.dQueries)
	}
	if q := h.fake.dQueries[0]; q != "sign=sig%3A0" {
		t.Errorf("hostile query forwarded upstream: %q, want sign only", q)
	}
	if q := h.fake.dQueries[1]; q != "sign=sig%3A0&type=inline" {
		t.Errorf("type selector lost or mangled: %q", q)
	}
}

// TestTicketConditionalRequestsUnderD pins the conditional semantics of the
// ticket redemption on the new route: a revalidation hit forwards as a
// bodyless 304 with the entity tag, a valid range stays a 206, and a range
// past EOF keeps the upstream 416 with its bytes */N Content-Range.
func TestTicketConditionalRequestsUnderD(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/dl/c.txt", "cond bytes")
	ticketURL, _ := mintTicket(t, h, chrootPlain, "d1", "/dl/c.txt")

	rec := redeem(h, http.MethodGet, ticketURL, map[string]string{"If-None-Match": `"anything"`})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional redeem = %d %s, want 304", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("304 lacks the upstream ETag")
	}

	rec = redeem(h, http.MethodGet, ticketURL, map[string]string{"Range": "bytes=0-4"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "cond " {
		t.Fatalf("range redeem = %d %q, want 206", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-4/10" {
		t.Errorf("Content-Range = %q", cr)
	}

	rec = redeem(h, http.MethodGet, ticketURL, map[string]string{"Range": "bytes=100-200"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past EOF = %d %s, want 416", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes */10" {
		t.Errorf("416 Content-Range = %q, want bytes */10", cr)
	}
}
