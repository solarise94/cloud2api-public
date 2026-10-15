package admin

// Step-1 OpenList-entry tests (IMPLEMENTATION-openlist-entry.md 第一步):
// openlist_public_url validation, snapshot-driven listings, sync, adopt with
// real state, rebind, and guarded reauth approval.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/reconcile"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// --- fake/storage helpers ---

func addFakeStorage(t *testing.T, h *portalHarness, st fakeStorage) {
	t.Helper()
	h.fake.mu.Lock()
	h.fake.storages = append(h.fake.storages, st)
	h.fake.mu.Unlock()
}

func removeFakeStorage(t *testing.T, h *portalHarness, id int64) {
	t.Helper()
	h.fake.mu.Lock()
	kept := h.fake.storages[:0]
	for _, st := range h.fake.storages {
		if st.ID != id {
			kept = append(kept, st)
		}
	}
	h.fake.storages = kept
	h.fake.mu.Unlock()
}

func nextFakeStorageID(t *testing.T, h *portalHarness) int64 {
	t.Helper()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	id := h.fake.nextID
	h.fake.nextID++
	return id
}

func setFakeStorageStatus(t *testing.T, h *portalHarness, id int64, status string) {
	t.Helper()
	h.fake.mu.Lock()
	for i := range h.fake.storages {
		if h.fake.storages[i].ID == id {
			h.fake.storages[i].Status = status
		}
	}
	h.fake.mu.Unlock()
}

// driveRowOf extracts one drive row out of a {drives:[...]} payload.
func driveRowOf(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	rows, ok := doc["drives"].([]any)
	if !ok {
		t.Fatalf("no drives array in %v", doc)
	}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("drive %q missing from %v", id, doc)
	return nil
}

// --- openlist_public_url validation (§地址配置) ---

func TestNormalizeOpenlistPublicURL(t *testing.T) {
	valid := []struct{ in, want string }{
		{"http://192.0.2.10:5244", "http://192.0.2.10:5244"},
		{"https://example.com/openlist/", "https://example.com/openlist"},
		{"  http://h:80/x/  ", "http://h:80/x"},
		{"http://h", "http://h"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range valid {
		got, err := NormalizeOpenlistPublicURL(tc.in)
		if err != nil {
			t.Errorf("NormalizeOpenlistPublicURL(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeOpenlistPublicURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	invalid := []string{
		"ftp://x",                 // scheme
		"http://u:p@h",            // userinfo
		"http://h/?q=1",           // query
		"http://h/#f",             // fragment
		"http://h/?",              // bare ?
		"http://h/#",              // bare #
		"http://h/@manage",        // already the manage entry
		"https://h/pre/@manage/x", // path prefix + /@manage
		"http://",                 // missing host
		"example.com",             // not absolute
		"//example.com",           // scheme-relative
		"://example.com",          // unparseable
		"localhost:5244",          // no scheme (parsed as scheme:opaque)
	}
	for _, in := range invalid {
		if _, err := NormalizeOpenlistPublicURL(in); err == nil {
			t.Errorf("NormalizeOpenlistPublicURL(%q) accepted an invalid value", in)
		}
	}
}

func TestPutSettingsValidatesOpenlistPublicURLBeforeSideEffects(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	before := h.fake.listCallCount()
	rec := doPortal(t, h.handler, http.MethodPut, "/admin/api/settings", cookie, map[string]any{
		"openlist_public_url": "ftp://x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid openlist_public_url PUT = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if msg := decodePortal(t, rec)["message"]; msg != "只支持 http:// 或 https:// 地址" {
		t.Errorf("validation message = %v, want the Chinese scheme message", msg)
	}
	// Nothing persisted, no OpenList reads, no storage writes.
	if _, ok := h.st.GetSetting("openlist_public_url"); ok {
		t.Error("invalid openlist_public_url was persisted")
	}
	if got := h.fake.listCallCount(); got != before {
		t.Errorf("settings PUT performed %d extra list calls, want 0", got-before)
	}
	if got := h.fake.writeCallCount(); got != 0 {
		t.Errorf("settings PUT performed %d storage writes, want 0", got)
	}

	// A valid value persists normalized.
	rec = doPortal(t, h.handler, http.MethodPut, "/admin/api/settings", cookie, map[string]any{
		"openlist_public_url": "  https://example.com/ol/  ",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid PUT = %d (%s)", rec.Code, rec.Body.String())
	}
	if v, ok := h.st.GetSetting("openlist_public_url"); !ok || v != "https://example.com/ol" {
		t.Errorf("stored openlist_public_url = %q ok=%v, want normalized https://example.com/ol", v, ok)
	}

	// The overview derives the entry link from the stored value.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	ol := dataOf(t, decodePortal(t, rec))["openlist"].(map[string]any)
	if ol["public_url"] != "https://example.com/ol" {
		t.Errorf("overview public_url = %v", ol["public_url"])
	}
	if ol["manage_url"] != "https://example.com/ol/@manage" {
		t.Errorf("overview manage_url = %v", ol["manage_url"])
	}
	if ol["public_url_error"] != "" {
		t.Errorf("overview public_url_error = %v", ol["public_url_error"])
	}
}

func TestOverviewStoredURLInvalid(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	if err := h.st.SetSetting("openlist_public_url", "http://u:p@host/x"); err != nil {
		t.Fatal(err)
	}
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	ol := doc["openlist"].(map[string]any)
	if ol["manage_url"] != "" {
		t.Errorf("manage_url with an invalid stored value = %v, want \"\"", ol["manage_url"])
	}
	if ol["public_url_error"] == "" {
		t.Error("public_url_error empty for an invalid stored value")
	}
	found := false
	for _, raw := range doc["warnings"].([]any) {
		if w := raw.(map[string]any); w["action_url"] == "#/settings" {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning points at the settings page: %v", doc["warnings"])
	}
}

func TestSeedOpenlistPublicURLBehaviour(t *testing.T) {
	h := newPortalHarness(t)

	// Empty seeds nothing.
	if v, err := SeedOpenlistPublicURL(h.st, "  "); err != nil || v != "" {
		t.Fatalf("blank seed = (%q, %v), want (\"\", nil)", v, err)
	}
	// Valid value is normalized and persisted.
	v, err := SeedOpenlistPublicURL(h.st, " http://ol.example:5244/ ")
	if err != nil || v != "http://ol.example:5244" {
		t.Fatalf("seed = (%q, %v)", v, err)
	}
	if got, ok := h.st.GetSetting("openlist_public_url"); !ok || got != "http://ol.example:5244" {
		t.Errorf("stored = %q ok=%v", got, ok)
	}
	// Invalid value keeps the stored one.
	if _, err := SeedOpenlistPublicURL(h.st, "ftp://nope"); err == nil {
		t.Error("invalid seed accepted")
	}
	if got, _ := h.st.GetSetting("openlist_public_url"); got != "http://ol.example:5244" {
		t.Errorf("stored value changed by an invalid seed: %q", got)
	}
}

// --- GET /admin/api/drives: rows from one snapshot ---

func TestDrivesRowsEditURLAndReplacement(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	// A stored value with a trailing slash: links use the normalized base.
	if err := h.st.SetSetting("openlist_public_url", "http://192.0.2.10:5244/"); err != nil {
		t.Fatal(err)
	}
	good := seedDrive(t, h, "good1", "/d/good", "Local")
	rOld := seedDrive(t, h, "r1", "/d/r", "WebDav")
	orph := seedDrive(t, h, "orph1", "/d/orph", "Local")

	// r1's storage was deleted; a different driver now sits at its mount.
	removeFakeStorage(t, h, rOld)
	rNew := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: rNew, MountPath: "/d/r", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	// orph1's storage is gone with nothing at the mount.
	removeFakeStorage(t, h, orph)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	if _, has := doc["snapshot_error"]; has {
		t.Errorf("snapshot_error on a healthy read: %v", doc["snapshot_error"])
	}

	row := driveRowOf(t, doc, "good1")
	if row["state"] != "healthy" {
		t.Errorf("good1 state = %v", row["state"])
	}
	if want := "http://192.0.2.10:5244/@manage/storages/edit/" + strconv.FormatInt(good, 10); row["openlist_edit_url"] != want {
		t.Errorf("good1 openlist_edit_url = %v, want %q", row["openlist_edit_url"], want)
	}
	if row["replacement"] != nil {
		t.Errorf("good1 replacement = %v, want null", row["replacement"])
	}

	row = driveRowOf(t, doc, "r1")
	if row["state"] != storageid.StateReplacementFound {
		t.Fatalf("r1 state = %v, want replacement_found", row["state"])
	}
	repl, ok := row["replacement"].(map[string]any)
	if !ok {
		t.Fatalf("r1 replacement = %v, want the candidate object", row["replacement"])
	}
	if int64(repl["storage_id"].(float64)) != rNew || repl["driver"] != "Local" ||
		repl["mount_path"] != "/d/r" || repl["status"] != "work" || repl["disabled"] != false {
		t.Errorf("r1 replacement = %v", repl)
	}
	if repl["driver_changed"] != true {
		t.Errorf("r1 replacement driver_changed = %v, want true (WebDav -> Local)", repl["driver_changed"])
	}
	if row["openlist_edit_url"] != "" {
		t.Errorf("r1 openlist_edit_url = %v, want \"\" (no matched storage)", row["openlist_edit_url"])
	}

	row = driveRowOf(t, doc, "orph1")
	if row["state"] != "orphaned" || row["replacement"] != nil {
		t.Errorf("orph1 row = %v", row)
	}

	// Without a configured base the edit URL is empty even when matched.
	if err := h.st.SetSetting("openlist_public_url", ""); err != nil {
		t.Fatal(err)
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	doc = dataOf(t, decodePortal(t, rec))
	if got := driveRowOf(t, doc, "good1")["openlist_edit_url"]; got != "" {
		t.Errorf("openlist_edit_url without a base = %v, want \"\"", got)
	}
}

func TestDrivesSnapshotErrorFallsBackToCache(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	seedDrive(t, h, "good1", "/d/good", "Local")

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first drives read = %d (%s)", rec.Code, rec.Body.String())
	}

	// OpenList goes down: the cached snapshot still serves the rows with
	// snapshot_error set.
	h.fake.mu.Lock()
	h.fake.failList = true
	h.fake.mu.Unlock()
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded drives read = %d (%s), want 200 from the cache", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	if doc["snapshot_error"] == "" {
		t.Error("snapshot_error missing on a degraded read")
	}
	if row := driveRowOf(t, doc, "good1"); row["state"] != "healthy" {
		t.Errorf("cached row state = %v, want healthy", row["state"])
	}

	// No cache at all → 502.
	h2 := newPortalHarness(t)
	h2.fake.mu.Lock()
	h2.fake.failList = true
	h2.fake.mu.Unlock()
	rec = doPortal(t, h2.handler, http.MethodGet, "/admin/api/drives", h2.login(t, adminPassword), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("drives without cache or OpenList = %d (%s), want 502", rec.Code, rec.Body.String())
	}
}

// --- POST /admin/api/drives/sync: ONE snapshot drives everything ---

func TestSyncUsesExactlyOneListingPass(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	h.p.SetReconciler(reconcile.New(h.st, h.registry, h.p.client, nil))

	// Five storages, forced page size 2 → one full paged pass = 3 requests.
	h.fake.mu.Lock()
	h.fake.perPage = 2
	h.fake.storages = append(h.fake.storages,
		fakeStorage{ID: 1, MountPath: "/m1", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"},
		fakeStorage{ID: 2, MountPath: "/u1", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"},
		fakeStorage{ID: 3, MountPath: "/u2", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"},
		fakeStorage{ID: 4, MountPath: "/u3", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"},
		fakeStorage{ID: 5, MountPath: "/u4", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"},
	)
	h.fake.mu.Unlock()

	before := h.fake.listCallCount()
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := h.fake.listCallCount() - before; got != 3 {
		t.Fatalf("sync performed %d storage/list calls, want exactly 3 (one paged pass over 5 storages in pages of 2)", got)
	}
	doc := dataOf(t, decodePortal(t, rec))
	ts, _ := doc["synced_at"].(string)
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("synced_at = %q, want RFC3339", ts)
	}

	// Both lists come from the same snapshot: static1 was adopted onto
	// storage 1 by the reconcile pass, /m1 is not unmanaged, and no storage
	// id appears on both sides.
	row := driveRowOf(t, doc, "static1")
	if row["state"] != "healthy" || int64(row["storage_id"].(float64)) != 1 {
		t.Errorf("static1 row after sync = %v, want adopted storage 1 healthy", row)
	}
	claimed := map[int64]bool{1: true}
	unmanaged, _ := doc["unmanaged"].([]any)
	if len(unmanaged) != 4 {
		t.Fatalf("unmanaged after sync = %v, want the four /u* storages", unmanaged)
	}
	for _, raw := range unmanaged {
		item := raw.(map[string]any)
		id := int64(item["storage_id"].(float64))
		if claimed[id] {
			t.Errorf("storage %d is both managed and unmanaged", id)
		}
		if item["mount_path"] == "/m1" {
			t.Errorf("adopted mount /m1 listed as unmanaged: %v", item)
		}
		if item["disabled"] == nil {
			t.Errorf("unmanaged item lacks disabled: %v", item)
		}
	}
}

func TestSyncFailureAnswers502(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	h.fake.mu.Lock()
	h.fake.failList = true
	h.fake.mu.Unlock()
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("sync with OpenList down = %d (%s), want 502", rec.Code, rec.Body.String())
	}
	if msg := decodePortal(t, rec)["message"]; msg == "" || msg == "success" {
		t.Errorf("sync failure message = %v", msg)
	}
}

// --- unmanaged listing + adopt semantics ---

func TestUnmanagedReplacementForAndAdoptMountGuard(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	// r1's storage was replaced: the candidate is listed with replacement_for
	// and is NOT adoptable (confirm via rebind instead).
	rOld := seedDrive(t, h, "r1", "/d/r", "WebDav")
	removeFakeStorage(t, h, rOld)
	cand := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: cand, MountPath: "/d/r", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})

	// ok1 keeps its storage; a squatter shares the mount (not adoptable
	// either — the mount belongs to an existing drive).
	seedDrive(t, h, "ok1", "/d/ok", "Local")
	squatter := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: squatter, MountPath: "/d/ok", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives/unmanaged", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	items, _ := doc["storages"].([]any)
	if len(items) != 1 {
		t.Fatalf("unmanaged = %v, want only the replacement candidate", items)
	}
	item := items[0].(map[string]any)
	if int64(item["storage_id"].(float64)) != cand || item["replacement_for"] != "r1" {
		t.Errorf("candidate row = %v, want storage %d as r1's replacement", item, cand)
	}

	// Adopting the replacement candidate → 409 pointing at the rebind flow.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": cand, "id": "new1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("adopt replacement candidate = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if msg := decodePortal(t, rec)["message"].(string); !strings.Contains(msg, "确认使用替代存储") {
		t.Errorf("adopt replacement message = %q, want it to point at 确认使用替代存储", msg)
	}

	// Adopting a storage whose mount belongs to a healthy drive → 409 too.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": squatter, "id": "new2"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("adopt mount squatter = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if _, err := h.st.GetDrive("new1"); err == nil {
		t.Error("a rejected adopt persisted a drive")
	}
}

func TestAdoptReturnsRealState(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	broken := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: broken, MountPath: "/d/err", Driver: "Local",
		Status: "failed get fs: upstream timeout", Modified: "2026-10-13T00:00:00Z"})
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": broken, "id": "err1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt broken storage = %d (%s)", rec.Code, rec.Body.String())
	}
	data := dataOf(t, decodePortal(t, rec))
	if data["state"] != "error" || data["status_text"] != "failed get fs: upstream timeout" {
		t.Errorf("adopted broken row = %v, want the storage's real error state", data)
	}
	if int64(data["storage_id"].(float64)) != broken || data["driver"] != "Local" {
		t.Errorf("adopted row identity = %v", data)
	}
	if data["replacement"] != nil || data["openlist_edit_url"] != "" {
		t.Errorf("adopted row extra fields = %v", data)
	}

	// A disabled storage may still be adopted, showing state disabled.
	off := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: off, MountPath: "/d/off", Driver: "Local",
		Status: "work", Disabled: true, Modified: "2026-10-13T00:00:00Z"})
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": off, "id": "off1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt disabled storage = %d (%s)", rec.Code, rec.Body.String())
	}
	if data := dataOf(t, decodePortal(t, rec)); data["state"] != "disabled" {
		t.Errorf("adopted disabled row state = %v, want disabled", data["state"])
	}
}

// --- POST /admin/api/drives/{id}/rebind ---

func TestRebindDrive(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	if err := h.st.SetSetting("openlist_public_url", "http://ol.example:5244"); err != nil {
		t.Fatal(err)
	}
	// A key granted on r1: existing grants must keep applying after rebind.
	if _, _, err := h.keys.Create(store.APIKey{
		ID:     "rk",
		Drives: []store.KeyDrive{{Drive: "r1", Root: "/", Perms: []string{"list", "read"}}},
		Trash:  store.TrashSettings{Enabled: true, Days: 7},
	}); err != nil {
		t.Fatal(err)
	}

	// Original storage still exists → 409.
	still := seedDrive(t, h, "still1", "/d/still", "Local")
	other := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: other, MountPath: "/d/still", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/still1/rebind", cookie, map[string]any{"storage_id": other})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rebind while original exists = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if msg := decodePortal(t, rec)["message"].(string); !strings.Contains(msg, "仍存在") ||
		!strings.Contains(msg, strconv.FormatInt(still, 10)) {
		t.Errorf("rebind original-exists message = %q", msg)
	}

	// Unknown drive → 404.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/ghost/rebind", cookie, map[string]any{"storage_id": other})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rebind unknown drive = %d (%s), want 404", rec.Code, rec.Body.String())
	}

	// Candidate at another mount → 409 asking to refresh.
	mv := seedDrive(t, h, "mv1", "/d/mv", "Local")
	removeFakeStorage(t, h, mv)
	away := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: away, MountPath: "/d/away", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/mv1/rebind", cookie, map[string]any{"storage_id": away})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rebind off-mount candidate = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// Candidate claimed by another drive → 409.
	r2old := seedDrive(t, h, "r2", "/d/r2", "WebDav")
	removeFakeStorage(t, h, r2old)
	q := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: q, MountPath: "/d/r2", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	if err := h.st.InsertDrive(store.Drive{
		ID: "t2", Provider: "local", Driver: "Local", StorageID: q, MountPath: "/d/t2",
		Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.registry.Add(config.Account{ID: "t2", Provider: "local", MountPath: "/d/t2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/r2/rebind", cookie, map[string]any{"storage_id": q})
	if rec.Code != http.StatusConflict {
		t.Fatalf("rebind claimed candidate = %d (%s), want 409", rec.Code, rec.Body.String())
	}

	// Unknown candidate storage → 404.
	rOld := seedDrive(t, h, "r1", "/d/r", "WebDav")
	removeFakeStorage(t, h, rOld)
	rNew := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: rNew, MountPath: "/d/r", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/r1/rebind", cookie, map[string]any{"storage_id": 424242})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("rebind unknown candidate = %d (%s), want 404", rec.Code, rec.Body.String())
	}

	// Success.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/r1/rebind", cookie, map[string]any{"storage_id": rNew})
	if rec.Code != http.StatusOK {
		t.Fatalf("rebind = %d (%s)", rec.Code, rec.Body.String())
	}
	data := dataOf(t, decodePortal(t, rec))
	if data["state"] != "healthy" || int64(data["storage_id"].(float64)) != rNew || data["driver"] != "Local" {
		t.Errorf("rebound row = %v", data)
	}
	if want := "http://ol.example:5244/@manage/storages/edit/" + strconv.FormatInt(rNew, 10); data["openlist_edit_url"] != want {
		t.Errorf("rebound openlist_edit_url = %v, want %q", data["openlist_edit_url"], want)
	}
	if data["replacement"] != nil {
		t.Errorf("rebound replacement = %v, want null", data["replacement"])
	}
	drv, err := h.st.GetDrive("r1")
	if err != nil || drv.StorageID != rNew || drv.Driver != "Local" {
		t.Fatalf("stored rebound drive = %+v err=%v", drv, err)
	}
	// The event names old → new, both drivers, the mount and the grant note.
	events, _ := h.st.ListEvents(20)
	found := false
	for _, ev := range events {
		if ev.Drive == "r1" && ev.Kind == "storage_rebound" {
			found = true
			for _, want := range []string{strconv.FormatInt(rOld, 10), strconv.FormatInt(rNew, 10), "WebDav", "Local", "/d/r", "key grants"} {
				if !strings.Contains(ev.Detail, want) {
					t.Errorf("storage_rebound detail %q lacks %q", ev.Detail, want)
				}
			}
		}
	}
	if !found {
		t.Fatal("storage_rebound event missing")
	}
	// Key grants unchanged.
	k, err := h.keys.Get("rk")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Drives) != 1 || k.Drives[0].Drive != "r1" || k.Drives[0].Root != "/" || len(k.Drives[0].Perms) != 2 {
		t.Errorf("key grants after rebind = %+v", k.Drives)
	}
	// The candidate no longer shows up as unmanaged.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives/unmanaged", cookie, nil)
	for _, raw := range dataOf(t, decodePortal(t, rec))["storages"].([]any) {
		if item := raw.(map[string]any); int64(item["storage_id"].(float64)) == rNew {
			t.Errorf("rebound candidate still unmanaged: %v", item)
		}
	}
}

// --- overview attention lists ---

func TestOverviewAttentionCountsReplacementFound(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	seedDrive(t, h, "good1", "/d/good", "Local")
	rOld := seedDrive(t, h, "rep1", "/d/rep", "Local")
	removeFakeStorage(t, h, rOld)
	cand := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: cand, MountPath: "/d/rep", Driver: "Onedrive", Status: "work", Modified: "2026-10-13T00:00:00Z"})

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	sawReplacement, sawGood := false, false
	for _, raw := range doc["warnings"].([]any) {
		w := raw.(map[string]any)
		text, _ := w["text"].(string)
		if strings.Contains(text, "rep1") && strings.Contains(text, "替代") {
			sawReplacement = true
		}
		if strings.Contains(text, "good1") {
			sawGood = true
		}
	}
	if !sawReplacement {
		t.Errorf("no attention warning for the replacement_found drive: %v", doc["warnings"])
	}
	if sawGood {
		t.Errorf("healthy drive listed as needing attention: %v", doc["warnings"])
	}
}

func TestStatusForAccountMapsReplacement(t *testing.T) {
	if got := statusForAccount(driveRow{State: storageid.StateReplacementFound}); got != "storage replaced; confirmation pending" {
		t.Errorf("statusForAccount(replacement_found) = %q", got)
	}
	if got := statusForAccount(driveRow{State: "healthy"}); got != "work" {
		t.Errorf("statusForAccount(healthy) = %q", got)
	}
	if got := statusForAccount(driveRow{State: "orphaned"}); got != "storage missing" {
		t.Errorf("statusForAccount(orphaned) = %q", got)
	}
}

// A rebind clears the persisted replacement_found itself, so the row is
// right even when no reconcile pass follows.
func TestRebindClearsPersistedReplacementState(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	old := seedDrive(t, h, "rs1", "/d/rs", "WebDav")
	removeFakeStorage(t, h, old)
	cand := nextFakeStorageID(t, h)
	addFakeStorage(t, h, fakeStorage{ID: cand, MountPath: "/d/rs", Driver: "Local", Status: "work", Modified: "2026-10-13T00:00:00Z"})
	if err := h.st.ApplyReconcile("rs1", "replacement_found", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/rs1/rebind", cookie, map[string]any{"storage_id": cand})
	if rec.Code != http.StatusOK {
		t.Fatalf("rebind = %d %s", rec.Code, rec.Body.String())
	}
	if row := dataOf(t, decodePortal(t, rec)); row["state"] != "healthy" {
		t.Fatalf("row state after rebind = %v", row["state"])
	}
	d, err := h.st.GetDrive("rs1")
	if err != nil || d.State == "replacement_found" || d.StorageID != cand {
		t.Fatalf("store after rebind = %+v %v", d, err)
	}
}
