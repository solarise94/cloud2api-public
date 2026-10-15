package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// --- stateful fake OpenList admin API ---

type fakeStorage struct {
	ID              int64  `json:"id"`
	MountPath       string `json:"mount_path"`
	Driver          string `json:"driver"`
	Status          string `json:"status"`
	Disabled        bool   `json:"disabled"`
	Addition        string `json:"addition"`
	CacheExpiration any    `json:"cache_expiration"`
	Order           any    `json:"order"`
	Remark          any    `json:"remark"`
	Modified        string `json:"modified"`
}

func (s fakeStorage) Enabled() bool { return !s.Disabled }

type fakeOpenList struct {
	mu       sync.Mutex
	storages []fakeStorage
	nextID   int64
	failList bool
	// perPage, when > 0, forces the storage list page size regardless of the
	// requested per_page (tests prove the client really paginates).
	perPage int
	// listCalls counts /api/admin/storage/list requests.
	listCalls int
	// writeCalls counts every storage configuration write request served:
	// storage/create|update|delete|enable|disable. Step 2 requires this to
	// stay 0 across ALL portal operations (IMPLEMENTATION 验收第二步 1), so
	// the fake keeps serving the endpoints — only counting them.
	writeCalls int
	// connectPlatforms, when non-nil, answers /api/admin/connect/platforms as
	// the patched OpenList's login module (envelope + platform list); nil
	// answers what an unpatched OpenList serves for the unknown API path: the
	// SPA index.html (HTTP 200, text/html) — "module not available".
	connectPlatforms []fakeConnectPlatform
	// connectFail turns the connect endpoint into a real probe failure.
	connectFail bool
	// connectCalls counts /api/admin/connect/platforms requests.
	connectCalls int
	modCounter   int
	guestOn      bool
}

// fakeConnectPlatform is one platform row of the connect module's response;
// App mirrors the app configuration cloud2api must ignore (never surfaced).
type fakeConnectPlatform struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Driver string         `json:"driver"`
	Method string         `json:"method"`
	App    map[string]any `json:"app,omitempty"`
}

func newFakeOpenList(t *testing.T) *fakeOpenList {
	t.Helper()
	return &fakeOpenList{nextID: 1}
}

func (f *fakeOpenList) countWrite() {
	f.writeCalls++
}

func (f *fakeOpenList) nextModified() string {
	f.modCounter++
	return time.Now().UTC().Add(time.Duration(f.modCounter) * time.Second).Format(time.RFC3339Nano)
}

func (f *fakeOpenList) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/admin/driver/info" && r.Method == http.MethodGet:
			// cloud2api no longer reads driver schemas (it never writes
			// storage config anymore); answer the way live OpenList does for
			// an unknown driver so any stray call fails loudly.
			writeFake(w, map[string]any{"code": 500, "message": "driver not found"})
		case r.URL.Path == "/api/admin/storage/list" && r.Method == http.MethodGet:
			f.listCalls++
			if f.failList {
				writeFake(w, map[string]any{"code": 500, "message": "openlist is down"})
				return
			}
			// Real OpenList paginates: data is {content: [...], total: N}.
			// perPage (when set) overrides the requested page size so tests
			// can force multi-page reads.
			total := len(f.storages)
			perPage := f.perPage
			if perPage <= 0 {
				if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && n > 0 {
					perPage = n
				}
			}
			if perPage <= 0 || perPage > total {
				perPage = total
			}
			page := 1
			if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 0 {
				page = n
			}
			start, end := (page-1)*perPage, page*perPage
			if start > total {
				start = total
			}
			if end > total {
				end = total
			}
			writeFake(w, map[string]any{"code": 200, "message": "success",
				"data": map[string]any{"content": f.storages[start:end], "total": total}})
		case r.URL.Path == "/api/admin/storage/get" && r.Method == http.MethodGet:
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			for i := range f.storages {
				if f.storages[i].ID == id {
					writeFake(w, map[string]any{"code": 200, "message": "success", "data": f.storages[i]})
					return
				}
			}
			// Live OpenList answers code 500 "record not found" for a
			// deleted id (maps to openlist.ErrStorageNotFound).
			writeFake(w, map[string]any{"code": 500, "message": "record not found"})
		case r.URL.Path == "/api/admin/user/list" && r.Method == http.MethodGet:
			writeFake(w, map[string]any{"code": 200, "message": "success", "data": map[string]any{
				"content": []map[string]any{
					{"id": 1, "username": "admin", "disabled": false},
					{"id": 2, "username": "guest", "disabled": !f.guestOn},
				}, "total": 2}})
		case r.URL.Path == "/api/admin/storage/create" && r.Method == http.MethodPost:
			f.countWrite()
			writeFake(w, map[string]any{"code": 500, "message": "storage creation is not supported anymore"})
		case r.URL.Path == "/api/admin/storage/update" && r.Method == http.MethodPost:
			f.countWrite()
			writeFake(w, map[string]any{"code": 500, "message": "storage updates are not supported anymore"})
		case r.URL.Path == "/api/admin/storage/delete" && r.Method == http.MethodPost:
			f.countWrite()
			writeFake(w, map[string]any{"code": 500, "message": "storage deletion is not supported anymore"})
		case r.URL.Path == "/api/admin/storage/enable" && r.Method == http.MethodPost:
			f.countWrite()
			writeFake(w, map[string]any{"code": 500, "message": "storage enable is not supported anymore"})
		case r.URL.Path == "/api/admin/storage/disable" && r.Method == http.MethodPost:
			f.countWrite()
			writeFake(w, map[string]any{"code": 500, "message": "storage disable is not supported anymore"})
		case r.URL.Path == "/api/admin/connect/platforms" && r.Method == http.MethodGet:
			f.connectCalls++
			switch {
			case f.connectFail:
				writeFake(w, map[string]any{"code": 500, "message": "connect module broken"})
			case f.connectPlatforms == nil:
				// Unpatched OpenList: the SPA answers unknown API paths.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "<!doctype html><html><body>OpenList SPA</body></html>")
			default:
				writeFake(w, map[string]any{"code": 200, "message": "success", "data": map[string]any{
					"module":        map[string]any{"name": "connect", "version": "1", "upstream_version": "v4.2.6"},
					"callback_path": "/api/connect/oauth/callback",
					"platforms":     f.connectPlatforms,
				}})
			}
		default:
			writeFake(w, map[string]any{"code": 404, "message": "not found"})
		}
	})
}

// storageByID returns a copy of one stored row (test helper).
func (f *fakeOpenList) storageByID(id int64) (fakeStorage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, st := range f.storages {
		if st.ID == id {
			return st, true
		}
	}
	return fakeStorage{}, false
}

// listCallCount reports how many storage/list requests were served so far.
func (f *fakeOpenList) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// writeCallCount reports how many storage configuration write requests
// (create|update|delete|enable|disable) were served so far.
func (f *fakeOpenList) writeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeCalls
}

// connectCallCount reports how many connect-module probe requests were served.
func (f *fakeOpenList) connectCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls
}

func writeFake(w http.ResponseWriter, body map[string]any) {
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeOpenList) snapshot() []fakeStorage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeStorage(nil), f.storages...)
}

// --- portal test harness ---

const adminPassword = "pw-secret"
const dataPlaneKey = "sk-data-plane-key"

type portalHarness struct {
	p        *Portal
	fake     *fakeOpenList
	st       *store.Store
	keys     *keymgr.Manager
	registry *gateway.AccountRegistry
	handler  http.Handler // portal + gateway (data plane) + 404 fallthrough
}

func newPortalHarness(t *testing.T) *portalHarness {
	t.Helper()
	fake := newFakeOpenList(t)
	ts := httptest.NewServer(fake.handler())
	t.Cleanup(ts.Close)

	statePath := filepath.Join(t.TempDir(), "state", "state.json")
	cfg := &config.Admin{
		Enabled:     true,
		PasswordEnv: "ADMIN_PASSWORD",
		Password:    adminPassword,
		StateFile:   statePath,
		SessionTTL:  config.Duration(time.Hour),
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// The static config account, like main.go's syncStatic would install it.
	if err := st.InsertDrive(store.Drive{
		ID: "static1", Provider: "local", Driver: "Local", MountPath: "/m1",
		Enabled: true, Static: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	registry := gateway.NewAccountRegistry([]config.Account{
		{ID: "static1", Provider: "local", MountPath: "/m1", Enabled: true},
	})
	client, err := openlist.New(ts.URL, "admin-token")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := keymgr.New(st)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPortal(cfg, registry, client, st, keys, nil)

	// A data-plane gateway sharing the registry, so probes can prove that
	// portal-created accounts are routable.
	gwCfg := &config.Config{
		Openlist: config.Openlist{Endpoint: ts.URL, TokenEnv: "T", Token: "admin-token"},
		Accounts: []config.Account{{ID: "static1", Provider: "local", MountPath: "/m1", Enabled: true}},
		APIKeys: []config.APIKey{{
			ID: "k1", KeyHash: auth.HashKey(dataPlaneKey),
			Accounts:    []string{"static1"},
			Permissions: []string{"list", "read", "write", "transfer"},
		}},
	}
	if err := st.InsertKey(store.APIKey{
		ID: "k1", Hash: auth.HashKey(dataPlaneKey), Static: true,
		CreatedAt:   time.Now().UTC(),
		Drives:      []store.KeyDrive{{Drive: "static1", Root: "/", Perms: []string{"list", "read", "write", "delete", "transfer"}}},
		GlobalPerms: []string{"list", "read", "write", "delete", "transfer"},
		Trash:       store.TrashSettings{Enabled: true, Days: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if err := keys.Reload(); err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gwCfg, keys, client, registry, nil)
	gw.SetAudit(func(e store.AuditEntry) {
		if err := st.InsertAudit(e); err != nil {
			t.Logf("audit insert failed: %v", err)
		}
	})

	return &portalHarness{
		p:        p,
		fake:     fake,
		st:       st,
		keys:     keys,
		registry: registry,
		handler:  p.Wrap(gw.Handler()),
	}
}

func doPortal(t *testing.T, h http.Handler, method, target, cookie string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (h *portalHarness) login(t *testing.T, password string) string {
	t.Helper()
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", map[string]string{"password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("no session cookie set on login")
	return ""
}

func decodePortal(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

// --- tests ---

func TestWrapDelegatesNonAdmin(t *testing.T) {
	h := newPortalHarness(t)
	// Data plane is delegated to the gateway (auth first -> 401).
	rec := doPortal(t, h.handler, http.MethodPost, "/api/storage/static1/list", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("data-plane delegation status = %d (%s), want 401 from the gateway", rec.Code, rec.Body.String())
	}
	// Unknown paths fall through to the gateway's 404.
	rec = doPortal(t, h.handler, http.MethodGet, "/other", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("fallthrough status = %d, want 404", rec.Code)
	}
}

func TestLogin(t *testing.T) {
	h := newPortalHarness(t)

	// Wrong password.
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", map[string]string{"password": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", rec.Code)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("failed login set cookies: %v", cookies)
	}

	// Correct password: check the cookie attributes.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", map[string]string{"password": adminPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d", rec.Code)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}
	if !session.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", session.SameSite)
	}
	if session.Path != "/admin" {
		t.Errorf("cookie Path = %q, want /admin", session.Path)
	}
	if session.MaxAge != 3600 {
		t.Errorf("cookie MaxAge = %d, want 3600", session.MaxAge)
	}
	if session.Secure {
		t.Error("cookie must not set Secure (LAN http deployment)")
	}
}

func TestLoginWrongMethodAndBody(t *testing.T) {
	h := newPortalHarness(t)
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/login", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET login status = %d, want 405", rec.Code)
	}
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("login without body status = %d, want 400", rec.Code)
	}
}

func TestUnauthenticated(t *testing.T) {
	h := newPortalHarness(t)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/", "", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("browser without session status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("redirect Location = %q, want /admin/login", loc)
	}

	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/accounts", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API without session status = %d, want 401", rec.Code)
	}
	m := decodePortal(t, rec)
	if m["code"] != float64(401) {
		t.Errorf("body = %v", m)
	}

	// Bad cookie.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/accounts", "session=forged", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie status = %d, want 401", rec.Code)
	}

	// Login page itself is public.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/login", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "password") {
		t.Errorf("login page status = %d", rec.Code)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/logout", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d", rec.Code)
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/accounts", cookie, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session still valid after logout: status = %d", rec.Code)
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	s := NewSessionStore(-time.Second)
	token, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if s.Validate(token) {
		t.Error("expired token validated")
	}
	if s.Validate("") {
		t.Error("empty token validated")
	}
}

// TestEnableDisableOnlyFlipsCloud2apiSwitch: enable/disable touches ONLY
// cloud2api's own switch — no OpenList storage write happens, the storage
// keeps its OpenList-side state, and the response carries the refreshed
// drive row (IMPLEMENTATION §已纳管网盘 启用/停用).
func TestEnableDisableOnlyFlipsCloud2apiSwitch(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	sid := seedDrive(t, h, "dyn", "/dyn", "Local")

	// Disable.
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/dyn/disable", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d (%s)", rec.Code, rec.Body.String())
	}
	row := dataOf(t, decodePortal(t, rec))
	if row["state"] != "disabled" || row["enabled"] != false || row["storage_disabled"] != false {
		t.Fatalf("row after disable = %v, want state disabled / enabled false / storage_disabled false", row)
	}
	// OpenList storage untouched: still present, still enabled.
	st, ok := h.fake.storageByID(sid)
	if !ok || st.Disabled || st.Status != "work" {
		t.Fatalf("storage after disable = %+v ok=%v, want untouched and enabled", st, ok)
	}
	if a, _ := h.registry.Get("dyn"); a.Enabled {
		t.Error("registry still enabled")
	}
	if drv, err := h.st.GetDrive("dyn"); err != nil || drv.Enabled {
		t.Errorf("store drive after disable = %+v err=%v", drv, err)
	}
	// Persisted granular state is consistent right away.
	if drv, err := h.st.GetDrive("dyn"); err != nil || drv.State != "disabled_local" {
		t.Errorf("persisted state after disable = %q err=%v, want disabled_local", drv.State, err)
	}

	// Re-enable: state recomputed from the storage (healthy), no stale
	// disabled_local behind.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/dyn/enable", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d (%s)", rec.Code, rec.Body.String())
	}
	row = dataOf(t, decodePortal(t, rec))
	if row["state"] != "healthy" || row["enabled"] != true || row["storage_disabled"] != false {
		t.Fatalf("row after enable = %v, want state healthy / enabled true", row)
	}
	st, ok = h.fake.storageByID(sid)
	if !ok || st.Disabled {
		t.Fatalf("storage after enable = %+v ok=%v, must never have been touched", st, ok)
	}
	if drv, err := h.st.GetDrive("dyn"); err != nil || drv.State != "healthy" {
		t.Errorf("persisted state after enable = %q err=%v, want healthy (no stale disabled_local)", drv.State, err)
	}

	// The switch flips are recorded as "enabled"/"disabled" events (the kinds
	// the frontend labels).
	events, _ := h.st.ListEvents(20)
	sawEnabled, sawDisabled := false, false
	for _, ev := range events {
		if ev.Drive != "dyn" {
			continue
		}
		switch ev.Kind {
		case "enabled":
			sawEnabled = true
		case "disabled":
			sawDisabled = true
		}
	}
	if !sawEnabled || !sawDisabled {
		t.Errorf("enabled/disabled events missing (enabled=%v disabled=%v)", sawEnabled, sawDisabled)
	}

	// Zero storage configuration writes across the whole scenario.
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("enable/disable produced %d OpenList storage writes, want 0", got)
	}

	// Static accounts are protected.
	for _, op := range []string{"enable", "disable"} {
		rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/accounts/static1/"+op, cookie, nil)
		if rec.Code != http.StatusConflict {
			t.Errorf("static %s status = %d, want 409", op, rec.Code)
		}
	}
	// Unknown account.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/accounts/ghost/enable", cookie, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown enable status = %d, want 404", rec.Code)
	}
}

// TestDeleteAlwaysReleaseOnly: DELETE removes ONLY the mapping — with or
// without keep_storage, the OpenList storage is never touched and shows up in
// the unmanaged list afterwards (IMPLEMENTATION §已纳管网盘 移出).
func TestDeleteAlwaysReleaseOnly(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	sid := seedDrive(t, h, "dyn", "/dyn", "Local")

	rec := doPortal(t, h.handler, http.MethodDelete, "/admin/api/drives/dyn", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete (no param) status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.registry.Get("dyn"); ok {
		t.Error("registry still has dyn")
	}
	if _, err := h.st.GetDrive("dyn"); err == nil {
		t.Error("store still has dyn")
	}
	st, ok := h.fake.storageByID(sid)
	if !ok || st.Disabled || st.Status != "work" {
		t.Fatalf("storage after delete = %+v ok=%v, want kept untouched", st, ok)
	}
	// The released storage surfaces as unmanaged.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives/unmanaged", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged status = %d", rec.Code)
	}
	found := false
	for _, raw := range dataOf(t, decodePortal(t, rec))["storages"].([]any) {
		if item := raw.(map[string]any); int64(item["storage_id"].(float64)) == sid {
			found = true
		}
	}
	if !found {
		t.Fatal("released storage missing from the unmanaged list")
	}

	// keep_storage=true behaves identically (accepted and ignored).
	sid2 := seedDrive(t, h, "dyn2", "/dyn2", "Local")
	rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/drives/dyn2?keep_storage=true", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete (keep_storage=true) status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := h.fake.storageByID(sid2); !ok {
		t.Fatal("keep_storage=true storage vanished")
	}

	// A "released" event is recorded every time.
	events, _ := h.st.ListEvents(20)
	released := 0
	for _, ev := range events {
		if ev.Kind == "released" && (ev.Drive == "dyn" || ev.Drive == "dyn2") {
			released++
		}
	}
	if released != 2 {
		t.Fatalf("released events = %d, want 2 (one per delete)", released)
	}

	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("delete produced %d OpenList storage writes, want 0", got)
	}

	// Static and unknown accounts.
	rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/accounts/static1", cookie, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("delete static status = %d, want 409", rec.Code)
	}
	rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/accounts/ghost", cookie, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown status = %d, want 404", rec.Code)
	}
}

// A dynamic drive whose storage vanished from OpenList still releases
// locally: delete never reads OpenList anymore.
func TestDeleteWithMissingOpenListStorage(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	seedDrive(t, h, "dyn", "/dyn", "Local")
	h.fake.mu.Lock()
	h.fake.storages = nil
	h.fake.mu.Unlock()

	rec := doPortal(t, h.handler, http.MethodDelete, "/admin/api/accounts/dyn", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 (release never touches OpenList)", rec.Code, rec.Body.String())
	}
	if _, err := h.st.GetDrive("dyn"); err == nil {
		t.Error("store still has dyn")
	}
}

func TestDeleteAccountOrphanLocalCleanup(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	// Inject an orphan: registry + store know it, OpenList never saw it.
	if err := h.registry.Add(config.Account{ID: "orphan", Provider: "local", MountPath: "/orphan", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.InsertDrive(store.Drive{
		ID: "orphan", Provider: "local", MountPath: "/orphan",
		Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := doPortal(t, h.handler, http.MethodDelete, "/admin/api/accounts/orphan", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if _, ok := h.registry.Get("orphan"); ok {
		t.Error("orphan still registered")
	}
}

func TestStatePersistenceAcrossRestart(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	// Adopt a storage (the step-2 way of adding a dynamic drive).
	h.fake.mu.Lock()
	h.fake.storages = append(h.fake.storages, fakeStorage{
		ID: 7, MountPath: "/bd", Driver: "BaiduNetdisk", Status: "work",
		Addition: `{"refresh_token":"rt"}`, Modified: "2026-10-13T00:00:00Z",
	})
	h.fake.mu.Unlock()
	if rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 7, "id": "dyn"}); rec.Code != http.StatusOK {
		t.Fatalf("adopt status = %d (%s)", rec.Code, rec.Body.String())
	}

	// Simulate a restart: fresh registry rebuilt from the store, dynamic
	// drive restored on top of the static ones (main.go semantics).
	drives, err := h.st.ListDrives()
	if err != nil {
		t.Fatal(err)
	}
	registry := gateway.NewAccountRegistry([]config.Account{
		{ID: "static1", Provider: "local", MountPath: "/m1", Enabled: true},
	})
	for _, d := range drives {
		if d.Static {
			continue
		}
		if err := registry.Add(config.Account{
			ID: d.ID, Provider: d.Provider, MountPath: d.MountPath, Enabled: d.Enabled,
		}); err != nil {
			t.Fatalf("restore %s: %v", d.ID, err)
		}
	}
	a, ok := registry.Get("dyn")
	if !ok || a.Provider != "baidu" || a.MountPath != "/bd" || !a.Enabled {
		t.Fatalf("restored account = %+v %v", a, ok)
	}
	if !registry.HasMount("/bd") {
		t.Error("restored mount missing from registry")
	}
}
