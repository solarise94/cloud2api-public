package admin

import (
	"net/http"
	"testing"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/store"
)

// --- Phase 1c linkage: unmanaged, adopt, events ---

// seedDrive inserts a store drive row plus the matching fake storage and
// returns the storage id.
func seedDrive(t *testing.T, h *portalHarness, id, mount, driver string) int64 {
	t.Helper()
	h.fake.mu.Lock()
	st := fakeStorage{
		ID: h.fake.nextID, MountPath: mount, Driver: driver, Status: "work",
		Addition:        `{"root_folder_path":"/mnt/x","thumbnail":false}`,
		CacheExpiration: "30",
		Modified:        h.fake.nextModified(),
	}
	h.fake.nextID++
	h.fake.storages = append(h.fake.storages, st)
	h.fake.mu.Unlock()
	if err := h.st.InsertDrive(store.Drive{
		ID: id, Provider: "local", Driver: driver, StorageID: st.ID, MountPath: mount,
		Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.registry.Add(config.Account{ID: id, Provider: "local", MountPath: mount, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return st.ID
}

func TestUnmanagedAndAdopt(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	// One managed drive (static1 → storage 1) and one unmanaged storage.
	h.fake.mu.Lock()
	h.fake.storages = append(h.fake.storages,
		fakeStorage{ID: 1, MountPath: "/m1", Driver: "Local", Status: "work", Modified: "2026-10-12T00:00:00Z"},
		fakeStorage{ID: 42, MountPath: "/ol-manual", Driver: "Local", Status: "work", Addition: `{"root_folder_path":"/mnt/manual"}`, CacheExpiration: "30", Modified: "2026-10-12T00:00:00Z"},
	)
	h.fake.mu.Unlock()
	if err := h.st.SetDriveStorageID("static1", 1); err != nil {
		t.Fatal(err)
	}

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives/unmanaged", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged status %d: %s", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	rows := doc["storages"].([]any)
	if len(rows) != 1 {
		t.Fatalf("unmanaged rows = %v, want only /ol-manual", rows)
	}
	if rows[0].(map[string]any)["mount_path"] != "/ol-manual" {
		t.Fatalf("unmanaged row = %v", rows[0])
	}

	// Adopt it.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 42, "id": "adopted1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt status %d: %s", rec.Code, rec.Body.String())
	}
	drv, err := h.st.GetDrive("adopted1")
	if err != nil || drv.StorageID != 42 || drv.Static {
		t.Fatalf("adopted drive row = %+v err=%v", drv, err)
	}
	if _, ok := h.registry.Get("adopted1"); !ok {
		t.Fatal("adopted drive missing from registry")
	}
	// OpenList untouched: no storage write call captured.
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("adopt produced %d OpenList storage writes, want 0", got)
	}

	// Re-adopt the same storage → 409.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 42, "id": "other"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-adopt status = %d", rec.Code)
	}
	// Duplicate drive id → 409.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 99, "id": "adopted1"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup id status = %d", rec.Code)
	}
	// Unknown storage → 404.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 999, "id": "fresh"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown storage status = %d", rec.Code)
	}
	// Invalid drive id → 400.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 42, "id": "bad/id"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status = %d", rec.Code)
	}

	// Release (mapping-only delete) keeps the OpenList storage — with and
	// without the legacy keep_storage parameter.
	if rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/drives/adopted1", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("release status %d: %s", rec.Code, rec.Body.String())
	}
	// Re-adopt, then release again through the legacy keep_storage form.
	if rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 42, "id": "adopted1"}); rec.Code != http.StatusOK {
		t.Fatalf("re-adopt status %d: %s", rec.Code, rec.Body.String())
	}
	if rec = doPortal(t, h.handler, http.MethodDelete, "/admin/api/drives/adopted1?keep_storage=true", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("keep_storage release status %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := h.fake.storageByID(42); !ok {
		t.Fatal("release must keep the OpenList storage")
	}
	events, _ := h.st.ListEvents(10)
	found := false
	for _, ev := range events {
		if ev.Kind == "released" && ev.Drive == "adopted1" {
			found = true
		}
	}
	if !found {
		t.Fatal("release event missing")
	}
}

func TestEventsEndpoint(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	if err := h.st.InsertEvent("d1", "mount_renamed", "/a -> /b"); err != nil {
		t.Fatal(err)
	}
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/events?limit=5", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("events %d", rec.Code)
	}
	doc := dataOf(t, decodePortal(t, rec))
	rows := doc["events"].([]any)
	if len(rows) != 1 {
		t.Fatalf("events = %v", rows)
	}
	ev := rows[0].(map[string]any)
	if ev["kind"] != "mount_renamed" || ev["drive"] != "d1" || ev["detail"] != "/a -> /b" || ev["ts"] == "" {
		t.Fatalf("event row = %v", ev)
	}
}

func TestOverviewOpenListDetail(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview %d", rec.Code)
	}
	doc := dataOf(t, decodePortal(t, rec))
	ol := doc["openlist"].(map[string]any)
	if _, ok := ol["guest_enabled"]; !ok {
		t.Fatal("overview.openlist.guest_enabled missing")
	}
	if tv, ok := ol["tested_version"].(string); !ok || len(tv) == 0 || tv[0] != 'v' {
		t.Fatalf("tested_version = %v", ol["tested_version"])
	}
}

func TestVersionInTestedRange(t *testing.T) {
	cases := map[string]bool{
		"v4.2.6 (Commit: 2bdf16d) - Frontend: v4.2.6": true,
		"v4.2.9":  true,
		"v4.3.0":  false,
		"v3.9.1":  false,
		"garbage": false,
	}
	for version, want := range cases {
		if got := versionInTestedRange(version); got != want {
			t.Errorf("versionInTestedRange(%q) = %v, want %v", version, got, want)
		}
	}
}

// helpers ---------------------------------------------------------------

// dataOf digs the {code,message,data} envelope's data object.
func dataOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	d, ok := doc["data"].(map[string]any)
	if !ok {
		t.Fatalf("response data missing: %v", doc)
	}
	return d
}
