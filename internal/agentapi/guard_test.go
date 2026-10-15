package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// guardPlain keys the swap harness (all permissions on drives a and b).
const guardPlain = "sk-c2a-12345678-guardsecretguardsecret"

// newSwapHarness builds a harness whose drives carry RECORDED storage ids
// (drive a -> storage X at /d/a, drive b -> storage at /d/b), so the storage
// guard exercises the id-bound path rather than the static first-boot one.
type swapHarness struct {
	t       *testing.T
	handler http.Handler
	fake    *fakeOL
	st      *store.Store
	keys    *keymgr.Manager
	api     *Server
	reg     *gateway.AccountRegistry
	storageA,
	storageB int64
}

func newSwapHarness(t *testing.T) *swapHarness {
	t.Helper()
	fake := newFakeOL()
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)

	storageA := fake.addStorage("/d/a", "Local")
	storageB := fake.addStorage("/d/b", "Local")

	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, d := range []store.Drive{
		{ID: "a", Provider: "provA", Driver: "Local", StorageID: storageA, MountPath: "/d/a", Enabled: true, CreatedAt: time.Now()},
		{ID: "b", Provider: "provB", Driver: "Local", StorageID: storageB, MountPath: "/d/b", Enabled: true, CreatedAt: time.Now()},
	} {
		if err := st.InsertDrive(d); err != nil {
			t.Fatal(err)
		}
	}

	client, err := openlist.New(up.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := keymgr.New(st)
	if err != nil {
		t.Fatal(err)
	}
	perms := []string{"list", "read", "write", "delete", "link", "transfer", "fetch", "relay"}
	k := store.APIKey{
		ID: "guard", Hash: hashOf(guardPlain), CreatedAt: time.Now().UTC(),
		Drives: []store.KeyDrive{
			{Drive: "a", Root: "/", Perms: perms},
			{Drive: "b", Root: "/", Perms: perms},
		},
		GlobalPerms: []string{"relay"},
		Trash:       store.TrashSettings{Enabled: true, Days: 7},
	}
	if pub, _, ok := splitNew(guardPlain); ok {
		k.PublicID = pub
	}
	if err := st.InsertKey(k); err != nil {
		t.Fatal(err)
	}
	if err := keys.Reload(); err != nil {
		t.Fatal(err)
	}

	registry := gateway.NewAccountRegistry([]config.Account{
		{ID: "a", Provider: "provA", MountPath: "/d/a", Enabled: true},
		{ID: "b", Provider: "provB", MountPath: "/d/b", Enabled: true},
	})
	api := New(keys, st, client, registry, nil)
	return &swapHarness{
		t: t, handler: api.Handler(http.NotFoundHandler()), fake: fake,
		st: st, keys: keys, api: api, reg: registry, storageA: storageA, storageB: storageB,
	}
}

func (h *swapHarness) do(method, target, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("apikey", guardPlain)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

// swapStorageA deletes drive a's recorded storage and creates a new one at
// the same mount (a different driver, like another netdisk account).
func (h *swapHarness) swapStorageA() {
	h.fake.deleteStorage(h.storageA)
	h.fake.addStorage("/d/a", "Aliyun")
}

// fsUnderA counts the fs/p calls the fake served under /d/a.
func (h *swapHarness) fsUnderA() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	n := 0
	for _, p := range h.fake.fsCalls {
		if strings.HasPrefix(p, "/d/a/") || strings.HasPrefix(p, "/p/d/a") {
			n++
		}
	}
	return n
}

func errHint(doc map[string]any) string {
	if e, ok := doc["error"].(map[string]any); ok {
		h, _ := e["hint"].(string)
		return h
	}
	return ""
}

func driveHealthOf(t *testing.T, doc map[string]any, id string) (string, bool) {
	t.Helper()
	drives, _ := doc["drives"].([]any)
	for _, d := range drives {
		dv, _ := d.(map[string]any)
		if dv["id"] == id {
			h, _ := dv["health"].(string)
			return h, true
		}
	}
	return "", false
}

// TestStaleHealthCacheDoesNotSurviveStorageSwap is the acceptance scenario of
// IMPLEMENTATION §存储被删除与替代存储: the 30s health cache still holds the
// OLD storage as healthy, the storage is deleted and a new one created at the
// same mount, and the very next /v1 request — with no cache invalidation and
// no reconcile pass in between — must answer 503 drive_unavailable for every
// file operation, transfer side, public link and the Gokapi relay, with
// nothing reaching the fake's fs endpoints under /d/a.
func TestStaleHealthCacheDoesNotSurviveStorageSwap(t *testing.T) {
	h := newSwapHarness(t)
	h.fake.mu.Lock()
	h.fake.entries["/d/a/file.txt"] = &fakeEntry{name: "file.txt", content: []byte("a"), modified: "2026-10-12T00:00:00Z"}
	h.fake.entries["/d/b/file.txt"] = &fakeEntry{name: "file.txt", content: []byte("b"), modified: "2026-10-12T00:00:00Z"}
	h.fake.mu.Unlock()

	// Warm both the request path and the /v1/me health cache on storage X.
	if rec, doc := h.do("GET", "/v1/drives/a/list?path=/", "", nil); rec.Code != 200 {
		t.Fatalf("warm-up list = %d %v", rec.Code, doc)
	}
	_, meDoc := h.do("GET", "/v1/me", "", nil)
	if health, ok := driveHealthOf(t, meDoc, "a"); !ok || health != "healthy" {
		t.Fatalf("pre-swap health of a = %q, want healthy", health)
	}

	// Mint a public link while the drive is healthy.
	linkURL := ""
	if rec, doc := h.do("POST", "/v1/links", `{"drive":"a","path":"/file.txt","expires_in":3600}`, jsonCT()); rec.Code == 200 {
		linkURL, _ = doc["url"].(string)
	} else {
		t.Fatalf("link create = %d %v", rec.Code, doc)
	}

	before := h.fsUnderA()

	// The swap: X deleted, Y created at /d/a. No reconcile, no invalidation.
	h.swapStorageA()

	requests := []struct {
		name   string
		method string
		target string
		body   string
		hdr    map[string]string
	}{
		{"list", "GET", "/v1/drives/a/list?path=/", "", nil},
		{"download", "GET", "/v1/drives/a/files?path=/file.txt", "", nil},
		{"upload", "PUT", "/v1/drives/a/files?path=/up.bin", "data", octetCT()},
		{"delete", "POST", "/v1/drives/a/delete", `{"paths":["/file.txt"]}`, jsonCT()},
		{"move", "POST", "/v1/drives/a/move", `{"from":"/file.txt","to":"/moved.txt"}`, jsonCT()},
		{"transfer from a", "POST", "/v1/transfers", `{"from":{"drive":"a","path":"/file.txt"},"to":{"drive":"b","path":"/file.txt"}}`, jsonCT()},
		{"transfer to a", "POST", "/v1/transfers", `{"from":{"drive":"b","path":"/file.txt"},"to":{"drive":"a","path":"/file.txt"}}`, jsonCT()},
		{"batch from a", "POST", "/v1/batch", `{"ops":[{"op":"copy","from":{"drive":"a","path":"/file.txt"},"to":{"drive":"b","path":"/file.txt"}}]}`, jsonCT()},
		{"batch to a", "POST", "/v1/batch", `{"ops":[{"op":"copy","from":{"drive":"b","path":"/file.txt"},"to":{"drive":"a","path":"/file.txt"}}]}`, jsonCT()},
		{"link create", "POST", "/v1/links", `{"drive":"a","path":"/file.txt","expires_in":3600}`, jsonCT()},
	}
	for _, tc := range requests {
		rec, doc := h.do(tc.method, tc.target, tc.body, tc.hdr)
		if rec.Code != http.StatusServiceUnavailable || errCode(doc) != CodeDriveUnavailable {
			t.Errorf("%s: %d %v, want 503 drive_unavailable", tc.name, rec.Code, doc)
			continue
		}
		if hint := errHint(doc); !strings.Contains(hint, "confirm") {
			t.Errorf("%s: hint must mention the pending confirmation: %q", tc.name, hint)
		}
	}

	// Public /l/ link on the swapped drive: never serve the file.
	if linkURL == "" {
		t.Fatal("no link URL")
	}
	req := httptest.NewRequest("GET", strings.TrimPrefix(linkURL, "http://example.com"), nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("link download = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "a" {
		t.Fatal("the link served file bytes from the replacement storage")
	}

	// Gokapi relay upload on the swapped drive (the key's relay drive is a).
	ct, body := multipartBody(t, map[string]string{"allowedDownloads": "0", "expiryDays": "1"}, "r.bin", []byte("relay"), "application/octet-stream")
	req = httptest.NewRequest("POST", "/api/files/add", bytes.NewReader(body))
	req.Header.Set("apikey", guardPlain)
	req.Header.Set("Content-Type", ct)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("gokapi relay add = %d %s, want 503", rec.Code, rec.Body.String())
	}

	// Nothing reached the fake's fs endpoints under /d/a after the swap.
	if after := h.fsUnderA(); after != before {
		t.Fatalf("fs calls under /d/a: %d before swap, %d after (the new storage must not be touched)", before, after)
	}

	// Drive b keeps working.
	if rec, doc := h.do("GET", "/v1/drives/b/list?path=/", "", nil); rec.Code != 200 {
		t.Fatalf("drive b list = %d %v", rec.Code, doc)
	}

	// /v1/me (display only): once reconcile has recorded replacement_found,
	// the stale cache must not talk the drive back to healthy.
	if err := h.st.ApplyReconcile("a", "replacement_found", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	_, meDoc = h.do("GET", "/v1/me", "", nil)
	if health, _ := driveHealthOf(t, meDoc, "a"); health != "replacement_found" {
		t.Fatalf("/v1/me health for a = %q, want replacement_found despite the stale cache", health)
	}
}

// TestDriveDisabledInCloud2apiReturnsUnavailable: disabling a drive through
// the cloud2api portal (store enabled=false — the ONLY thing the portal's
// disable switch changes now) blocks /v1 with 503 drive_unavailable while the
// OpenList storage itself stays enabled (IMPLEMENTATION §已纳管网盘).
func TestDriveDisabledInCloud2apiReturnsUnavailable(t *testing.T) {
	h := newSwapHarness(t)
	if err := h.st.SetDriveEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	rec, doc := h.do("GET", "/v1/drives/a/list?path=/", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errCode(doc) != CodeDriveUnavailable {
		t.Fatalf("disabled list = %d %v, want 503 drive_unavailable", rec.Code, doc)
	}
	if hint := errHint(doc); !strings.Contains(hint, "disabled in cloud2api") {
		t.Fatalf("disabled hint = %q", hint)
	}
	// The OpenList storage was never touched: drive b (still enabled) and
	// the admin listing keep working.
	if rec, doc := h.do("GET", "/v1/drives/b/list?path=/", "", nil); rec.Code != 200 {
		t.Fatalf("drive b list = %d %v", rec.Code, doc)
	}
}

func octetCT() map[string]string {
	return map[string]string{"Content-Type": "application/octet-stream"}
}

// TestSwappedDriveOrphanedWhenMountEmpty: storage deleted, nothing at the
// mount → orphaned 503.
func TestSwappedDriveOrphanedWhenMountEmpty(t *testing.T) {
	h := newSwapHarness(t)
	h.fake.deleteStorage(h.storageA)
	rec, doc := h.do("GET", "/v1/drives/a/list?path=/", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errCode(doc) != CodeDriveUnavailable {
		t.Fatalf("orphaned list = %d %v", rec.Code, doc)
	}
	if hint := errHint(doc); !strings.Contains(hint, "deleted") {
		t.Fatalf("orphaned hint = %q", hint)
	}
}

// TestGuardFailClosedWhenOpenListAdminFails: an admin API that errors (but is
// reachable) during the verification denies the request (fail closed).
func TestGuardFailClosedWhenOpenListAdminFails(t *testing.T) {
	h := newSwapHarness(t)
	h.fake.mu.Lock()
	h.fake.failAdmin = true
	h.fake.mu.Unlock()
	rec, doc := h.do("GET", "/v1/drives/a/list?path=/", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errCode(doc) != CodeDriveUnavailable {
		t.Fatalf("unverified list = %d %v", rec.Code, doc)
	}
	if hint := errHint(doc); !strings.Contains(hint, "could not verify") {
		t.Fatalf("unverified hint = %q", hint)
	}
}

// TestGuardFollowsMountRenameWithRecordedID: the same storage id moved to a
// new mount keeps serving /v1 traffic — the guard's reconcile hook follows
// the rename and the registry mount is used for the fs call.
func TestGuardFollowsMountRenameWithRecordedID(t *testing.T) {
	h := newSwapHarness(t)
	h.fake.mu.Lock()
	h.fake.entries["/d/a/renamed.txt"] = &fakeEntry{name: "renamed.txt", content: []byte("r"), modified: "2026-10-12T00:00:00Z"}
	h.fake.mu.Unlock()

	h.api.SetReconciler(func(ctx context.Context) {
		_ = h.api.registry.SetMount("a", "/d/a2")
		if d, err := h.st.GetDrive("a"); err == nil {
			d.MountPath = "/d/a2"
			_ = h.st.UpdateDrive(d)
		}
	})

	h.fake.renameStorage(h.storageA, "/d/a", "/d/a2")
	rec, doc := h.do("GET", "/v1/drives/a/list?path=/", "", nil)
	if rec.Code != 200 {
		t.Fatalf("list after rename = %d %v", rec.Code, doc)
	}
	items, _ := doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "renamed.txt" {
		t.Fatalf("items = %v (the renamed mount must serve the entries)", doc)
	}
}
