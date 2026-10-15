package admin

import (
	"net/http"
	"strings"
	"testing"
)

func TestConnectReturnWithoutSession(t *testing.T) {
	h := newPortalHarness(t)
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/connect-return?storage_id=7", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "connect-return.js") || rec.Header().Get("Location") != "" {
		t.Fatalf("return landing needs an unauthenticated portal-origin document, not an HTTP redirect: %d %s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Security-Policy") != relayCSP {
		t.Fatal("return landing changed authentication or was not hardened")
	}
	cancelled := doPortal(t, h.handler, http.MethodGet, "/admin/connect-return", "", nil)
	if cancelled.Code != 200 || len(cancelled.Result().Cookies()) != 0 || cancelled.Header().Get("Location") != "" {
		t.Fatal("cancelled connect must return through an unauthenticated same-origin document")
	}
	asset := doPortal(t, h.handler, http.MethodGet, "/admin/assets/connect-return.js", "", nil)
	if asset.Code != 200 || !strings.Contains(asset.Body.String(), "location.replace('/admin/#/drives?connected='") {
		t.Fatal("return landing does not navigate to the fixed same-origin drive route")
	}
	if h.fake.writeCallCount() != 0 {
		t.Fatal("return landing mutated OpenList")
	}
}

func TestConnectReturnRejectsUntrustedTargets(t *testing.T) {
	h := newPortalHarness(t)
	for _, query := range []string{"storage_id=", "storage_id=0", "storage_id=-1", "storage_id=01", "storage_id=9223372036854775808", "storage_id=7&storage_id=8", "storage_id=7&return=https://evil.example.com", "storage_id=%3Cscript%3E"} {
		rec := doPortal(t, h.handler, http.MethodGet, "/admin/connect-return?"+query, "", nil)
		if rec.Code != 400 || rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("unsafe return query %q accepted: %d", query, rec.Code)
		}
	}
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/connect-return?storage_id=7", "", nil)
	if rec.Code != 405 {
		t.Fatalf("return POST = %d, want 405", rec.Code)
	}
}
