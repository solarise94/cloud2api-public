package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustOpen(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	// Reopening must not wipe data.
	if err := s2.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
}

func TestDriveCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	d := Drive{ID: "baidu-main", Provider: "baidu", Driver: "BaiduNetdisk", StorageID: 3, MountPath: "/baidu", Enabled: true, Static: true, CreatedAt: time.Now()}
	if err := s.InsertDrive(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDrive("baidu-main")
	if err != nil || got.Provider != "baidu" || got.StorageID != 3 || !got.Static {
		t.Fatalf("GetDrive = %+v %v", got, err)
	}
	if err := s.SetDriveEnabled("baidu-main", false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDrive("baidu-main")
	if got.Enabled {
		t.Error("disable failed")
	}
	if err := s.SetDriveEnabled("ghost", true); err != ErrNotFound {
		t.Errorf("unknown drive err = %v", err)
	}
	if err := s.UpdateDrive(Drive{ID: "baidu-main", Provider: "baidu", MountPath: "/bd", Enabled: true, Static: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetDrive("baidu-main")
	if got.MountPath != "/bd" {
		t.Error("update failed")
	}
	if err := s.DeleteDrive("baidu-main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDrive("baidu-main"); err != ErrNotFound {
		t.Errorf("delete failed: %v", err)
	}
}

func i64(v int64) *int64 { return &v }
func intp(v int) *int    { return &v }

func TestKeyLifecycleAndRevokeCascade(t *testing.T) {
	s, _ := mustOpen(t)
	parent := APIKey{
		ID: "parent", PublicID: "aaaa1111", Hash: "sha256:" + hex64("a"),
		CreatedAt: time.Now(), GlobalPerms: []string{"link"},
		Drives: []KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list", "read"}}},
		Trash:  TrashSettings{Enabled: true, Days: 7},
	}
	if err := s.InsertKey(parent); err != nil {
		t.Fatal(err)
	}
	child := parent
	child.ID, child.ParentID, child.PublicID = "child", "parent", "bbbb2222"
	child.Hash = "sha256:" + hex64("b")
	if err := s.InsertKey(child); err != nil {
		t.Fatal(err)
	}
	grandchild := child
	grandchild.ID, grandchild.ParentID, grandchild.PublicID = "grandchild", "child", "cccc3333"
	grandchild.Hash = "sha256:" + hex64("c")
	if err := s.InsertKey(grandchild); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetKeyWithGrants("parent")
	if err != nil || len(got.Drives) != 1 || got.Drives[0].Drive != "d1" {
		t.Fatalf("GetKeyWithGrants = %+v %v", got, err)
	}

	// Update replaces drives and limits.
	got.Limits = Limits{MaxUploadBytes: i64(1024), RequestsPerMinute: intp(30)}
	got.Drives = []KeyDrive{
		{Drive: "d1", Root: "/a", Perms: []string{"list"}},
		{Drive: "d2", Root: "/", Perms: []string{"read"}},
	}
	if err := s.UpdateKey(got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetKeyWithGrants("parent")
	if len(got.Drives) != 2 || got.Drives[0].Root != "/a" || *got.Limits.MaxUploadBytes != 1024 {
		t.Fatalf("update did not stick: %+v", got)
	}

	// Revoke cascades parent -> child -> grandchild and clears idempotency.
	if err := s.PutIdempotency(IdempotencyRecord{KeyID: "grandchild", IdemKey: "k", Method: "PUT", Route: "/v1/files", Status: 200, Body: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey("parent"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"parent", "child", "grandchild"} {
		k, err := s.GetKey(id)
		if err != nil || !k.Revoked {
			t.Errorf("%s not revoked: %+v %v", id, k, err)
		}
	}
	if _, err := s.GetIdempotency("grandchild", "k", time.Hour); err != ErrNotFound {
		t.Error("idempotency rows survived revocation")
	}

	// Rotate replaces the secret and unrevokes.
	if err := s.RotateKeyHash("child", "dddd4444", "sha256:"+hex64("d")); err != nil {
		t.Fatal(err)
	}
	k, _ := s.GetKey("child")
	if k.PublicID != "dddd4444" || k.Revoked {
		t.Fatalf("rotate = %+v", k)
	}
	if err := s.RotateKeyHash("ghost", "x", "y"); err != ErrNotFound {
		t.Error("rotating an unknown key must fail")
	}
}

func TestSyncStaticKeyAndPrune(t *testing.T) {
	s, _ := mustOpen(t)
	spec := APIKey{ID: "static1", Hash: "sha256:" + hex64("1"), CreatedAt: time.Now(),
		Drives: []KeyDrive{{Drive: "d1", Root: "/", Perms: []string{"list"}}}}
	if err := s.SyncStaticKey(spec); err != nil {
		t.Fatal(err)
	}
	// Hash change is picked up.
	spec.Hash = "sha256:" + hex64("2")
	if err := s.SyncStaticKey(spec); err != nil {
		t.Fatal(err)
	}
	k, _ := s.GetKey("static1")
	if k.Hash != spec.Hash || !k.Static {
		t.Fatalf("sync = %+v", k)
	}
	// Disappearing from config removes the row.
	if err := s.DeleteStaticKeysNotIn([]string{"other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetKey("static1"); err != ErrNotFound {
		t.Error("stale static key survived")
	}
}

func TestIdempotencyTTL(t *testing.T) {
	s, _ := mustOpen(t)
	old := IdempotencyRecord{KeyID: "k", IdemKey: "i1", Method: "PUT", Route: "/v1/files", Status: 200, Body: []byte(`{"a":1}`), CreatedAt: time.Now().Add(-2 * time.Hour)}
	if err := s.PutIdempotency(old); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.IdemKey, fresh.CreatedAt, fresh.Body = "i2", time.Now(), []byte(`{"a":2}`)
	if err := s.PutIdempotency(fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdempotency("k", "i1", time.Hour); err != ErrNotFound {
		t.Error("expired record replayed")
	}
	got, err := s.GetIdempotency("k", "i2", time.Hour)
	if err != nil || got.Status != 200 || string(got.Body) != `{"a":2}` {
		t.Fatalf("fresh record = %+v %v", got, err)
	}
	if err := s.PruneIdempotency(30 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdempotency("k", "i1", 24*time.Hour); err != ErrNotFound {
		t.Error("prune did not drop the old record")
	}
}

func TestAuditQueryAndPaging(t *testing.T) {
	s, _ := mustOpen(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.InsertAudit(AuditEntry{
			TS: base.Add(time.Duration(i) * time.Minute), KeyID: "k1", Drive: "d1",
			Op: "list", Path: "/", Status: 200, DurationMS: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertAudit(AuditEntry{KeyID: "k2", Drive: "d2", Op: "upload", Status: 413}); err != nil {
		t.Fatal(err)
	}
	entries, next, err := s.QueryAudit(AuditFilter{Key: "k1", Limit: 2})
	if err != nil || len(entries) != 2 || next == "" {
		t.Fatalf("page1 = %d entries, next=%q err=%v", len(entries), next, err)
	}
	// Newest first.
	if entries[0].DurationMS < entries[1].DurationMS {
		t.Error("not newest-first")
	}
	page2, next2, err := s.QueryAudit(AuditFilter{Key: "k1", Before: next, Limit: 2})
	if err != nil || len(page2) != 2 || next2 == "" {
		t.Fatalf("page2 = %d, next=%q err=%v", len(page2), next2, err)
	}
	page3, _, err := s.QueryAudit(AuditFilter{Key: "k1", Before: next2, Limit: 2})
	if err != nil || len(page3) != 1 {
		t.Fatalf("page3 = %d err=%v", len(page3), err)
	}
	// Filters.
	if got, _, _ := s.QueryAudit(AuditFilter{Op: "upload"}); len(got) != 1 || got[0].KeyID != "k2" {
		t.Errorf("op filter = %+v", got)
	}
	if got, _, _ := s.QueryAudit(AuditFilter{Status: "413"}); len(got) != 1 {
		t.Errorf("status filter = %+v", got)
	}
	if err := s.PruneAudit(30 * time.Minute); err != nil {
		t.Fatal(err)
	}
	// The hour-old entries are pruned; the fresh one survives.
	if got, _, _ := s.QueryAudit(AuditFilter{}); len(got) != 1 {
		t.Errorf("prune left %d entries, want 1", len(got))
	}
}

func TestTrashAndUploadsAndLinks(t *testing.T) {
	s, _ := mustOpen(t)
	id, err := s.InsertTrashItem(TrashItem{
		KeyID: "k1", Drive: "d1", OrigPath: "/a/b.txt", TrashPath: "/.c2a-trash/2026-10-12/a/b.txt",
		PurgeAfter: time.Now().Add(-time.Minute),
	})
	if err != nil || id == 0 {
		t.Fatalf("insert trash = %d %v", id, err)
	}
	items, err := s.ListTrashForKey("k1")
	if err != nil || len(items) != 1 || items[0].OrigPath != "/a/b.txt" {
		t.Fatalf("list trash = %+v %v", items, err)
	}
	got, err := s.GetTrashItem(id, "k1")
	if err != nil || got.TrashPath == "" {
		t.Fatalf("get trash = %+v %v", got, err)
	}
	if _, err := s.GetTrashItem(id, "other-key"); err != ErrNotFound {
		t.Error("trash rows are key-scoped")
	}
	expired, _ := s.ExpiredTrash(time.Now())
	if len(expired) != 1 {
		t.Fatalf("expired = %+v", expired)
	}
	if err := s.DeleteTrashItem(id); err != nil {
		t.Fatal(err)
	}
	if expired, _ = s.ExpiredTrash(time.Now()); len(expired) != 0 {
		t.Error("trash row survived delete")
	}

	// Uploads: newest record wins.
	for i, crc := range []string{"crc1", "crc2"} {
		if err := s.RecordUpload(Upload{Drive: "d1", Path: "/f", Size: 10, CRC64: crc, SHA256: "sha", Mtime: "m", KeyID: "k"}); i == 1 && err != nil {
			t.Fatal(err)
		}
	}
	rec, err := s.LatestUploadFor("d1", "/f")
	if err != nil || rec.CRC64 != "crc2" {
		t.Fatalf("latest upload = %+v %v", rec, err)
	}
	if _, err := s.LatestUploadFor("d1", "/missing"); err != ErrNotFound {
		t.Error("missing upload record")
	}

	// Links + settings.
	if err := s.InsertLink(Link{Token: "tok", KeyID: "k1", Drive: "d1", Path: "/f", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.GetLink("tok"); err != nil || l.KeyID != "k1" {
		t.Fatalf("link = %+v %v", l, err)
	}
	if v, ok := s.GetSetting("nope"); ok || v != "" {
		t.Error("unknown setting returned")
	}
	if err := s.SetSetting("public_base_url", "http://x"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("public_base_url", "http://y"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.AllSettings()
	if all["public_base_url"] != "http://y" {
		t.Errorf("settings = %v", all)
	}
}

func TestImportLegacyState(t *testing.T) {
	s, _ := mustOpen(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	stateJSON := `{"accounts":[{"id":"dyn1","provider":"local","mount_path":"/dyn1","enabled":true,"storage_id":7,"created_at":"2026-10-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ImportLegacyState(s, path); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDrive("dyn1")
	if err != nil || d.StorageID != 7 || d.Static {
		t.Fatalf("imported drive = %+v %v", d, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state.json not renamed")
	}
	if _, err := os.Stat(path + ".migrated"); err != nil {
		t.Error(".migrated file missing")
	}
	// Re-running is a no-op (file gone), and existing rows win on conflict.
	if err := ImportLegacyState(s, path); err != nil {
		t.Fatal(err)
	}

	// Corrupt file aborts startup.
	corrupt := filepath.Join(dir, "state2.json")
	os.WriteFile(corrupt, []byte("not json"), 0o600)
	if err := ImportLegacyState(s, corrupt); err == nil {
		t.Error("corrupt state.json imported without error")
	}

	// Missing file is fine.
	if err := ImportLegacyState(s, filepath.Join(dir, "absent.json")); err != nil {
		t.Fatal(err)
	}
}

func hex64(seed string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = seed[i%len(seed)]
	}
	return string(out)
}
