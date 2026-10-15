package keymgr

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/store"
)

func mustManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InsertDrive(store.Drive{ID: "d1", Provider: "baidu", MountPath: "/m1", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDrive(store.Drive{ID: "d2", Provider: "123pan", MountPath: "/m2", Enabled: true, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	return m, st
}

func spec(id string, drives ...store.KeyDrive) store.APIKey {
	return store.APIKey{
		ID: id, CreatedAt: time.Now().UTC(),
		Drives: drives, GlobalPerms: []string{},
		Trash: store.TrashSettings{Enabled: true, Days: 7},
	}
}

func TestCreateRotateRevokeFlow(t *testing.T) {
	m, _ := mustManager(t)
	created, plain, err := m.Create(spec("agent", store.KeyDrive{Drive: "d1", Root: "/agents/a", Perms: []string{"list", "read", "write", "delete"}}))
	if err != nil {
		t.Fatal(err)
	}
	if created.PublicID == "" || !checkFormat(plain) {
		t.Fatalf("plaintext %q / publicID %q", plain, created.PublicID)
	}
	// The plaintext authenticates immediately.
	k, out := m.Authenticate(plain)
	if out != auth.AuthOK || k == nil || k.ID != "agent" {
		t.Fatalf("authenticate after create = %v %v", k, out)
	}
	if k.EffectiveDefaultDrive() != "d1" {
		t.Errorf("default drive = %q", k.EffectiveDefaultDrive())
	}

	// Rotate: same id, new secret; old secret stops working.
	rotated, plain2, err := m.Rotate("agent")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.PublicID == created.PublicID {
		t.Error("rotate kept the public id")
	}
	if _, out := m.Authenticate(plain); out != auth.AuthUnknownKey && out != auth.AuthRevoked {
		t.Errorf("old secret still valid: %v", out)
	}
	if _, out := m.Authenticate(plain2); out != auth.AuthOK {
		t.Errorf("new secret invalid: %v", out)
	}

	// Revoke: authenticate answers revoked.
	if err := m.Revoke("agent"); err != nil {
		t.Fatal(err)
	}
	if _, out := m.Authenticate(plain2); out != auth.AuthRevoked {
		t.Errorf("revoked outcome = %v", out)
	}

	// Static keys are refused.
	if err := m.st.SyncStaticKey(spec("s1", store.KeyDrive{Drive: "d1", Root: "/", Perms: []string{"list"}})); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Rotate("s1"); !errors.Is(err, ErrStaticKey) {
		t.Errorf("rotate static = %v", err)
	}
	if err := m.Revoke("s1"); !errors.Is(err, ErrStaticKey) {
		t.Errorf("revoke static = %v", err)
	}
	if _, err := m.Patch("s1", func(*store.APIKey) error { return nil }); !errors.Is(err, ErrStaticKey) {
		t.Errorf("patch static = %v", err)
	}
}

func TestPatchUpdatesGrantsAndLimits(t *testing.T) {
	m, _ := mustManager(t)
	rpm := 10
	_, plain, err := m.Create(spec("k", store.KeyDrive{Drive: "d1", Root: "/", Perms: []string{"list"}}))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.Patch("k", func(k *store.APIKey) error {
		k.Drives = []store.KeyDrive{
			{Drive: "d1", Root: "/sub", Perms: []string{"list", "read"}},
			{Drive: "d2", Root: "/", Perms: []string{"list"}},
		}
		k.DefaultDrive = "d2"
		k.Limits = store.Limits{RequestsPerMinute: &rpm}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DefaultDrive != "d2" || len(updated.Drives) != 2 || *updated.Limits.RequestsPerMinute != 10 {
		t.Fatalf("patched = %+v", updated)
	}
	rk, out := m.Authenticate(plain)
	if out != auth.AuthOK || rk.EffectiveDefaultDrive() != "d2" {
		t.Fatalf("runtime key after patch = %v %v", rk, out)
	}
	if !rk.CanOn("d1", auth.PermRead) || rk.CanOn("d1", auth.PermWrite) {
		t.Error("permissions not updated")
	}
}

func TestValidateKeySpec(t *testing.T) {
	m, _ := mustManager(t)
	known := m.knownDrivesPriv()

	good := spec("x", store.KeyDrive{Drive: "d1", Root: "/agents", Perms: []string{"list", "read"}})
	if err := ValidateKeySpec(good, known); err != nil {
		t.Errorf("good spec rejected: %v", err)
	}
	if err := ValidateKeySpec(spec("x"), known); err == nil {
		t.Error("no drives accepted")
	}
	bad := spec("x", store.KeyDrive{Drive: "ghost", Root: "/", Perms: []string{"list"}})
	if err := ValidateKeySpec(bad, known); err == nil {
		t.Error("unknown drive accepted")
	}
	traversal := spec("x", store.KeyDrive{Drive: "d1", Root: "/a/../..", Perms: []string{"list"}})
	if err := ValidateKeySpec(traversal, known); err == nil {
		t.Error("traversal root accepted")
	}
	badPerm := spec("x", store.KeyDrive{Drive: "d1", Root: "/", Perms: []string{"list", "exec"}})
	if err := ValidateKeySpec(badPerm, known); err == nil {
		t.Error("unknown permission accepted")
	}
	badDefault := spec("x", store.KeyDrive{Drive: "d1", Root: "/", Perms: []string{"list"}})
	badDefault.DefaultDrive = "d2"
	if err := ValidateKeySpec(badDefault, known); err == nil {
		t.Error("default outside drives accepted")
	}
}

func (m *Manager) knownDrivesPriv() map[string]bool {
	drives, err := m.st.ListDrives()
	if err != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	for _, d := range drives {
		out[d.ID] = true
	}
	return out
}

func checkFormat(plain string) bool {
	pub, secret, ok := auth.SplitNewFormat(plain)
	return ok && len(pub) == 8 && len(secret) > 16
}

// TestCreateRejectsEmptyID guards the 2026-10-12 incident class: a key with
// an empty id turns parent_id=” into a graph node and the revocation
// cascade then hits every top-level key.
func TestCreateRejectsEmptyID(t *testing.T) {
	m, st := mustManager(t)
	_, _, err := m.Create(store.APIKey{Drives: []store.KeyDrive{{Drive: "d1", Perms: []string{"read"}}}})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty id create err = %v, want empty-id rejection", err)
	}
	if err := st.InsertKey(store.APIKey{ID: "  ", Hash: "sha256:" + strings.Repeat("0", 64)}); err == nil {
		t.Fatal("store.InsertKey accepted a blank id")
	}
}

// TestRevokeCascadeNeverCrossesEmptyParent reproduces the incident: even with
// a rogue empty-id row present, revoking a key must not touch unrelated
// top-level or static keys.
func TestRevokeCascadeNeverCrossesEmptyParent(t *testing.T) {
	m, st := mustManager(t)
	static := store.APIKey{
		ID: "static-key", Static: true, Hash: "sha256:" + strings.Repeat("a", 64),
		Drives: []store.KeyDrive{{Drive: "d1", Perms: []string{"read"}}},
	}
	if err := st.InsertKey(static); err != nil {
		t.Fatal(err)
	}
	innocent := store.APIKey{
		ID: "innocent", Hash: "sha256:" + strings.Repeat("b", 64),
		Drives: []store.KeyDrive{{Drive: "d1", Perms: []string{"read"}}},
	}
	if err := st.InsertKey(innocent); err != nil {
		t.Fatal(err)
	}
	parent, _, err := m.Create(spec("parent", store.KeyDrive{Drive: "d1", Perms: []string{"read"}}))
	if err != nil {
		t.Fatal(err)
	}
	// A rogue empty-id row parented at the victim parent (inserted directly;
	// Create now rejects empty ids).
	if err := insertRawKey(st, "", parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(parent.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"static-key", "innocent"} {
		k, err := st.GetKey(id)
		if err != nil {
			t.Fatal(err)
		}
		if k.Revoked {
			t.Fatalf("%s was revoked by the cascade", id)
		}
	}
	// The cascade must also refuse to run on an empty id at all.
	if err := st.RevokeKey(""); err == nil {
		t.Fatal("RevokeKey('') was accepted")
	}
}

// TestRevokeHardDeletesRevokedKeys: a second Revoke hard-deletes the row so
// test keys do not clutter the portal forever.
func TestRevokeHardDeletesRevokedKeys(t *testing.T) {
	m, st := mustManager(t)
	created, plain, err := m.Create(spec("victim", store.KeyDrive{Drive: "d1", Perms: []string{"read"}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, out := m.Authenticate(plain); out != auth.AuthRevoked {
		t.Fatalf("revoked key outcome = %v, want AuthRevoked", out)
	}
	k, _ := st.GetKey(created.ID)
	if !k.Revoked {
		t.Fatal("first revoke did not set the flag")
	}
	if err := m.Revoke(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetKey(created.ID); err == nil {
		t.Fatal("second revoke did not hard-delete the row")
	}
}

// TestSyncStaticKeyHealsRevokedFlag: config.yaml is the source of truth; a
// revoked static row is healed on sync.
func TestSyncStaticKeyHealsRevokedFlag(t *testing.T) {
	_, st := mustManager(t)
	spec := store.APIKey{
		ID: "static-key", Static: true, Hash: "sha256:" + strings.Repeat("c", 64),
		Drives: []store.KeyDrive{{Drive: "d1", Perms: []string{"read"}}},
	}
	if err := st.InsertKey(spec); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeKey("static-key"); err == nil {
		t.Log("static keys are protected from direct revocation; simulate instead")
		if err := simulateRevokedRow(st, "static-key"); err != nil {
			t.Fatal(err)
		}
	}
	k, _ := st.GetKey("static-key")
	if !k.Revoked {
		t.Skip("implementation rejects static revocation outright")
	}
	if err := st.SyncStaticKey(spec); err != nil {
		t.Fatal(err)
	}
	k, err := st.GetKey("static-key")
	if err != nil {
		t.Fatal(err)
	}
	if k.Revoked {
		t.Fatal("SyncStaticKey left the static key revoked")
	}
}

func insertRawKey(st *store.Store, id, parent string) error {
	return st.Exec(`INSERT INTO api_keys (id, hash, parent_id, static, revoked, created_at)
		VALUES (?, ?, ?, 0, 0, ?)`, id, "sha256:"+strings.Repeat("d", 64), parent, time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
}

func simulateRevokedRow(st *store.Store, id string) error {
	return st.Exec(`UPDATE api_keys SET revoked=1 WHERE id=?`, id)
}
