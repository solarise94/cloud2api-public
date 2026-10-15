package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// guardTestEnv wires the legacy gateway to a store-backed storage guard (like
// main does) over a stateful fake OpenList, so storage swaps and mount renames
// can be replayed against /api/storage/* and /api/transfer.
type guardTestEnv struct {
	up       *fakeUpstream
	ts       *httptest.Server
	st       *store.Store
	reg      *AccountRegistry
	client   *openlist.Client
	handler  http.Handler
	storageA int64
	storageB int64
}

func newGuardEnv(t *testing.T) *guardTestEnv {
	t.Helper()
	up := &fakeUpstream{status: http.StatusOK, body: okBody}
	up.storages = []guardStorage{
		{id: 21, mount: "/d/a", driver: "Local"},
		{id: 22, mount: "/d/b", driver: "Local"},
	}
	up.nextID = 22
	ts := httptest.NewServer(up.handler())
	t.Cleanup(ts.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, d := range []store.Drive{
		{ID: "a", Provider: "provA", Driver: "Local", StorageID: 21, MountPath: "/d/a", Enabled: true, CreatedAt: time.Now()},
		{ID: "b", Provider: "provB", Driver: "Local", StorageID: 22, MountPath: "/d/b", Enabled: true, CreatedAt: time.Now()},
	} {
		if err := st.InsertDrive(d); err != nil {
			t.Fatal(err)
		}
	}
	perms := []string{"list", "read", "write", "delete", "transfer"}
	drives := []store.KeyDrive{
		{Drive: "a", Root: "/", Perms: perms},
		{Drive: "b", Root: "/", Perms: perms},
	}
	if err := st.InsertKey(store.APIKey{
		ID: "full", Hash: auth.HashKey(fullKeyPlain), CreatedAt: time.Now().UTC(),
		Drives: drives, GlobalPerms: perms, Trash: store.TrashSettings{Enabled: true, Days: 7},
	}); err != nil {
		t.Fatal(err)
	}
	keys, err := keymgr.New(st)
	if err != nil {
		t.Fatal(err)
	}

	client, err := openlist.New(ts.URL, "admin-token")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewAccountRegistry([]config.Account{
		{ID: "a", Provider: "provA", MountPath: "/d/a", Enabled: true},
		{ID: "b", Provider: "provB", MountPath: "/d/b", Enabled: true},
	})

	followMount := func(driveID, mount string) {
		_ = reg.SetMount(driveID, mount)
		if d, err := st.GetDrive(driveID); err == nil {
			d.MountPath = mount
			_ = st.UpdateDrive(d)
		}
	}
	guard := storageid.NewGuard(client,
		func(driveID string) (storageid.Drive, bool) {
			d, err := st.GetDrive(driveID)
			if err != nil {
				return storageid.Drive{}, false
			}
			return storageid.Drive{StorageID: d.StorageID, MountPath: d.MountPath, Enabled: d.Enabled, State: d.State}, true
		},
		func(driveID string) (string, bool) {
			a, ok := reg.Get(driveID)
			return a.MountPath, ok
		}, nil)
	guard.SetReconciler(func(ctx context.Context) {
		// Follow renames of the known storages, like reconcile.Reconcile does.
		for _, s := range up.snapshotStorages() {
			var driveID string
			switch s.id {
			case 21:
				driveID = "a"
			case 22:
				driveID = "b"
			default:
				continue
			}
			followMount(driveID, s.mount)
		}
	})

	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		Openlist: config.Openlist{Endpoint: ts.URL, Token: "admin-token"},
		Accounts: []config.Account{
			{ID: "a", Provider: "provA", MountPath: "/d/a", Enabled: true},
			{ID: "b", Provider: "provB", MountPath: "/d/b", Enabled: true},
		},
	}
	gw := New(cfg, keys, client, reg, nil)
	gw.SetStorageGuard(guard)
	return &guardTestEnv{up: up, ts: ts, st: st, reg: reg, client: client, handler: gw.Handler(), storageA: 21, storageB: 22}
}

func (e *guardTestEnv) post(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *guardTestEnv) put(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader("data"))
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	req.Header.Set("File-Path", "%2Fup%2Fx.bin")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *guardTestEnv) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+fullKeyPlain)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// setUpStorages replaces the fake's storage list.
func (e *guardTestEnv) setUpStorages(list []guardStorage) {
	e.up.mu.Lock()
	e.up.storages = list
	e.up.mu.Unlock()
}

// swapStorageA deletes drive a's recorded storage and creates a new one at
// the same mount (IMPLEMENTATION §存储被删除与替代存储).
func (e *guardTestEnv) swapStorageA(t *testing.T) {
	e.up.mu.Lock()
	e.up.nextID++
	newID := e.up.nextID
	kept := e.up.storages[:0]
	for _, st := range e.up.storages {
		if st.id != e.storageA {
			kept = append(kept, st)
		}
	}
	kept = append(kept, guardStorage{id: newID, mount: "/d/a", driver: "Aliyun"})
	e.up.storages = kept
	e.up.mu.Unlock()
}

// fsPathsUnder returns the data-plane (/api/fs/*, /p/*) requests whose body
// or path references a path under mount (path-boundary aware, so /d/a does
// not match /d/a-renamed).
func (e *guardTestEnv) fsPathsUnder(t *testing.T, mount string) []string {
	t.Helper()
	e.up.mu.Lock()
	defer e.up.mu.Unlock()
	var out []string
	for _, r := range e.up.reqs {
		if !strings.HasPrefix(r.Path, "/api/fs/") && !strings.HasPrefix(r.Path, "/p/") {
			continue
		}
		blob := r.Path + " " + r.FilePath + " " + string(r.Body)
		if strings.Contains(blob, mount+"/") || strings.Contains(blob, mount+`"`) {
			out = append(out, r.Path)
		}
	}
	return out
}

// TestLegacyStorageSwapBlocksAllOps: after the recorded storage of drive a is
// deleted and a new storage takes over the mount, every legacy data-plane op
// on a answers 503 — including transfer src/dst — and nothing reaches the
// fake's fs endpoints under /d/a. This holds even though no reconcile pass
// ran in between (the guard verifies freshly per request).
func TestLegacyStorageSwapBlocksAllOps(t *testing.T) {
	e := newGuardEnv(t)

	// Healthy before the swap.
	if rec := e.post(t, "/api/storage/a/list", `{"path":"/"}`); rec.Code != http.StatusOK {
		t.Fatalf("pre-swap list = %d %s", rec.Code, rec.Body.String())
	}
	before := len(e.fsPathsUnder(t, "/d/a"))

	e.swapStorageA(t)

	denied := []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"list", e.post(t, "/api/storage/a/list", `{"path":"/"}`)},
		{"get", e.post(t, "/api/storage/a/get", `{"path":"/x"}`)},
		{"mkdir", e.post(t, "/api/storage/a/mkdir", `{"path":"/x"}`)},
		{"remove", e.post(t, "/api/storage/a/remove", `{"dir":"/","names":["x"]}`)},
		{"copy", e.post(t, "/api/storage/a/copy", `{"src_dir":"/","dst_dir":"/","names":["x"]}`)},
		{"move", e.post(t, "/api/storage/a/move", `{"src_dir":"/","dst_dir":"/","names":["x"]}`)},
		{"put", e.put(t, "/api/storage/a/put")},
		{"raw", e.get(t, "/api/storage/a/raw/x.bin")},
		{"transfer src a", e.post(t, "/api/transfer", `{"src_account":"a","dst_account":"b","src_dir":"/","dst_dir":"/","names":["x"]}`)},
		{"transfer dst a", e.post(t, "/api/transfer", `{"src_account":"b","dst_account":"a","src_dir":"/","dst_dir":"/","names":["x"]}`)},
	}
	for _, tc := range denied {
		if tc.rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (body %s)", tc.name, tc.rec.Code, tc.rec.Body.String())
		}
		if msg := tc.rec.Body.String(); !strings.Contains(msg, "confirm") {
			t.Errorf("%s: message must mention the pending confirmation: %s", tc.name, msg)
		}
	}

	after := e.fsPathsUnder(t, "/d/a")
	if len(after) != before {
		t.Fatalf("fs calls under /d/a after the swap: %v (had %d before)", after, before)
	}

	// The other drive keeps working.
	if rec := e.post(t, "/api/storage/b/list", `{"path":"/"}`); rec.Code != http.StatusOK {
		t.Fatalf("drive b list = %d %s", rec.Code, rec.Body.String())
	}
}

// TestLegacyOrphanedStorageBlocks: storage deleted with nothing taking over
// the mount → 503.
func TestLegacyOrphanedStorageBlocks(t *testing.T) {
	e := newGuardEnv(t)
	e.setUpStorages([]guardStorage{{id: e.storageB, mount: "/d/b", driver: "Local"}})
	if rec := e.post(t, "/api/storage/a/list", `{"path":"/"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("orphaned list = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if msg := lastMessage(t, e.post(t, "/api/storage/a/list", `{"path":"/"}`)); !strings.Contains(msg, "deleted") {
		t.Fatalf("orphaned message = %q", msg)
	}
}

func lastMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeBody(t, rec.Body)
	msg, _ := m["message"].(string)
	return msg
}

// TestLegacyGuardFollowsMountRename: the same storage id moved to a new mount
// keeps working — the guard's reconcile hook follows the rename (registry and
// store) and the request lands on the new mount path.
func TestLegacyGuardFollowsMountRename(t *testing.T) {
	e := newGuardEnv(t)
	e.setUpStorages([]guardStorage{
		{id: e.storageA, mount: "/d/a-renamed", driver: "Local"},
		{id: e.storageB, mount: "/d/b", driver: "Local"},
	})

	if rec := e.post(t, "/api/storage/a/list", `{"path":"/"}`); rec.Code != http.StatusOK {
		t.Fatalf("list after rename = %d %s", rec.Code, rec.Body.String())
	}
	if got := len(e.fsPathsUnder(t, "/d/a-renamed")); got == 0 {
		t.Error("no fs call under the renamed mount")
	}
	if got := e.fsPathsUnder(t, "/d/a"); len(got) != 0 {
		t.Errorf("fs calls still hit the old mount: %v", got)
	}
	// The registry followed the rename.
	if a, _ := e.reg.Get("a"); a.MountPath != "/d/a-renamed" {
		t.Fatalf("registry mount = %q, want /d/a-renamed", a.MountPath)
	}
}
