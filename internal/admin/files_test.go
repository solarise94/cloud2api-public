package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	// Root serves the public navigation page (web/home.html, frontend-owned)
	// with zero cookies and never forwards query secrets anywhere. Until the
	// frontend ships home.html the route answers 404 — still no redirect and
	// no cookie.
	rec := doPortal(t, h.handler, http.MethodGet, "/?apikey=must-not-be-forwarded", "", nil)
	if len(h.p.homePage) == 0 {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("root without home.html = %d, want 404", rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Fatalf("root redirected instead of serving navigation: %q", rec.Header().Get("Location"))
		}
	} else {
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("root = %d, want 200 navigation HTML", rec.Code)
		}
		if rec.Body.String() != string(h.p.homePage) {
			t.Fatal("root did not serve the navigation page verbatim")
		}
		for key, want := range map[string]string{
			"Cache-Control": "no-store", "Content-Security-Policy": homeCSP,
			"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := rec.Header().Get(key); got != want {
				t.Errorf("root %s = %q, want %q", key, got, want)
			}
		}
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("root navigation issued a cookie")
	}
	if rec := doPortal(t, h.handler, http.MethodPost, "/", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST root = %d", rec.Code)
	}

	// Three-way auth separation (admin / files workspace / machine data plane).
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

	// Fail closed: with the workspace handler not installed, /files API
	// requests never fall through to the data plane.
	entry := strings.Repeat("aB9", 8)[:22]
	rec = doPortal(t, h.handler, http.MethodGet, "/files/w/"+entry+"/api/v1/me", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("uninstalled workspace API = %d, want 503 fail closed", rec.Code)
	}
}

func TestFilesWorkspaceEntryRoute(t *testing.T) {
	h := newPortalHarness(t)
	valid := strings.Repeat("aB9", 8)[:22] // base64url of 128 bit
	for _, target := range []string{"/files/w/" + valid + "/", "/files/w/" + valid} {
		rec := doPortal(t, h.handler, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("entry page %s = %d", target, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("entry page %s issued a cookie", target)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Security-Policy") != relayCSP {
			t.Errorf("entry page %s is missing security headers", target)
		}
	}
	for _, target := range []string{
		"/files/w/", "/files/w", "/files/w//", // no or empty id
		"/files/w/short-id/",                        // below 128 bit
		"/files/w/" + strings.Repeat("a", 65) + "/", // above the length cap
		"/files/w/" + valid + "/extra/",             // no static subpaths
		"/files/w/../../etc/passwd/",                // traversal is not an id
		"/files/w/" + valid + ".mal/",               // dots are not in the alphabet
	} {
		rec := doPortal(t, h.handler, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
}

// captureHandler records delegated workspace API requests.
type captureHandler struct {
	mu    sync.Mutex
	paths []string
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
	w.WriteHeader(http.StatusTeapot)
}

func (c *captureHandler) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...)
}

func TestFilesAPIDelegationAndFailClosed(t *testing.T) {
	entry := strings.Repeat("xY8", 8)[:22]
	apiTargets := []string{
		"/files/api/login", "/files/api/workspaces",
		"/files/w/" + entry + "/api/v1/me", "/files/w/" + entry + "/api/logout",
		"/files/w/" + entry + "/api", "/files/w/" + entry + "/api/",
	}

	// Without the handler every workspace API request fails closed and never
	// reaches the data plane behind Wrap.
	h := newPortalHarness(t)
	for _, target := range apiTargets {
		rec := doPortal(t, h.handler, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("uninstalled handler %s = %d, want 503", target, rec.Code)
		}
	}

	// With the handler installed the API paths are delegated verbatim; the
	// static shells stay with the portal.
	marker := &captureHandler{}
	p := &Portal{}
	p.SetWorkspaceHandler(marker)
	handler := p.Wrap(http.NotFoundHandler())
	for _, target := range apiTargets {
		rec := doPortal(t, handler, http.MethodPost, target, "", nil)
		if rec.Code != http.StatusTeapot {
			t.Errorf("delegated %s = %d, want teapot", target, rec.Code)
		}
	}
	want := append([]string(nil), apiTargets...)
	got := marker.recorded()
	if len(got) != len(want) {
		t.Fatalf("delegated paths = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delegated path %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Static surface is never delegated and stays zero-cookie.
	rec := doPortal(t, handler, http.MethodGet, "/files/", "", nil)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("static shell delegated or cookie-issuing: %d", rec.Code)
	}
	rec = doPortal(t, handler, http.MethodGet, "/files/w/"+entry+"/", "", nil)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("entry shell delegated or cookie-issuing: %d", rec.Code)
	}
	for _, target := range []string{"/files/assets/files.js", "/files/assets/api.js", "/files/other", "/files/w/"} {
		rec := doPortal(t, handler, http.MethodGet, target, "", nil)
		wantStatus := http.StatusNotFound
		if target == "/files/assets/files.js" {
			wantStatus = http.StatusOK
		}
		if rec.Code != wantStatus {
			t.Errorf("static %s = %d, want %d", target, rec.Code, wantStatus)
		}
	}
	if got = marker.recorded(); len(got) != len(apiTargets) {
		t.Fatalf("static requests escaped to the workspace handler: %v", got)
	}
	// Static pages remain GET-only even when a handler is installed.
	rec = doPortal(t, handler, http.MethodPost, "/files/w/"+entry+"/", "", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST entry shell = %d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}
