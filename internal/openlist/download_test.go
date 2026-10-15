package openlist

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- shared recording helpers ---

// recReq is one upstream request reduced to the bits the assertions need.
type recReq struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

// recUpstream is a thread-safe request recorder for download tests.
type recUpstream struct {
	mu   sync.Mutex
	reqs []recReq
}

func (ru *recUpstream) record(r *http.Request) {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	ru.reqs = append(ru.reqs, recReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()})
}

func (ru *recUpstream) n() int {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return len(ru.reqs)
}

func (ru *recUpstream) at(i int) recReq {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.reqs[i]
}

// dlJSON responds with a JSON envelope (used for fake 401s and fake token
// rejections inside HTTP 200 payloads).
func dlJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

const dlHealLogin = `{"code":200,"message":"success","data":{"token":"jwt-fresh"}}`
const dlHealSetting = `{"code":200,"message":"success","data":{"key":"token","value":"openlist-permanent"}}`

// dlHealCounters tracks how often the self-heal protocol endpoints were hit.
type dlHealCounters struct {
	mu     sync.Mutex
	logins int
}

func (hc *dlHealCounters) hitLogin() {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.logins++
}

func (hc *dlHealCounters) loginCount() int {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.logins
}

// --- chain, credentials, headers ---

func TestDoDownloadFollowsSameOriginChain(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		switch r.URL.Path {
		case "/p/secret.bin":
			http.Redirect(w, r, "/relay/one", http.StatusFound)
		case "/relay/one":
			w.Header().Set("Location", "/relay/two")
			w.WriteHeader(http.StatusMovedPermanently)
		case "/relay/two":
			w.Header().Set("Location", "/p/final.bin?sign=FINAL")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			fmt.Fprint(w, "final-bytes")
		}
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/secret.bin?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); body != "final-bytes" {
		t.Errorf("body = %q, want final-bytes", body)
	}
	if ru.n() != 4 {
		t.Fatalf("upstream requests = %d, want 4 (three redirects + final)", ru.n())
	}
	for i := 0; i < 4; i++ {
		rec := ru.at(i)
		if rec.Method != http.MethodGet {
			t.Errorf("hop %d method = %s, want GET", i, rec.Method)
		}
		if got := rec.Header.Get("Authorization"); got != "tok-1" {
			t.Errorf("hop %d Authorization = %q, want the managed token on every same-origin hop", i, got)
		}
		if got := rec.Header.Get(hopHeader); got != "" {
			t.Errorf("hop %d X-GoAPI-Hop = %q, want none without a hop context", i, got)
		}
	}
	if rec := ru.at(0); rec.Query != "sign=SGN" || rec.Path != "/p/secret.bin" {
		t.Errorf("first hop = %s?%s, want the original signed URL", rec.Path, rec.Query)
	}
	if rec := ru.at(3); rec.Path != "/p/final.bin" || rec.Query != "sign=FINAL" {
		t.Errorf("final hop = %s?%s, want /p/final.bin?sign=FINAL", rec.Path, rec.Query)
	}
}

func TestDoDownloadStripsCredentialsCrossOriginAndNeverRestores(t *testing.T) {
	logins := &dlHealCounters{}
	ru := &recUpstream{}
	// external is captured by the origin handler and filled in once the
	// external server exists (its URL is only known after creation).
	var external string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			logins.hitLogin()
			fmt.Fprint(w, dlHealLogin)
			return
		}
		ru.record(r)
		switch r.URL.Path {
		case "/p/f":
			http.Redirect(w, r, external, http.StatusFound)
		case "/p/back":
			fmt.Fprint(w, "returned")
		default:
			dlJSON(w, http.StatusInternalServerError, `{"code":500}`)
		}
	}))
	defer origin.Close()

	ext := &recUpstream{}
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext.record(r)
		// The chain returns to the OpenList origin — credentials must stay gone.
		http.Redirect(w, r, origin.URL+"/p/back", http.StatusFound)
	}))
	defer extSrv.Close()
	external = extSrv.URL + "/dl?sig=EX"

	c, err := New(origin.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	ctx := WithHop(context.Background(), 2)
	resp, err := c.DoDownload(ctx, http.MethodGet, mustURL("/p/f?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if body := readAll(t, resp.Body); body != "returned" {
		t.Errorf("body = %q, want returned", body)
	}
	if logins.loginCount() != 0 {
		t.Errorf("logins = %d, want 0 (no 401, no heal)", logins.loginCount())
	}
	if ru.n() != 2 {
		t.Fatalf("origin requests = %d, want 2", ru.n())
	}
	first, back := ru.at(0), ru.at(1)
	if got := first.Header.Get("Authorization"); got != "tok-1" {
		t.Errorf("initial Authorization = %q, want the managed token", got)
	}
	if got := first.Header.Get(hopHeader); got != "2" {
		t.Errorf("initial X-GoAPI-Hop = %q, want 2", got)
	}
	if ext.n() != 1 {
		t.Fatalf("external requests = %d, want 1", ext.n())
	}
	ereq := ext.at(0)
	for _, h := range []string{"Authorization", "Cookie", hopHeader} {
		if got := ereq.Header.Get(h); got != "" {
			t.Errorf("external request carries %s = %q, want it stripped", h, got)
		}
	}
	// The chain returned to the OpenList origin: credentials must NOT be restored.
	for _, h := range []string{"Authorization", "Cookie", hopHeader} {
		if got := back.Header.Get(h); got != "" {
			t.Errorf("return-to-origin request carries %s = %q, want it gone permanently", h, got)
		}
	}
}

func TestDoDownloadHeaderAllowlist(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		fmt.Fprint(w, "ok")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Range", "bytes=0-3")
	hdr.Set("If-Range", `"etag-1"`)
	hdr.Set("If-Modified-Since", "Wed, 01 Jan 2025 00:00:00 GMT")
	hdr.Set("If-None-Match", `"w-1"`)
	hdr.Set("Accept", "*/*")
	hdr.Add("Accept", "text/plain")
	hdr.Set("User-Agent", "downloader/1")
	hdr.Set("Cookie", "session=x")
	hdr.Set("Authorization", "forged-token")
	hdr.Set("X-Custom", "nope")

	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), hdr)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()

	rec := ru.at(0)
	for name, want := range map[string]string{
		"Range":             "bytes=0-3",
		"If-Range":          `"etag-1"`,
		"If-Modified-Since": "Wed, 01 Jan 2025 00:00:00 GMT",
		"If-None-Match":     `"w-1"`,
		"User-Agent":        "downloader/1",
	} {
		if got := rec.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := rec.Header.Values("Accept"); len(got) != 2 || got[0] != "*/*" || got[1] != "text/plain" {
		t.Errorf("Accept = %v, want both forwarded values", got)
	}
	if got := rec.Header.Get("Cookie"); got != "" {
		t.Errorf("Cookie = %q, want dropped", got)
	}
	if got := rec.Header.Get("X-Custom"); got != "" {
		t.Errorf("X-Custom = %q, want dropped", got)
	}
	if got := rec.Header.Get("Authorization"); got != "tok-1" {
		t.Errorf("Authorization = %q, want the managed token (caller value never wins)", got)
	}
}

func TestDoDownloadPinsInitialURLToOrigin(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		fmt.Fprint(w, "pinned")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	// Hostile initial URL: foreign origin and embedded userinfo are ignored —
	// only path and query reach the wire, pinned to the OpenList endpoint.
	u := url.URL{
		Scheme:   "https",
		Host:     "evil.example:99",
		User:     url.UserPassword("bob", "hunter2"),
		Path:     "/p/x",
		RawQuery: "sign=SGN",
	}
	resp, err := c.DoDownload(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if ru.n() != 1 {
		t.Fatalf("upstream requests = %d, want 1 (hit the pinned origin)", ru.n())
	}
	rec := ru.at(0)
	if rec.Path != "/p/x" || rec.Query != "sign=SGN" {
		t.Errorf("upstream got %s?%s, want /p/x?sign=SGN", rec.Path, rec.Query)
	}
}

func TestDoDownloadRejectsMethodsOtherThanGetHead(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		fmt.Fprint(w, "x")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions, "CONNECT"} {
		if _, err := c.DoDownload(context.Background(), method, mustURL("/p/x"), nil); err == nil {
			t.Errorf("DoDownload(%s) succeeded, want error", method)
		}
	}
	if ru.n() != 0 {
		t.Errorf("upstream requests = %d, want 0", ru.n())
	}
	// GET and HEAD are accepted.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if _, err := c.DoDownload(context.Background(), method, mustURL("/p/x"), nil); err != nil {
			t.Errorf("DoDownload(%s): %v", method, err)
		}
	}
}

// --- redirect validation ---

func TestDoDownloadRejectsBadRedirectTargets(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		wantErr  string
	}{
		{"empty-location", http.StatusFound, "", "Location"},
		{"blank-location", http.StatusFound, "   ", "Location"},
		{"ftp-scheme", http.StatusMovedPermanently, "ftp://127.0.0.1:21/f", "scheme"},
		{"javascript-scheme", http.StatusFound, "javascript:alert(1)", "scheme"},
		{"userinfo-target", http.StatusTemporaryRedirect, "http://user:pw@127.0.0.1:9/x", "credentials"},
		{"status-300", http.StatusMultipleChoices, "/x", "not followed"},
		{"status-305", http.StatusUseProxy, "/x", "not followed"},
		// A Location the transport itself cannot parse is rejected inside
		// http.Client, before ErrUseLastResponse applies; downloadError hides
		// the client's message (which embeds both the signed URL and the
		// Location value) behind the generic transport text.
		{"malformed-location", http.StatusFound, "http://127.0.0.1:1/%zz", "request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", tc.location)
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()

			c, err := New(ts.URL, "tok-1")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/secret.bin?sign=SGN"), nil)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("DoDownload succeeded with status %d and Location %q, want error", tc.status, tc.location)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantErr)
			}
			// The signed URL and its path never appear in the error text.
			for _, secret := range []string{"sign=SGN", "/p/secret.bin"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error text %q leaks %q", err.Error(), secret)
				}
			}
		})
	}
}

func TestDoDownloadPasses304Through(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		if got := r.Header.Get("If-None-Match"); got != `"v1"` {
			t.Errorf("upstream If-None-Match = %q", got)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("If-None-Match", `"v1"`)
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), hdr)
	if err != nil {
		t.Fatalf("DoDownload: %v (304 is a valid conditional response, not a redirect)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); body != "" {
		t.Errorf("304 body = %q, want empty", body)
	}
}

func TestDoDownloadRedirectLimit(t *testing.T) {
	cases := []struct {
		name        string
		redirects   int  // how many 302s the server answers before 200
		wantErr     bool // expect "too many redirects"
		wantErrText string
	}{
		{"exactly-five-is-ok", 5, false, ""},
		{"six-is-too-many", 6, true, "too many redirects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			hits := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hits++
				h := hits
				mu.Unlock()
				if h <= tc.redirects {
					http.Redirect(w, r, "/loop", http.StatusFound)
					return
				}
				fmt.Fprint(w, "made-it")
			}))
			defer ts.Close()

			c, err := New(ts.URL, "tok-1")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x"), nil)
			mu.Lock()
			served := hits
			mu.Unlock()
			if tc.wantErr {
				if err == nil {
					resp.Body.Close()
					t.Fatalf("DoDownload succeeded, want %q", tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantErrText)
				}
			} else {
				if err != nil {
					t.Fatalf("DoDownload: %v", err)
				}
				defer resp.Body.Close()
				if body := readAll(t, resp.Body); body != "made-it" {
					t.Errorf("body = %q", body)
				}
			}
			// The client stops exactly at the budget: initial request plus five hops.
			if served != 6 {
				t.Errorf("upstream requests = %d, want 6", served)
			}
		})
	}
}

func TestResolveDownloadTarget(t *testing.T) {
	base := url.URL{Scheme: "http", Host: "o:1", Path: "/p/a", RawQuery: "sign=OLD"}
	next, err := resolveDownloadTarget(&base, "b?sign=NEW")
	if err != nil {
		t.Fatal(err)
	}
	if got := next.String(); got != "http://o:1/p/b?sign=NEW" {
		t.Errorf("relative resolution = %q, want http://o:1/p/b?sign=NEW", got)
	}
	next, err = resolveDownloadTarget(&base, "/abs?z=1")
	if err != nil {
		t.Fatal(err)
	}
	if got := next.String(); got != "http://o:1/abs?z=1" {
		t.Errorf("absolute-path resolution = %q, want http://o:1/abs?z=1", got)
	}
	for _, loc := range []string{"", "   ", "http://127.0.0.1:1/%zz"} {
		if _, err := resolveDownloadTarget(&base, loc); err == nil {
			t.Errorf("resolveDownloadTarget(%q) succeeded, want error", loc)
		}
	}
}

func TestValidateDownloadTargetDowngrade(t *testing.T) {
	httpsOrigin := &url.URL{Scheme: "https", Host: "o:1"}
	if err := validateDownloadTarget(httpsOrigin, &url.URL{Scheme: "http", Host: "any:2"}); err == nil {
		t.Error("https → http redirect accepted, want a downgrade error")
	} else if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %q, want it to mention the downgrade", err.Error())
	}
	// Stable and upgrading schemes are fine.
	if err := validateDownloadTarget(httpsOrigin, &url.URL{Scheme: "https", Host: "any:2"}); err != nil {
		t.Errorf("https → https redirect rejected: %v", err)
	}
	httpOrigin := &url.URL{Scheme: "http", Host: "o:1"}
	if err := validateDownloadTarget(httpOrigin, &url.URL{Scheme: "https", Host: "any:2"}); err != nil {
		t.Errorf("http → https redirect rejected: %v", err)
	}
}

func TestDoDownloadDowngradeRejectedEndToEnd(t *testing.T) {
	ru := &recUpstream{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		// Attempt an https → http downgrade.
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
	}))
	defer origin.Close()

	c, err := New(origin.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	// The self-signed test certificate would otherwise end the request at the
	// transport layer, before the redirect logic runs.
	c.hc.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	if _, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), nil); err == nil {
		t.Fatal("https → http redirect accepted, want error")
	} else if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error = %q, want it to mention the downgrade", err.Error())
	}
	if ru.n() != 1 {
		t.Errorf("upstream requests = %d, want 1 (redirect never followed)", ru.n())
	}
}

// --- token self-heal boundary ---

// dlHealUpstream answers data-plane requests with HTTP 401 unless the request
// carries the permanent settings token, and implements the heal protocol.
type dlHealUpstream struct {
	always401   bool
	logins      dlHealCounters
	mu          sync.Mutex
	dataAuths   []string
	fileContent string
}

func (u *dlHealUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			u.logins.hitLogin()
			fmt.Fprint(w, dlHealLogin)
		case r.URL.Path == "/api/admin/setting/get":
			fmt.Fprint(w, dlHealSetting)
		default:
			auth := r.Header.Get("Authorization")
			u.mu.Lock()
			u.dataAuths = append(u.dataAuths, auth)
			u.mu.Unlock()
			if u.always401 || auth != "openlist-permanent" {
				dlJSON(w, http.StatusUnauthorized, `{"code":401,"message":"token is invalidated","data":null}`)
				return
			}
			fmt.Fprint(w, u.fileContent)
		}
	})
}

func (u *dlHealUpstream) dataRequests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.dataAuths...)
}

func TestDoDownloadInitial401HealsAndRetries(t *testing.T) {
	up := &dlHealUpstream{fileContent: "FILE-CONTENT"}
	ts := httptest.NewServer(up.handler())
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after heal+retry", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); body != "FILE-CONTENT" {
		t.Errorf("body = %q, want the healed download", body)
	}
	if got := c.Token(); got != "openlist-permanent" {
		t.Errorf("token after heal = %q, want the permanent settings token", got)
	}
	if got := up.logins.loginCount(); got != 1 {
		t.Errorf("logins = %d, want 1", got)
	}
	auths := up.dataRequests()
	if len(auths) != 2 || auths[0] != "stale-jwt" || auths[1] != "openlist-permanent" {
		t.Errorf("data-plane auths = %v, want [stale-jwt openlist-permanent]", auths)
	}
}

func TestDoDownloadInitial401RetriedOnlyOnce(t *testing.T) {
	up := &dlHealUpstream{always401: true}
	ts := httptest.NewServer(up.handler())
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the persistent 401 passed through", resp.StatusCode)
	}
	if got := up.logins.loginCount(); got != 1 {
		t.Errorf("logins = %d, want exactly one heal", got)
	}
	if got := len(up.dataRequests()); got != 2 {
		t.Errorf("data-plane requests = %d, want stale attempt + healed retry", got)
	}
}

func TestDoDownload401PassesThroughWithoutCredentials(t *testing.T) {
	up := &dlHealUpstream{}
	ts := httptest.NewServer(up.handler())
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 passed through", resp.StatusCode)
	}
	if got := up.logins.loginCount(); got != 0 {
		t.Errorf("logins = %d, want 0 without credentials", got)
	}
}

func TestDoDownloadDownstream401NeverHeals(t *testing.T) {
	t.Run("same-origin-after-redirect", func(t *testing.T) {
		logins := &dlHealCounters{}
		// /a redirects to /b, and /b rejects — but only after a redirect, so
		// no heal may happen even though the origin is still trusted.
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/auth/login":
				logins.hitLogin()
				fmt.Fprint(w, dlHealLogin)
			case "/api/admin/setting/get":
				fmt.Fprint(w, dlHealSetting)
			case "/a":
				http.Redirect(w, r, "/b", http.StatusFound)
			default:
				dlJSON(w, http.StatusUnauthorized, `{"code":401,"message":"token is invalidated","data":null}`)
			}
		}))
		defer ts.Close()

		c, err := New(ts.URL, "stale-jwt")
		if err != nil {
			t.Fatal(err)
		}
		c.SetCredentials("admin", "pw")

		resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/a"), nil)
		if err != nil {
			t.Fatalf("DoDownload: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 passed through", resp.StatusCode)
		}
		if body := readAll(t, resp.Body); !strings.Contains(body, `"code":401`) {
			t.Errorf("body = %q, want the upstream envelope intact", body)
		}
		if got := logins.loginCount(); got != 0 {
			t.Errorf("logins = %d, want 0 (401 after a redirect never heals)", got)
		}
	})

	t.Run("external-provider-401", func(t *testing.T) {
		logins := &dlHealCounters{}
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login" {
				logins.hitLogin()
				fmt.Fprint(w, dlHealLogin)
				return
			}
			http.Redirect(w, r, "/dl", http.StatusFound) // rewritten below
		}))
		defer origin.Close()

		ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dlJSON(w, http.StatusUnauthorized, `{"code":401,"message":"provider rejected","data":null}`)
		}))
		defer ext.Close()

		external := ext.URL + "/dl"
		origin2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login" {
				logins.hitLogin()
				fmt.Fprint(w, dlHealLogin)
				return
			}
			http.Redirect(w, r, external, http.StatusFound)
		}))
		defer origin2.Close()

		c, err := New(origin2.URL, "stale-jwt")
		if err != nil {
			t.Fatal(err)
		}
		c.SetCredentials("admin", "pw")

		resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/f"), nil)
		if err != nil {
			t.Fatalf("DoDownload: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want the provider 401 passed through", resp.StatusCode)
		}
		if got := logins.loginCount(); got != 0 {
			t.Errorf("logins = %d, want 0 (provider 401 never heals)", got)
		}
	})
}

// --- payload fidelity: the {"code":401} trap ---

func TestDoDownloadJSONFileWithCode401IsNeverHealed(t *testing.T) {
	// The regression case: a downloaded JSON document whose content contains
	// the token-rejection envelope. It must stream through byte-identical,
	// with no peek, no buffering and no bogus re-login.
	payload := `{"note":"` + strings.Repeat("A", 200*1024) + `","code":401,"message":"this is file content"}`
	logins := &dlHealCounters{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			logins.hitLogin()
			fmt.Fprint(w, dlHealLogin)
			return
		}
		dlJSON(w, http.StatusOK, payload)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/files/data.json?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("payload altered: got %d bytes, want %d (byte-identical)", len(got), len(payload))
	}
	if got := logins.loginCount(); got != 0 {
		t.Errorf("logins = %d, want 0 (file content must never trigger a heal)", got)
	}
}

func TestDoDownloadBinaryPayloadUnaltered(t *testing.T) {
	payload := make([]byte, 128*1024)
	for i := range payload {
		payload[i] = byte(i % 251) // includes 0x00 and 0xFF patterns
	}
	logins := &dlHealCounters{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			logins.hitLogin()
			fmt.Fprint(w, dlHealLogin)
			return
		}
		switch r.URL.Path {
		case "/p/bin":
			http.Redirect(w, r, "/p/raw", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		}
	}))
	defer origin.Close()

	c, err := New(origin.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/bin?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("binary payload altered through the redirect: got %d bytes, want %d", len(got), len(payload))
	}
	if got := logins.loginCount(); got != 0 {
		t.Errorf("logins = %d, want 0", got)
	}
}

func TestDoDownloadStreamsUnbuffered(t *testing.T) {
	readFirst := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "chunk-one-")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-readFirst:
		case <-time.After(5 * time.Second):
			// The client did not start reading while the handler was still
			// writing: the response body was buffered, not streamed.
			t.Error("body was buffered: client did not read before the handler finished")
			return
		}
		_, _ = io.WriteString(w, "chunk-two")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/stream"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, len("chunk-one-"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(first) != "chunk-one-" {
		t.Errorf("first chunk = %q", first)
	}
	close(readFirst)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if string(rest) != "chunk-two" {
		t.Errorf("rest = %q, want chunk-two", rest)
	}
}

// --- HEAD and range/conditional handling ---

func TestDoDownloadHeadPreservedAcrossRedirect(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		if r.URL.Path == "/p/head" {
			http.Redirect(w, r, "/p/head2", http.StatusFound)
			return
		}
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoDownload(context.Background(), http.MethodHead, mustURL("/p/head?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); body != "" {
		t.Errorf("HEAD body = %q, want empty", body)
	}
	if ru.n() != 2 {
		t.Fatalf("upstream requests = %d, want 2", ru.n())
	}
	for i := 0; i < 2; i++ {
		if rec := ru.at(i); rec.Method != http.MethodHead {
			t.Errorf("hop %d method = %s, want HEAD preserved", i, rec.Method)
		}
	}
}

func TestDoDownloadRange206(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("upstream Range = %q, want bytes=2-5 forwarded", got)
		}
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "CDEF")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Range", "bytes=2-5")
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), hdr)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); body != "CDEF" {
		t.Errorf("body = %q, want the 206 window", body)
	}
}

func TestDoDownload416PassesThroughWithoutHeal(t *testing.T) {
	logins := &dlHealCounters{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			logins.hitLogin()
			fmt.Fprint(w, dlHealLogin)
			return
		}
		// Even a 416 whose body looks like the token-rejection envelope must
		// pass through untouched.
		dlJSON(w, http.StatusRequestedRangeNotSatisfiable, `{"code":401,"message":"range nonsense","data":null}`)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	hdr := http.Header{}
	hdr.Set("Range", "bytes=999999-")
	resp, err := c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/x?sign=SGN"), hdr)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416 passed through", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); !strings.Contains(body, `"code":401`) {
		t.Errorf("body = %q, want it intact", body)
	}
	if got := logins.loginCount(); got != 0 {
		t.Errorf("logins = %d, want 0 (416 never heals)", got)
	}
}

// --- cancellation and error hygiene ---

func TestDoDownloadPreCanceledContext(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		fmt.Fprint(w, "x")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.DoDownload(ctx, http.MethodGet, mustURL("/p/secret.bin?sign=SGN"), nil)
	if err == nil {
		t.Fatal("DoDownload with a canceled context succeeded, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, err = %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "sign=SGN") || strings.Contains(msg, "/p/secret.bin") {
		t.Errorf("error text %q leaks the signed URL", msg)
	}
	if ru.n() != 0 {
		t.Errorf("upstream requests = %d, want 0", ru.n())
	}
}

func TestDoDownloadCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first-")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "second")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := c.DoDownload(ctx, http.MethodGet, mustURL("/p/x?sign=SGN"), nil)
	if err != nil {
		t.Fatalf("DoDownload: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("first-"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	cancel() // drop the transfer mid-stream
	defer close(release)
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatalf("read after cancellation succeeded (%d bytes), want error", n)
	}
	if msg := err.Error(); strings.Contains(msg, "sign=SGN") {
		t.Errorf("body error text %q leaks the signed URL", msg)
	}
}

func TestDoDownloadTransportErrorHidesSignedURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := ts.URL
	ts.Close() // nothing listens anymore

	c, err := New(dead, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DoDownload(context.Background(), http.MethodGet, mustURL("/p/secret.bin?sign=SGN"), nil)
	if err == nil {
		t.Fatal("DoDownload against a dead upstream succeeded, want error")
	}
	msg := err.Error()
	for _, secret := range []string{"sign=SGN", "/p/secret.bin", "127.0.0.1", dead} {
		if strings.Contains(msg, secret) {
			t.Errorf("transport error text %q leaks %q", msg, secret)
		}
	}
	// The wrapper message is the fixed generic text; the underlying *url.Error
	// (with the signed URL) stays reachable only through Unwrap.
	if msg != "openlist download: upstream request failed" {
		t.Errorf("transport error text = %q, want the fixed generic message", msg)
	}
}

func TestDoDownloadNoHopHeaderWithoutContext(t *testing.T) {
	ru := &recUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru.record(r)
		fmt.Fprint(w, "x")
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DoDownload(WithHop(context.Background(), 0), http.MethodGet, mustURL("/p/x"), nil); err != nil {
		t.Fatal(err)
	}
	if got := ru.at(0).Header.Get(hopHeader); got != "" {
		t.Errorf("X-GoAPI-Hop = %q, want none for hop 0", got)
	}
}
