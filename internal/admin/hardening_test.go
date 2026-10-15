package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// adminReq builds an admin API request with optional headers. A JSON content
// type is set when a body is present, matching real portal callers.
func adminReq(method, target string, body []byte, hdr map[string]string) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

// TestCrossOriginMutatingRejected covers the v0.3.1 CSRF rules (UPGRADE
// §5.3): mutating /admin/api/* requests with a foreign Origin, or with a
// cross-site Sec-Fetch-Site and no Origin, answer 403 before any auth or
// business logic runs.
func TestCrossOriginMutatingRejected(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"foreign origin", map[string]string{"Origin": "http://evil.example"}},
		{"origin other port", map[string]string{"Origin": "http://example.com:9999"}},
		{"origin https different host", map[string]string{"Origin": "https://bad.example"}},
		{"origin null", map[string]string{"Origin": "null"}},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site fetch metadata", map[string]string{"Sec-Fetch-Site": "same-site"}},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			req := adminReq(method, "/admin/api/accounts", []byte(`{"id":"x"}`), tc.hdr)
			req.Header.Set("Cookie", cookie)
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s (%s): status = %d, want 403", method, "/admin/api/accounts", tc.name, rec.Code)
			}
		}
	}

	// Unauthenticated cross-origin probes are rejected too (pre-auth check).
	req := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"x"}`), map[string]string{"Origin": "http://evil.example"})
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated cross-origin login: status = %d, want 403", rec.Code)
	}

	// Non-mutating requests are never blocked by the origin check.
	req = adminReq(http.MethodGet, "/admin/api/accounts", nil, map[string]string{"Origin": "http://evil.example"})
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET with foreign origin: status = %d, want 200 (GET is not mutating)", rec.Code)
	}
}

// TestSameOriginMutatingAccepted: same-origin Origin headers and benign
// fetch metadata pass the guard and reach normal handling.
func TestSameOriginMutatingAccepted(t *testing.T) {
	h := newPortalHarness(t)

	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"same origin http", map[string]string{"Origin": "http://example.com"}},
		{"same origin https", map[string]string{"Origin": "https://example.com"}},
		{"fetch same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"fetch none (direct nav)", map[string]string{"Sec-Fetch-Site": "none"}},
		{"no headers at all", nil},
	}
	for _, tc := range cases {
		req := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"wrong"}`), tc.hdr)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		// Wrong password on purpose: anything except 403/429/415 proves the
		// request passed the origin guard and reached the login logic.
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: login status = %d, want 401 (guard must pass)", tc.name, rec.Code)
		}
	}
}

// TestMutatingBodyRequiresJSONContentType covers the v0.3.1 content-type
// rule for mutating admin API requests carrying a body.
func TestMutatingBodyRequiresJSONContentType(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	req := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"x"}`), nil)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain login: status = %d, want 415", rec.Code)
	}

	req = adminReq(http.MethodPost, "/admin/api/accounts", []byte(`{}`), map[string]string{"Origin": "http://example.com"})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded accounts POST: status = %d, want 415", rec.Code)
	}

	// charset parameters are fine.
	req = adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"wrong"}`), nil)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("application/json; charset: status = %d, want 401 (must pass CT check)", rec.Code)
	}

	// Bodyless mutating requests need no content type at all.
	req = adminReq(http.MethodPost, "/admin/api/accounts/ghost/disable", nil, nil)
	req.Header.Set("Cookie", cookie)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Error("bodyless POST must not require a content type")
	}
}

// TestLoginRateLimit covers the v0.3.1 brute-force throttle: 5 failed
// logins per client IP per minute, then 429 + Retry-After. A successful
// login resets the counter; other IPs are unaffected.
func TestLoginRateLimit(t *testing.T) {
	h := newPortalHarness(t)

	wrongLogin := func(ip string) int {
		req := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"wrong"}`), nil)
		req.RemoteAddr = ip + ":1111"
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// 5 failures pass through as 401...
	for i := 1; i <= maxLoginFailures; i++ {
		if c := wrongLogin("10.0.0.1"); c != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i, c)
		}
	}
	// ...the 6th attempt is throttled with Retry-After.
	req := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"wrong"}`), nil)
	req.RemoteAddr = "10.0.0.1:1111"
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 response missing Retry-After")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After = %q, want 1..60 seconds", ra)
	}

	// Even the correct password is throttled once blocked.
	good := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"`+adminPassword+`"}`), nil)
	good.RemoteAddr = "10.0.0.1:1111"
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, good)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while blocked: status = %d, want 429", rec.Code)
	}

	// A different client IP is unaffected.
	if c := wrongLogin("10.0.0.2"); c != http.StatusUnauthorized {
		t.Errorf("other IP hit the limit: status = %d, want 401", c)
	}

	// A success resets the counter for its IP.
	if c := wrongLogin("10.0.0.3"); c != http.StatusUnauthorized {
		t.Fatal("setup failure")
	}
	suc := adminReq(http.MethodPost, "/admin/api/login", []byte(`{"password":"`+adminPassword+`"}`), nil)
	suc.RemoteAddr = "10.0.0.3:1111"
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, suc)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after one failure: status = %d, want 200", rec.Code)
	}
	// maxLoginFailures-1 further failures stay at 401 (counter restarted).
	for i := 1; i < maxLoginFailures; i++ {
		if c := wrongLogin("10.0.0.3"); c != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status = %d, want 401", i, c)
		}
	}
}
