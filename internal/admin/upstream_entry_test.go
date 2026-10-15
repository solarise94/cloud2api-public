package admin

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminLoginDoesNotOfferLocalUploadLogin(t *testing.T) {
	h := newPortalHarness(t)
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/login", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d", rec.Code)
	}
	for _, wrong := range []string{"API 中转登录", "href=\"/relay/\"", "href='/relay/'"} {
		if strings.Contains(rec.Body.String(), wrong) {
			t.Fatalf("administrator login still offers the misplaced upload-tool login: %q", wrong)
		}
	}
}

func TestLocalUploadToolIsNotAnUpstreamStorageForm(t *testing.T) {
	h := newPortalHarness(t)
	rec := doPortal(t, h.handler, http.MethodGet, "/relay/", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "中转上传工具") {
		t.Fatalf("local upload tool must have a distinct title: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "API 中转登录") {
		t.Fatal("local upload tool still claims to be the upstream-storage login")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("local upload tool issued an administrator session")
	}
}
