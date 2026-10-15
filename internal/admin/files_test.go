package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilesWorkspaceServing(t *testing.T) {
	var delegated []string
	handler := (&Portal{}).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delegated = append(delegated, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	for _, target := range []string{"/files", "/files/", "/files/?mock=1"} {
		t.Run(target, func(t *testing.T) {
			rec := doPortal(t, handler, http.MethodGet, target, "", nil)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "我的网盘") {
				t.Fatalf("workspace = %d (%s)", rec.Code, rec.Body.String())
			}
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Fatal("workspace must be HTML")
			}
			for key, want := range map[string]string{
				"Cache-Control": "no-store", "Content-Security-Policy": relayCSP,
				"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
				"Cross-Origin-Resource-Policy": "same-origin",
			} {
				if got := rec.Header().Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Fatal("workspace must not issue a session cookie")
			}
			for _, forbidden := range []string{"<style", "onclick=", "onsubmit=", "<script>", "/admin/api/", "localStorage", "sessionStorage"} {
				if strings.Contains(rec.Body.String(), forbidden) {
					t.Errorf("workspace shell includes %q", forbidden)
				}
			}
		})
	}
	for name, contentType := range map[string]string{
		"files.js": "javascript", "files.css": "text/css", "app.css": "text/css", "logo.png": "image/png",
	} {
		rec := doPortal(t, handler, http.MethodGet, "/files/assets/"+name, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), contentType) {
			t.Fatalf("%s = %d ct=%s", name, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Security-Policy") != relayCSP {
			t.Errorf("asset %s is missing security headers", name)
		}
	}
	for target, status := range map[string]int{
		"/files/other": 404, "/files/assets/config.yaml": 404,
		"/files/assets/api.js": 404, "/files/assets/mock.js": 404,
		"/files/assets/../app.html": 400, "/files/assets/%2e%2e%2fapp.html": 400,
		"/files/assets/sub/files.js": 400, "/files/assets/%5cfiles.js": 400,
		"/files/assets/%00files.js": 400,
	} {
		rec := doPortal(t, handler, http.MethodGet, target, "", nil)
		if rec.Code != status {
			t.Errorf("%s = %d, want %d", target, rec.Code, status)
		}
	}
	for _, target := range []string{"/files", "/files/", "/files/assets/files.js"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
			rec := doPortal(t, handler, method, target, "", nil)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
				t.Errorf("%s %s = %d allow=%q", method, target, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	if len(delegated) != 0 {
		t.Fatalf("workspace requests escaped static handler: %v", delegated)
	}
	for _, target := range []string{"/v1/me", "/v1/drives/test/files", "/l/test", "/downloadFile", "/files-other"} {
		rec := doPortal(t, handler, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusTeapot {
			t.Errorf("data-plane request %s intercepted by workspace", target)
		}
	}
}

func TestFilesWorkspaceRootAndAuthSeparation(t *testing.T) {
	h := newPortalHarness(t)
	rec := doPortal(t, h.handler, http.MethodGet, "/?apikey=must-not-be-forwarded", "", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/files/" {
		t.Fatalf("root = %d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("root redirect issued a session")
	}
	if rec := doPortal(t, h.handler, http.MethodPost, "/", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST root = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.Header.Set("apikey", dataPlaneKey)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API key authenticated admin session: %d", rec.Code)
	}
	cookie := h.login(t, adminPassword)
	rec = doPortal(t, h.handler, http.MethodGet, "/files/", cookie, nil)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("admin cookie changed workspace auth: %d", rec.Code)
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/api/storage/static1/list", cookie, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin cookie authenticated data plane: %d", rec.Code)
	}
}
