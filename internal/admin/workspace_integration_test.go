package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/agentapi"
	"cloud2api/internal/auth"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/store"
)

func TestPersistentWorkspaceIntegration(t *testing.T) {
	h := newPortalHarness(t)
	if err := h.st.InsertKey(store.APIKey{
		ID: "workspace-second", Hash: auth.HashKey("synthetic-workspace-second"),
		CreatedAt: time.Now().UTC(),
		Drives:    []store.KeyDrive{{Drive: "static1", Root: "/second", Perms: []string{"list", "read"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	build := func(st *store.Store, keys *keymgr.Manager) http.Handler {
		api := agentapi.New(keys, st, h.p.client, h.registry, nil)
		p := NewPortal(&h.p.cfg, h.registry, h.p.client, st, keys, nil)
		p.SetWorkspaceHandler(api.WorkspaceHandler())
		return p.Wrap(api.Handler(http.NotFoundHandler()))
	}
	var mu sync.RWMutex
	current := build(h.st, h.keys)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		current.ServeHTTP(w, r)
	}))
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	anon := &http.Client{CheckRedirect: browser.CheckRedirect}
	do := func(client *http.Client, method, path string, body any, workspaceHeader bool) (int, []byte, http.Header) {
		t.Helper()
		var data []byte
		if body != nil {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Origin", ts.URL)
		if workspaceHeader {
			req.Header.Set("X-C2A-Workspace", "1")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out, res.Header
	}
	login := func(key string) string {
		t.Helper()
		status, data, headers := do(browser, "POST", "/files/api/login", map[string]string{"api_key": key}, true)
		if status != 200 {
			t.Fatalf("workspace login = %d: %s", status, data)
		}
		var v struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(data, &v); err != nil || !strings.HasPrefix(v.URL, "/files/w/") || !strings.HasSuffix(v.URL, "/") {
			t.Fatalf("invalid workspace login response: %s", data)
		}
		cookie := headers.Get("Set-Cookie")
		if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") || strings.Contains(cookie, key) {
			t.Fatalf("workspace cookie flags or credential leak: flags missing=%v", cookie == "")
		}
		return v.URL
	}
	status, _, _ := do(browser, "POST", "/admin/api/login", map[string]string{"password": adminPassword}, false)
	if status != 200 {
		t.Fatalf("admin login = %d", status)
	}
	first := login(dataPlaneKey)
	second := login("synthetic-workspace-second")
	if first == second {
		t.Fatal("different Keys received the same workspace")
	}
	for _, workspace := range []string{first, second} {
		if status, body, _ := do(browser, "GET", workspace+"api/v1/me", nil, true); status != 200 {
			t.Fatalf("logged-in workspace me = %d: %s", status, body)
		}
		if status, _, _ := do(anon, "GET", workspace+"api/v1/me", nil, true); status != 401 {
			t.Fatalf("bookmark authenticated anonymous browser: %d", status)
		}
	}
	if status, _, _ := do(browser, "GET", "/v1/me", nil, true); status != 401 {
		t.Fatalf("browser cookies authenticated machine API: %d", status)
	}
	if status, _, _ := do(browser, "GET", first+"api/v1/me", nil, false); status != 403 {
		t.Fatalf("missing workspace anti-CSRF header = %d", status)
	}

	mu.Lock()
	dbPath := h.st.Path()
	if err := h.st.Close(); err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	reopened, err := store.Open(dbPath)
	if err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	defer reopened.Close()
	keys, err := keymgr.New(reopened)
	if err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	current = build(reopened, keys)
	mu.Unlock()
	if status, body, _ := do(browser, "GET", "/admin/api/overview", nil, false); status != 200 {
		t.Fatalf("administrator session did not survive store reopen: %d %s", status, body)
	}
	for _, workspace := range []string{first, second} {
		if status, body, _ := do(browser, "GET", workspace+"api/v1/me", nil, true); status != 200 {
			t.Fatalf("workspace did not survive store reopen: %d %s", status, body)
		}
	}
	if status, _, _ := do(browser, "POST", first+"api/logout", map[string]any{}, true); status != 200 && status != 204 {
		t.Fatalf("logout = %d", status)
	}
	if status, _, _ := do(browser, "GET", first+"api/v1/me", nil, true); status != 401 {
		t.Fatalf("workspace logout did not revoke membership: %d", status)
	}
	if status, _, _ := do(browser, "GET", second+"api/v1/me", nil, true); status != 200 {
		t.Fatalf("logging out first workspace logged out second: %d", status)
	}
	if status, _, _ := do(browser, "GET", "/admin/api/overview", nil, false); status != 200 {
		t.Fatalf("file logout revoked administrator session: %d", status)
	}
	if _, _, err := keys.Rotate("workspace-second"); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := do(browser, "GET", second+"api/v1/me", nil, true); status != 401 {
		t.Fatalf("Key rotation did not invalidate persistent session: %d", status)
	}
}
