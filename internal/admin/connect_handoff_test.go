package admin

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud2api/internal/openlist"
)

const portalFakeHandoffCode = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func newHandoffHarness(t *testing.T, handoff bool, response string) (*portalHarness, *atomic.Int32) {
	t.Helper()
	h := newPortalHarness(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/admin/connect/handoff/start" || r.Header.Get("Authorization") != "synthetic-backend-only-token" {
			t.Errorf("unexpected management handoff request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	t.Cleanup(ts.Close)
	client, err := openlist.New(ts.URL, "synthetic-backend-only-token")
	if err != nil {
		t.Fatal(err)
	}
	h.p.client = client
	h.p.connectHas, h.p.connectOK, h.p.connectAt = true, true, time.Now()
	h.p.connectInfo = openlist.ConnectInfo{Version: "4", Handoff: handoff, Platforms: []openlist.ConnectPlatform{{ID: "goapi", Driver: "GoAPI"}}}
	setPublicURL(t, h, "http://192.0.2.10:5244/prefix")
	return h, &calls
}

func handoffRequest(h *portalHarness, method, cookie, origin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://gateway.example.com/admin/api/connect/handoff", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func TestPortalConnectHandoff(t *testing.T) {
	h, calls := newHandoffHarness(t, true, fmt.Sprintf(`{"code":200,"data":{"code":%q,"expires_in":60}}`, portalFakeHandoffCode))
	cookie := h.login(t, adminPassword)
	rec := handoffRequest(h, http.MethodPost, cookie, "http://gateway.example.com", `{"target":"/new/goapi"}`)
	if rec.Code != 200 || calls.Load() != 1 {
		t.Fatalf("handoff = %d %s calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
	data := dataOf(t, decodePortal(t, rec))
	link, err := url.Parse(data["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if link.Scheme != "http" || link.Host != "192.0.2.10:5244" || link.Path != "/prefix/@connect/" {
		t.Errorf("handoff target is not the configured OpenList page: %s", link)
	}
	if link.Query().Get("return") != "http://gateway.example.com/admin/connect-return?storage_id={storage_id}" {
		t.Errorf("return URL is not restricted to requesting portal: %s", link.Query().Get("return"))
	}
	fragment, err := url.ParseQuery(link.EscapedFragment())
	if err != nil || fragment.Get("handoff") != portalFakeHandoffCode || fragment.Get("target") != "/new/goapi" {
		t.Errorf("browser fragment does not decode once to the target: %s", link.EscapedFragment())
	}
	if strings.Contains(link.EscapedFragment(), "%252F") {
		t.Error("target separators were escaped twice")
	}
	if strings.Contains(link.RawQuery, portalFakeHandoffCode) || strings.Contains(rec.Body.String(), "synthetic-backend-only-token") || strings.Contains(rec.Body.String(), adminPassword) {
		t.Error("handoff exposed long-lived credentials or a ticket in the query")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || len(rec.Result().Cookies()) != 0 {
		t.Error("portal handoff must be uncacheable and must not issue OpenList cookies")
	}
	if h.fake.writeCallCount() != 0 {
		t.Fatal("portal handoff wrote storage config")
	}
}

func TestPortalHandoffBrowserFragmentTargets(t *testing.T) {
	for _, target := range []string{"/", "/new/goapi", "/app/goapi", "/reauth/7", "/reauth/9223372036854775807"} {
		t.Run(target, func(t *testing.T) {
			h, _ := newHandoffHarness(t, true, fmt.Sprintf(`{"code":200,"data":{"code":%q,"expires_in":60}}`, portalFakeHandoffCode))
			rec := handoffRequest(h, http.MethodPost, h.login(t, adminPassword), "http://gateway.example.com", fmt.Sprintf(`{"target":%q}`, target))
			link, err := url.Parse(dataOf(t, decodePortal(t, rec))["url"].(string))
			if err != nil {
				t.Fatal(err)
			}
			params, err := url.ParseQuery(link.EscapedFragment())
			if err != nil || params.Get("target") != target || params.Get("handoff") != portalFakeHandoffCode {
				t.Fatalf("serialized browser fragment = %q: %v", link.EscapedFragment(), err)
			}
		})
	}
}

func TestPortalHandoffRejectsUnauthorizedOriginAndTargets(t *testing.T) {
	for _, tc := range []struct {
		name, origin, body, method string
		logged                     bool
		want                       int
	}{
		{"unauthorized", "http://gateway.example.com", `{}`, "POST", false, 401},
		{"missingOrigin", "", `{}`, "POST", true, 403},
		{"nullOrigin", "null", `{}`, "POST", true, 403},
		{"crossOrigin", "http://evil.example.com", `{}`, "POST", true, 403},
		{"pathOrigin", "http://gateway.example.com/path", `{}`, "POST", true, 403},
		{"userinfoOrigin", "http://user@gateway.example.com", `{}`, "POST", true, 403},
		{"unknownPlatform", "http://gateway.example.com", `{"target":"/new/missing"}`, "POST", true, 400},
		{"URLTarget", "http://gateway.example.com", `{"target":"https://evil.example.com"}`, "POST", true, 400},
		{"traversalTarget", "http://gateway.example.com", `{"target":"/new/goapi/../app/goapi"}`, "POST", true, 400},
		{"badStorageID", "http://gateway.example.com", `{"target":"/reauth/0"}`, "POST", true, 400},
		{"returnOverride", "http://gateway.example.com", `{"target":"/","return":"http://evil.example.com"}`, "POST", true, 400},
		{"credentialFields", "http://gateway.example.com", `{"target":"/","api_key":"synthetic-upstream-key"}`, "POST", true, 400},
		{"extraJSON", "http://gateway.example.com", `{} {}`, "POST", true, 400},
		{"wrongMethod", "http://gateway.example.com", `{}`, "GET", true, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := newHandoffHarness(t, true, `{"code":200}`)
			cookie := ""
			if tc.logged {
				cookie = h.login(t, adminPassword)
			}
			rec := handoffRequest(h, tc.method, cookie, tc.origin, tc.body)
			if rec.Code != tc.want || calls.Load() != 0 {
				t.Fatalf("status=%d want=%d upstreamCalls=%d body=%s", rec.Code, tc.want, calls.Load(), rec.Body.String())
			}
		})
	}
}

func TestPortalHandoffLegacyFallbackAndFailure(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		h, calls := newHandoffHarness(t, false, "{}")
		rec := handoffRequest(h, "POST", h.login(t, adminPassword), "http://gateway.example.com", `{"target":"/reauth/7"}`)
		data := dataOf(t, decodePortal(t, rec))
		if rec.Code != 200 || data["url"] != "" || !strings.HasSuffix(data["fallback_url"].(string), "#/reauth/7") || calls.Load() != 0 {
			t.Fatalf("legacy fallback mismatch: %d %s calls=%d", rec.Code, rec.Body.String(), calls.Load())
		}
	})
	t.Run("failure", func(t *testing.T) {
		h, calls := newHandoffHarness(t, true, `{"code":500,"message":"synthetic-backend-only-token password"}`)
		rec := handoffRequest(h, "POST", h.login(t, adminPassword), "http://gateway.example.com", `{}`)
		if rec.Code != 502 || calls.Load() != 1 || strings.Contains(rec.Body.String(), "synthetic-backend-only-token") || !bytes.Contains(rec.Body.Bytes(), []byte("fallback_url")) {
			t.Fatalf("failed mint must be honest with a safe fallback: %d %s calls=%d", rec.Code, rec.Body.String(), calls.Load())
		}
	})
	t.Run("unset", func(t *testing.T) {
		h, calls := newHandoffHarness(t, true, "{}")
		setPublicURL(t, h, "")
		rec := handoffRequest(h, "POST", h.login(t, adminPassword), "http://gateway.example.com", `{}`)
		if rec.Code != 503 || calls.Load() != 0 {
			t.Fatalf("unset URL minted ticket: %d calls=%d", rec.Code, calls.Load())
		}
	})
}

func TestValidConnectTarget(t *testing.T) {
	info := openlist.ConnectInfo{Platforms: []openlist.ConnectPlatform{{ID: "goapi"}}}
	for _, target := range []string{"/", "/new/goapi", "/app/goapi", "/reauth/1"} {
		if !validConnectTarget(target, info) {
			t.Errorf("rejected valid target %q", target)
		}
	}
	for _, target := range []string{"", "//evil.example.com", "/reauth/+1", "/reauth/01", "/reauth/-1", "/new/goapi?x=1", "/new/goapi#foo", "/s/secret"} {
		if validConnectTarget(target, info) {
			t.Errorf("accepted invalid target %q", target)
		}
	}
}
