package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

const (
	fullKeyPlain = "sk-full-key-000"
	listKeyPlain = "sk-list-key-000"
)

// recordedRequest is what the fake OpenList upstream observed.
type recordedRequest struct {
	Method      string
	Path        string
	RawQuery    string
	Auth        string
	ContentType string
	FilePath    string
	Body        []byte
}

type fakeUpstream struct {
	mu     sync.Mutex
	reqs   []recordedRequest
	status int                                          // HTTP status to answer with
	body   string                                       // raw body to answer with
	serve  func(w http.ResponseWriter, r *http.Request) // optional override
	// storages backs /api/admin/storage/list|get for the storage guard; the
	// canned body is never used for those paths.
	storages []guardStorage
	nextID   int64
}

// guardStorage is one admin storage of the gateway test fake.
type guardStorage struct {
	id       int64
	mount    string
	driver   string
	status   string
	disabled bool
}

// defaultFakeStorages mirrors the accounts of newTestGateway: healthy
// storages at /m1 and /m2 plus a disabled one at /m3.
func defaultFakeStorages() []guardStorage {
	return []guardStorage{
		{id: 1, mount: "/m1", driver: "Local"},
		{id: 2, mount: "/m2", driver: "Local"},
		{id: 3, mount: "/m3", driver: "Local", disabled: true},
	}
}

func (st guardStorage) statusText() string {
	if st.status == "" {
		return "work"
	}
	return st.status
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The admin storage endpoints always answer a well-formed list/get
		// (the canned body below is data-plane only), so the default storage
		// guard installed by New can verify drives in every test.
		switch {
		case r.URL.Path == "/api/admin/storage/list" && r.Method == http.MethodGet:
			f.record(w, r, func() {
				writeGuardStorageList(w, f.snapshotStorages())
			})
			return
		case r.URL.Path == "/api/admin/storage/get" && r.Method == http.MethodGet:
			f.record(w, r, func() {
				id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
				for _, st := range f.snapshotStorages() {
					if st.id == id {
						writeGuardStorage(w, st)
						return
					}
				}
				fmt.Fprintf(w, `{"code":500,"message":"record not found","data":null}`)
			})
			return
		}
		if f.serve != nil {
			f.serve(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			RawQuery:    r.URL.RawQuery,
			Auth:        r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			FilePath:    r.Header.Get("File-Path"),
			Body:        body,
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	})
}

// record captures the request like the canned path and runs serve.
func (f *fakeUpstream) record(w http.ResponseWriter, r *http.Request, serve func()) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedRequest{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"),
		Body: body,
	})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	serve()
}

func (f *fakeUpstream) snapshotStorages() []guardStorage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storages == nil {
		return defaultFakeStorages()
	}
	return append([]guardStorage{}, f.storages...)
}

func writeGuardStorageList(w http.ResponseWriter, list []guardStorage) {
	content := make([]map[string]any, 0, len(list))
	for _, st := range list {
		content = append(content, guardStorageJSON(st))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "message": "success",
		"data": map[string]any{"content": content, "total": len(list)},
	})
}

func writeGuardStorage(w http.ResponseWriter, st guardStorage) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "message": "success", "data": guardStorageJSON(st),
	})
}

func guardStorageJSON(st guardStorage) map[string]any {
	return map[string]any{
		"id": st.id, "mount_path": st.mount, "driver": st.driver,
		"status": st.statusText(), "disabled": st.disabled, "enabled": !st.disabled,
	}
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeUpstream) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("upstream received no requests")
	}
	return f.reqs[len(f.reqs)-1]
}

func newTestGateway(t *testing.T, up *fakeUpstream) http.Handler {
	t.Helper()
	ts := httptest.NewServer(up.handler())
	t.Cleanup(ts.Close)

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Openlist: config.Openlist{Endpoint: ts.URL, TokenEnv: "OPENLIST_TOKEN", Token: "admin-token"},
		Accounts: []config.Account{
			{ID: "a1", Provider: "prov1", MountPath: "/m1", Enabled: true},
			{ID: "a2", Provider: "prov2", MountPath: "/m2", Enabled: true},
			{ID: "a3", Provider: "prov3", MountPath: "/m3", Enabled: false},
		},
		APIKeys: []config.APIKey{
			{
				ID:          "full",
				KeyHash:     auth.HashKey(fullKeyPlain),
				Accounts:    []string{"a1", "a2"},
				Permissions: []string{"list", "read", "write", "transfer"},
			},
			{
				ID:          "limited",
				KeyHash:     auth.HashKey(listKeyPlain),
				Accounts:    []string{"a1"},
				Permissions: []string{"list"},
			},
		},
	}
	keys := newTestKeyManager(t,
		keySpec{plain: fullKeyPlain, id: "full", accounts: []string{"a1", "a2"},
			perms: []auth.Permission{auth.PermList, auth.PermRead, auth.PermWrite, auth.PermDelete, auth.PermTransfer}},
		keySpec{plain: listKeyPlain, id: "limited", accounts: []string{"a1"},
			perms: []auth.Permission{auth.PermList}},
	)
	client, err := openlist.New(cfg.Openlist.Endpoint, cfg.Openlist.Token)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, keys, client, nil, nil).Handler()
}

// keySpec is one static key for the test key manager.
type keySpec struct {
	plain    string
	id       string
	accounts []string
	perms    []auth.Permission
}

// newTestKeyManager builds a keymgr.Manager backed by a temporary SQLite
// database with the given keys installed.
func newTestKeyManager(t *testing.T, specs ...keySpec) *keymgr.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, sp := range specs {
		permNames := make([]string, 0, len(sp.perms))
		for _, p := range sp.perms {
			permNames = append(permNames, string(p))
		}
		drives := make([]store.KeyDrive, 0, len(sp.accounts))
		for _, a := range sp.accounts {
			drives = append(drives, store.KeyDrive{Drive: a, Root: "/", Perms: permNames})
		}
		if err := st.InsertKey(store.APIKey{
			ID: sp.id, Hash: auth.HashKey(sp.plain), Static: true,
			CreatedAt: time.Now().UTC(), Drives: drives, GlobalPerms: permNames,
			Trash: store.TrashSettings{Enabled: true, Days: 7},
		}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := keymgr.New(st)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const okBody = `{"code":200,"message":"success","data":{"path":"/m1/sub","content":[{"name":"f.txt","path":"/m1/sub/f.txt"}]}}`

func newUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	return &fakeUpstream{status: http.StatusOK, body: okBody}
}

func decodeBody(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("response body is not JSON: %v", err)
	}
	return m
}

func TestAuthFailures(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	cases := []struct {
		name string
		key  string
	}{
		{"no key", ""},
		{"unknown key", "sk-nope"},
		{"empty bearer", " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/list", strings.NewReader(`{"path":"/"}`))
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var m map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Fatalf("body not JSON: %v", err)
			}
			if m["code"] != float64(401) {
				t.Errorf("body = %v", m)
			}
		})
	}
	// Non-Bearer scheme.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/list", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-bearer scheme: status = %d, want 401", rec.Code)
	}
	if up.count() != 0 {
		t.Error("upstream must not be reached on auth failure")
	}
}

func TestAccountWhitelistAndPermission(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	// Unknown account -> 404, upstream untouched.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/ghost/list", strings.NewReader(`{"path":"/"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown account status = %d, want 404", rec.Code)
	}

	// A3: existing account outside the whitelist -> 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/storage/a2/list", strings.NewReader(`{"path":"/"}`))
	req.Header.Set("Authorization", "Bearer "+listKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("whitelist status = %d, want 403", rec.Code)
	}

	// Disabled account -> 403 even for a whitelisted key.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/storage/a3/list", strings.NewReader(`{"path":"/"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled account status = %d, want 403", rec.Code)
	}

	// Permission mapping: limited key holds list only.
	perOK := []string{"/api/storage/a1/list"}
	perForbidden := []string{"/api/storage/a1/get", "/api/storage/a1/mkdir", "/api/storage/a1/remove", "/api/storage/a1/copy", "/api/storage/a1/move"}
	for _, p := range perOK {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"path":"/"}`))
		req.Header.Set("Authorization", "Bearer "+listKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("POST %s with list-only key: status = %d, want 200", p, rec.Code)
		}
	}
	for _, p := range perForbidden {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"path":"/"}`))
		req.Header.Set("Authorization", "Bearer "+listKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with list-only key: status = %d, want 403", p, rec.Code)
		}
	}
	// raw needs read.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/x", nil)
	req.Header.Set("Authorization", "Bearer "+listKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET raw with list-only key: status = %d, want 403", rec.Code)
	}
	// transfer endpoint needs transfer permission.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/transfer", strings.NewReader(`{"src_account":"a1","dst_account":"a2","names":["x"]}`))
	req.Header.Set("Authorization", "Bearer "+listKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("transfer with list-only key: status = %d, want 403", rec.Code)
	}
}

func TestMethodNotAllowedAndUnknownOp(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	cases := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/api/storage/a1/list", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/storage/a1/put", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/storage/a1/raw/x", http.StatusOK},
		{http.MethodPost, "/api/storage/a1/unknown", http.StatusNotFound},
		{http.MethodPost, "/api/storage/a1", http.StatusNotFound},
		{http.MethodGet, "/other", http.StatusNotFound},
		// Without a portal mounted, admin paths answer 404 from the gateway.
		{http.MethodGet, "/admin/", http.StatusNotFound},
		{http.MethodPost, "/admin/api/login", http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{"path":"/"}`))
		req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.target, rec.Code, tc.want)
		}
	}
}

// TestPathTraversalRejected covers acceptance case A5: hostile paths must be
// rejected with 400 before anything reaches OpenList.
func TestPathTraversalRejected(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	badPaths := []string{
		`/../`,
		`..`,
		"/..",
		"/a/../../b",
		"/baidu/../123pan",
		"/x%00",
		"a\x00b",
		"a\\b",
		"%2e%2e",
		"/x/%2E%2E/y",
	}
	for _, p := range badPaths {
		payload, _ := json.Marshal(map[string]any{"path": p})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/list", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("list path %q: status = %d, want 400 (body %s)", p, rec.Code, rec.Body.String())
		}
	}
	// Malicious names.
	for _, name := range []string{"..", "a/b", "x\\y", "%00"} {
		payload, _ := json.Marshal(map[string]any{"dir": "/", "names": []string{name}})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/remove", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("remove name %q: status = %d, want 400", name, rec.Code)
		}
	}
	// Malicious dst_dir.
	payload, _ := json.Marshal(map[string]any{"src_dir": "/", "dst_dir": "/../../m3", "names": []string{"f"}, "dst_account": "a2"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/copy", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("copy dst_dir traversal: status = %d, want 400", rec.Code)
	}
	// Malicious raw URL path (encoded dot-dot survives the transport).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/%2e%2e/%2e%2e/etc", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("raw traversal: status = %d, want 400", rec.Code)
	}
	// Same request as the real server presents it: the URL path is decoded
	// once before routing, so the cleaner never sees a raw/ prefix.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://gw/api/storage/a1/raw/%2e%2e/x", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("raw traversal (server-decoded path): status = %d, want 400", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("upstream received %d requests, want 0 for all hostile paths", up.count())
	}
}

func TestListTranslatesAndStrips(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/list", strings.NewReader(`{"path":"/sub","page":1,"per_page":100,"refresh":false,"password":""}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	sent := up.last(t)
	if sent.Path != "/api/fs/list" {
		t.Errorf("upstream path = %q", sent.Path)
	}
	if sent.Auth != "admin-token" {
		t.Errorf("upstream Authorization = %q, want OpenList token", sent.Auth)
	}
	var sentBody map[string]any
	if err := json.Unmarshal(sent.Body, &sentBody); err != nil {
		t.Fatalf("forwarded body not JSON: %v", err)
	}
	if sentBody["path"] != "/m1/sub" {
		t.Errorf("forwarded path = %v, want /m1/sub", sentBody["path"])
	}
	if fmt.Sprint(sentBody["page"]) != "1" {
		t.Errorf("page must be preserved, got %v (%T)", sentBody["page"], sentBody["page"])
	}

	// Response: internal /m1 prefix stripped everywhere (A1).
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	data := resp["data"].(map[string]any)
	if data["path"] != "/sub" {
		t.Errorf("response data.path = %v, want /sub", data["path"])
	}
	entry := data["content"].([]any)[0].(map[string]any)
	if entry["path"] != "/sub/f.txt" {
		t.Errorf("response entry path = %v, want /sub/f.txt", entry["path"])
	}
	if strings.Contains(rec.Body.String(), "/m1") {
		t.Errorf("response still contains internal prefix: %s", rec.Body.String())
	}
}

func TestCopyTranslatesDstAccount(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	payload := `{"src_dir":"/docs","dst_dir":"/incoming","names":["a.txt"],"dst_account":"a2"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/copy", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	sent := up.last(t)
	if sent.Path != "/api/fs/copy" {
		t.Errorf("upstream path = %q", sent.Path)
	}
	var sentBody map[string]any
	if err := json.Unmarshal(sent.Body, &sentBody); err != nil {
		t.Fatal(err)
	}
	if sentBody["src_dir"] != "/m1/docs" {
		t.Errorf("src_dir = %v, want /m1/docs", sentBody["src_dir"])
	}
	if sentBody["dst_dir"] != "/m2/incoming" {
		t.Errorf("dst_dir = %v, want /m2/incoming", sentBody["dst_dir"])
	}
	if _, present := sentBody["dst_account"]; present {
		t.Error("dst_account must be removed from the forwarded body")
	}
}

func TestCopyCrossAccountRequiresTransfer(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	// limited key: account a2 not whitelisted -> 403.
	payload := `{"src_dir":"/","dst_dir":"/","names":["a.txt"],"dst_account":"a2"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/copy", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+listKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("dst not whitelisted: status = %d, want 403", rec.Code)
	}
	if up.count() != 0 {
		t.Error("upstream must not be reached")
	}
}

func TestTransferEndpoint(t *testing.T) {
	up := newUpstream(t)
	h := newTestGateway(t, up)

	payload := `{"src_account":"a1","dst_account":"a2","src_dir":"/docs","dst_dir":"/incoming","names":["big.bin"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/transfer", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	sent := up.last(t)
	if sent.Path != "/api/fs/copy" {
		t.Errorf("transfer must expand to /api/fs/copy, got %q", sent.Path)
	}
	var sentBody map[string]any
	if err := json.Unmarshal(sent.Body, &sentBody); err != nil {
		t.Fatal(err)
	}
	if sentBody["src_dir"] != "/m1/docs" || sentBody["dst_dir"] != "/m2/incoming" {
		t.Errorf("transfer dirs = %v / %v", sentBody["src_dir"], sentBody["dst_dir"])
	}
	if _, present := sentBody["src_account"]; present {
		t.Error("src_account must be removed from forwarded body")
	}
	if _, present := sentBody["dst_account"]; present {
		t.Error("dst_account must be removed from forwarded body")
	}

	// B3: transfer to an account outside the whitelist -> 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/transfer",
		strings.NewReader(`{"src_account":"a1","dst_account":"a2","names":["x"]}`))
	req.Header.Set("Authorization", "Bearer "+listKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("transfer to non-whitelisted: status = %d, want 403", rec.Code)
	}
}

func TestPutStreamsAndTranslatesFilePath(t *testing.T) {
	up := &fakeUpstream{
		status: http.StatusOK,
		body:   `{"code":200,"message":"success","data":{"task":{"id":"t1"}}}`,
	}
	h := newTestGateway(t, up)

	content := strings.Repeat("hello upload ", 1000)
	target := "/api/storage/a1/put"
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	req.Header.Set("File-Path", url.QueryEscape("/up/hello.txt"))
	req.Header.Set("Content-Type", "text/plain")
	req.ContentLength = int64(len(content))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	sent := up.last(t)
	if sent.Method != http.MethodPut || sent.Path != "/api/fs/put" {
		t.Errorf("upstream %s %s", sent.Method, sent.Path)
	}
	wantPath := url.QueryEscape("/m1/up/hello.txt")
	if sent.FilePath != wantPath {
		t.Errorf("upstream File-Path = %q, want %q", sent.FilePath, wantPath)
	}
	if string(sent.Body) != content {
		t.Errorf("upload body corrupted: got %d bytes, want %d", len(sent.Body), len(content))
	}

	// Missing File-Path -> 400, upstream untouched.
	n := up.count()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, target, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put without File-Path: status = %d, want 400", rec.Code)
	}
	if up.count() != n {
		t.Error("upstream must not be reached for invalid put")
	}

	// Traversal in File-Path -> 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, target, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	req.Header.Set("File-Path", url.QueryEscape("/../../etc/passwd"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("put traversal: status = %d, want 400", rec.Code)
	}
}

func TestRawStreamsDownload(t *testing.T) {
	payload := bytes.Repeat([]byte{0xA5, 0x5A}, 64*1024) // 128 KiB binary
	up := &fakeUpstream{status: http.StatusOK}
	up.serve = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.reqs = append(up.reqs, recordedRequest{
			Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Body: body,
		})
		up.mu.Unlock()
		if r.URL.Path == "/api/fs/get" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"sign":"server-sign"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}
	h := newTestGateway(t, up)

	req := httptest.NewRequest(http.MethodGet, "/api/storage/a1/raw/sub/f.bin?sign=abc", nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("downloaded %d bytes, want %d identical bytes", rec.Body.Len(), len(payload))
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}

	sent := up.last(t)
	if sent.Path != "/p/m1/sub/f.bin" {
		t.Errorf("upstream path = %q, want /p/m1/sub/f.bin", sent.Path)
	}
	if sent.RawQuery != "sign=server-sign" {
		t.Errorf("upstream query = %q, want server-fetched sign", sent.RawQuery)
	}
	if sent.Auth != "admin-token" {
		t.Errorf("upstream Authorization = %q", sent.Auth)
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	// OpenList-style business error: HTTP 200 with non-200 body code.
	up := &fakeUpstream{status: http.StatusOK, body: `{"code":500,"message":"failed get /m1/x: not found"}`}
	h := newTestGateway(t, up)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/get", strings.NewReader(`{"path":"/x"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["code"] != float64(500) {
		t.Errorf("code = %v, want upstream code 500", m["code"])
	}
	if m["message"] != "failed get /x: not found" {
		t.Errorf("message = %v, want prefix-stripped upstream message", m["message"])
	}

	// Transport-level HTTP error with parseable body.
	up2 := &fakeUpstream{status: http.StatusForbidden, body: `{"code":403,"message":"denied"}`}
	h2 := newTestGateway(t, up2)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/storage/a1/get", strings.NewReader(`{"path":"/x"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	m = decodeBody(t, rec.Body)
	if m["code"] != float64(403) || m["message"] != "denied" {
		t.Errorf("error body = %v", m)
	}

	// Non-JSON success response: passed through unmodified, not blocked.
	up3 := &fakeUpstream{status: http.StatusOK, body: `not-json at all /m1/x`}
	h3 := newTestGateway(t, up3)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/storage/a1/get", strings.NewReader(`{"path":"/x"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h3.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not-json at all") {
		t.Errorf("non-JSON passthrough: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// TestGetAndListResponsesNeverLeakInternalKeys covers the v0.3.1 leak fix
// (UPGRADE §1.2 P1): fs/get and fs/list responses returned by the gateway
// must not contain raw_url, sign or provider — neither in the top-level
// data nor in content items — nor the internal OpenList address.
func TestGetAndListResponsesNeverLeakInternalKeys(t *testing.T) {
	up := &fakeUpstream{status: http.StatusOK}
	up.serve = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/fs/get":
			fmt.Fprint(w, `{"code":200,"message":"success","data":{"name":"hello.txt","path":"/m1/hello.txt","sign":"abc123:0","raw_url":"http://127.0.0.1:5244/p/m1/hello.txt?sign=abc123:0","provider":"Local","size":6}}`)
		case "/api/fs/list":
			fmt.Fprint(w, `{"code":200,"message":"success","data":{"path":"/m1","content":[{"name":"a.txt","path":"/m1/a.txt","sign":"s:0","raw_url":"http://127.0.0.1:5244/d/m1/a.txt","provider":"Local"}]}}`)
		default:
			fmt.Fprint(w, `{"code":200,"message":"success"}`)
		}
	}
	h := newTestGateway(t, up)

	for _, target := range []string{"/api/storage/a1/get", "/api/storage/a1/list"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"path":"/"}`))
		req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, banned := range []string{`"raw_url"`, `"sign"`, `"provider"`, "5244"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s response contains %s: %s", target, banned, body)
			}
		}
		// The useful metadata must survive.
		if !strings.Contains(body, `"name"`) {
			t.Errorf("%s response lost file metadata: %s", target, body)
		}
	}
}

// TestUpstreamUnreachable: with the storage guard installed, an unreachable
// OpenList fails the verification first (fail closed, 503 drive-unavailable
// style) instead of proxying into a dead upstream (502 would leak nothing,
// but the guard message is the documented behavior now).
func TestUpstreamUnreachable(t *testing.T) {
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Openlist: config.Openlist{Endpoint: "http://127.0.0.1:1", TokenEnv: "T", Token: "tok"},
		Accounts: []config.Account{{ID: "a1", Provider: "p", MountPath: "/m1", Enabled: true}},
		APIKeys:  []config.APIKey{{ID: "k", KeyHash: auth.HashKey(fullKeyPlain), Accounts: []string{"a1"}, Permissions: []string{"list"}}},
	}
	keys := newTestKeyManager(t, keySpec{
		plain: fullKeyPlain, id: "k", accounts: []string{"a1"},
		perms: []auth.Permission{auth.PermList},
	})
	client, err := openlist.New(cfg.Openlist.Endpoint, cfg.Openlist.Token)
	if err != nil {
		t.Fatal(err)
	}
	h := New(cfg, keys, client, nil, nil).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/storage/a1/list", strings.NewReader(`{"path":"/"}`))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (guard fails closed)", rec.Code)
	}
	m := decodeBody(t, rec.Body)
	if m["code"] != float64(http.StatusServiceUnavailable) {
		t.Errorf("body = %v", m)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "could not verify") {
		t.Errorf("message = %v, want the unverified wording", m["message"])
	}
}
