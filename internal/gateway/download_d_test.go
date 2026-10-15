package gateway

// Regression tests for the /d download migration on the legacy raw route:
// handleRaw must fetch OpenList's /d endpoint (server-fetched sign only, the
// caller's own type selector whitelisted through, a client sign/key/arbitrary
// query dropped) and never touch the restricted /p route. When the storage is
// not a MustProxy one, /d answers a 302 direct link that DoDownload follows
// server-side, so the gateway client still sees a plain 200 byte stream.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dFakeState records the fake upstream's download-route observations.
type dFakeState struct {
	mu       sync.Mutex
	pHits    int
	dQueries []string
	direct   bool
}

func (s *dFakeState) addP() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pHits++
}

func (s *dFakeState) addDQuery(q string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dQueries = append(s.dQueries, q)
}

func (s *dFakeState) markDirect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.direct = true
}

// serveSign writes the fs/get answer carrying the server-fetched sign.
func serveSign(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"code":200,"message":"success","data":{"sign":"server-sign"}}`)
}

// serveDBytes answers an authorized /d request with the payload bytes,
// enforcing the same sign + whitelist gate as the fake in internal/agentapi.
func serveDBytes(w http.ResponseWriter, r *http.Request, payload []byte) {
	if r.URL.Query().Get("sign") != "server-sign" {
		http.Error(w, "invalid or missing sign", http.StatusForbidden)
		return
	}
	for k := range r.URL.Query() {
		if k != "sign" && k != "type" {
			http.Error(w, "unexpected query parameter "+k, http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	_, _ = w.Write(payload)
}

// TestRawUsesDAndWhitelistsQuery proves the raw route fetches /d (never /p)
// with the server-fetched sign; a client sign, key or arbitrary query
// parameter is dropped, while the caller's own type selector rides along.
func TestRawUsesDAndWhitelistsQuery(t *testing.T) {
	payload := bytes.Repeat([]byte{0xC2, 0xA5}, 256)
	st := &dFakeState{}
	up := &fakeUpstream{status: http.StatusOK}
	up.serve = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/get":
			serveSign(w)
		case strings.HasPrefix(r.URL.Path, "/p/"):
			st.addP()
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":403,"message":"proxy download is disabled"}`)
		case strings.HasPrefix(r.URL.Path, "/d/"):
			st.addDQuery(r.URL.RawQuery)
			serveDBytes(w, r, payload)
		default:
			http.Error(w, "fake: unknown "+r.URL.Path, http.StatusNotFound)
		}
	}
	h := newTestGateway(t, up)

	// Hostile client query: only the whitelist may reach the upstream.
	req := httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/f.bin?sign=evil&key=x&foo=1", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hostile query download = %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("downloaded %d bytes, want %d identical bytes", rec.Body.Len(), len(payload))
	}

	// A preexisting caller type selector is forwarded next to the sign.
	req = httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/f.bin?type=inline", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("typed download = %d %s", rec.Code, rec.Body.String())
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pHits != 0 {
		t.Errorf("the restricted /p download route was hit %d times", st.pHits)
	}
	if len(st.dQueries) != 2 {
		t.Fatalf("/d queries = %v, want exactly two downloads", st.dQueries)
	}
	if q := st.dQueries[0]; q != "sign=server-sign" {
		t.Errorf("hostile query forwarded upstream: %q, want sign only", q)
	}
	if q := st.dQueries[1]; q != "sign=server-sign&type=inline" {
		t.Errorf("type selector lost or mangled: %q", q)
	}
}

// TestRawFollowsDRedirectServerSide proves a storage without MustProxy (a /d
// 302 to a direct link) still streams the bytes through the gateway: the
// redirect is followed server-side and its URL never reaches the client.
func TestRawFollowsDRedirectServerSide(t *testing.T) {
	payload := []byte("direct-link provider bytes")
	st := &dFakeState{}
	up := &fakeUpstream{status: http.StatusOK}
	up.serve = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/get":
			serveSign(w)
		case strings.HasPrefix(r.URL.Path, "/p/"):
			st.addP()
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":403,"message":"proxy download is disabled"}`)
		case strings.HasPrefix(r.URL.Path, "/d/"):
			// No MustProxy: a 302 to the direct link, same host.
			http.Redirect(w, r, "/direct"+r.URL.Path[len("/d"):], http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/direct/"):
			st.markDirect()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		default:
			http.Error(w, "fake: unknown "+r.URL.Path, http.StatusNotFound)
		}
	}
	h := newTestGateway(t, up)

	req := httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/f.bin", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redirected download = %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("downloaded %q, want the direct-link payload", rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("the upstream redirect leaked to the client: Location %q", loc)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.pHits != 0 {
		t.Errorf("the restricted /p download route was hit %d times", st.pHits)
	}
	if !st.direct {
		t.Error("the /d 302 was never followed to the direct link")
	}
}
