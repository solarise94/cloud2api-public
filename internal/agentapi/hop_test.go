package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The relay hop guard is a key-independent, cheap rejection at the /v1 and
// Gokapi ingress points, and the validated count must reach the outbound
// OpenList calls (openlist client), where the peer's GoAPI driver reads it.

func TestHopIngressV1(t *testing.T) {
	h := newHarness(t)
	do := func(headerValues ...string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		for _, v := range headerValues {
			req.Header.Add(HopHeader, v)
		}
		req.Header.Set("apikey", legacyPlain)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		code := ""
		if e, ok := body["error"].(map[string]any); ok {
			code, _ = e["code"].(string)
		}
		return rec.Code, code
	}

	if status, code := do(); status != http.StatusOK || code != "" {
		t.Fatalf("no header must pass: status=%d code=%q", status, code)
	}
	if status, _ := do("0"); status != http.StatusOK {
		t.Fatalf("hop 0 must pass, got %d", status)
	}
	if status, _ := do("4"); status != http.StatusOK {
		t.Fatalf("hop at the cap must pass, got %d", status)
	}
	if status, code := do("5"); status != http.StatusForbidden || code != CodeHopExceeded {
		t.Fatalf("hop above the cap must 403 %s, got %d %q", CodeHopExceeded, status, code)
	}
	if status, code := do("99"); status != http.StatusForbidden || code != CodeHopExceeded {
		t.Fatalf("far above the cap must 403, got %d %q", status, code)
	}
	if status, code := do("x"); status != http.StatusBadRequest || code != CodeInvalidRequest {
		t.Fatalf("malformed hop must 400, got %d %q", status, code)
	}
	if status, code := do("-1"); status != http.StatusBadRequest || code != CodeInvalidRequest {
		t.Fatalf("negative hop must 400, got %d %q", status, code)
	}
	if status, code := do("1", "2"); status != http.StatusBadRequest || code != CodeInvalidRequest {
		t.Fatalf("repeated hop headers must 400, got %d %q", status, code)
	}
	// The guard runs before authentication: no key at all, malformed hop.
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Add(HopHeader, "zz")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed hop without a key must still 400, got %d", rec.Code)
	}
}

func TestHopIngressGokapiLayer(t *testing.T) {
	h := newHarness(t)
	do := func(headerValues ...string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/info/version", nil)
		for _, v := range headerValues {
			req.Header.Add(HopHeader, v)
		}
		req.Header.Set("apikey", legacyPlain)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do(); got != http.StatusOK {
		t.Fatalf("no header must pass, got %d", got)
	}
	if got := do("2"); got != http.StatusOK {
		t.Fatalf("valid hop must pass, got %d", got)
	}
	if got := do("oops"); got != http.StatusBadRequest {
		t.Fatalf("malformed hop must 400, got %d", got)
	}
	if got := do("5"); got != http.StatusForbidden {
		t.Fatalf("hop above the cap must 403, got %d", got)
	}
	if got := do("1", "1"); got != http.StatusBadRequest {
		t.Fatalf("repeated hop headers must 400, got %d", got)
	}
	// The Gokapi layer answers in its own Message shape.
	req := httptest.NewRequest(http.MethodGet, "/api/info/version", nil)
	req.Header.Add(HopHeader, "nope")
	req.Header.Set("apikey", legacyPlain)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Message") {
		t.Fatalf("gokapi-shaped error expected, got %s", rec.Body.String())
	}
}

// TestHopReachesOpenList proves the propagation over the wire: with a valid
// hop header the fs calls the data plane makes to OpenList carry
// X-GoAPI-Hop+0 (the driver adds one on its own outbound), and without one
// they carry nothing.
func TestHopReachesOpenList(t *testing.T) {
	h := newHarness(t)
	h.fake.entries["/m1/x"] = &fakeEntry{name: "x"}
	req := httptest.NewRequest(http.MethodGet, "/v1/drives/d1/stat?path=%2Fx", nil)
	req.Header.Set("apikey", legacyPlain)
	req.Header.Add(HopHeader, "3")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stat with hop must pass: %d %s", rec.Code, rec.Body.String())
	}
	var seen []string
	for _, c := range h.fake.fsHeaders {
		if v := c.Get(HopHeader); v != "" {
			seen = append(seen, v)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the hop count must be forwarded to OpenList")
	}
	for _, v := range seen {
		if v != "3" {
			t.Fatalf("OpenList must receive the inbound hop unchanged (driver adds one), got %q", v)
		}
	}

	// Without the header, nothing is forwarded.
	h.fake.fsHeaders = nil
	req = httptest.NewRequest(http.MethodGet, "/v1/drives/d1/stat?path=%2Fx", nil)
	req.Header.Set("apikey", legacyPlain)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stat without hop must pass: %d", rec.Code)
	}
	for _, c := range h.fake.fsHeaders {
		if v := c.Get(HopHeader); v != "" {
			t.Fatalf("no hop header must reach OpenList, got %q", v)
		}
	}
}

func TestMeAdvertisesRelayHopLimit(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("apikey", legacyPlain)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/me: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RelayHopLimit int `json:"relay_hop_limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if body.RelayHopLimit != hopLimit {
		t.Fatalf("relay_hop_limit = %d, want %d", body.RelayHopLimit, hopLimit)
	}
}
