package store

import (
	"errors"
	"testing"
	"time"
)

func TestWebSessionCRUD(t *testing.T) {
	s, _ := mustOpen(t)
	now := time.Now().UTC()
	v := WebSession{
		TokenHash:     "hash-a",
		Kind:          SessionKindFiles,
		WorkspaceID:   "ws-1",
		SubjectID:     "key-1",
		CredentialTag: "tag-1",
		CreatedAt:     now,
		ExpiresAt:     now.Add(24 * time.Hour),
	}
	if err := s.PutWebSession(v); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebSession("hash-a", SessionKindFiles, "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectID != "key-1" || got.CredentialTag != "tag-1" ||
		!got.ExpiresAt.Equal(v.ExpiresAt) || got.CreatedAt.IsZero() {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, v)
	}
	if _, err := s.GetWebSession("hash-a", SessionKindFiles, "ws-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row err = %v", err)
	}
	if _, err := s.GetWebSession("hash-a", SessionKindAdmin, "ws-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kind mismatch err = %v", err)
	}

	// A second member of the same browser token.
	v2 := WebSession{TokenHash: "hash-a", Kind: SessionKindFiles, WorkspaceID: "ws-2",
		SubjectID: "key-2", CredentialTag: "tag-2", ExpiresAt: now.Add(time.Hour)}
	if err := s.PutWebSession(v2); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListWebSessions("hash-a", SessionKindFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].WorkspaceID != "ws-1" || rows[1].WorkspaceID != "ws-2" {
		t.Fatalf("list = %+v", rows)
	}

	// Upsert replaces the member in place.
	v.ExpiresAt = now.Add(48 * time.Hour)
	if err := s.PutWebSession(v); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListWebSessions("hash-a", SessionKindFiles)
	if len(rows) != 2 {
		t.Fatalf("upsert duplicated rows: %+v", rows)
	}
	got, _ = s.GetWebSession("hash-a", SessionKindFiles, "ws-1")
	if !got.ExpiresAt.Equal(v.ExpiresAt) {
		t.Fatalf("upsert did not replace: %+v vs %+v", got, v)
	}

	if err := s.DeleteWebSession("hash-a", SessionKindFiles, "ws-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebSession("hash-a", SessionKindFiles, "ws-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted row err = %v", err)
	}
	// Deleting an absent row is not an error (idempotent logout).
	if err := s.DeleteWebSession("hash-a", SessionKindFiles, "ws-1"); err != nil {
		t.Fatalf("idempotent delete = %v", err)
	}
	if rows, _ := s.ListWebSessions("hash-a", SessionKindFiles); len(rows) != 1 {
		t.Fatalf("delete removed the wrong row: %+v", rows)
	}
}

func TestWebSessionKindIsolation(t *testing.T) {
	s, _ := mustOpen(t)
	now := time.Now().UTC()
	admin := WebSession{TokenHash: "hash-k", Kind: SessionKindAdmin, SubjectID: "admin-sub", ExpiresAt: now.Add(time.Hour)}
	files := WebSession{TokenHash: "hash-k", Kind: SessionKindFiles, WorkspaceID: "ws-x", SubjectID: "key-x", ExpiresAt: now.Add(time.Hour)}
	if err := s.PutWebSession(admin); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWebSession(files); err != nil {
		t.Fatal(err)
	}
	// Same token hash, different kinds: independent rows.
	if _, err := s.GetWebSession("hash-k", SessionKindAdmin, ""); err != nil {
		t.Fatalf("admin row: %v", err)
	}
	if _, err := s.GetWebSession("hash-k", SessionKindFiles, "ws-x"); err != nil {
		t.Fatalf("files row: %v", err)
	}
	if err := s.DeleteWebSession("hash-k", SessionKindAdmin, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebSession("hash-k", SessionKindFiles, "ws-x"); err != nil {
		t.Fatalf("files row must survive an admin-kind delete: %v", err)
	}
}

func TestDeleteExpiredWebSessions(t *testing.T) {
	s, _ := mustOpen(t)
	now := time.Now().UTC()
	mk := func(hash, ws string, exp time.Time) {
		if err := s.PutWebSession(WebSession{TokenHash: hash, Kind: SessionKindFiles,
			WorkspaceID: ws, SubjectID: "k", ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	mk("h1", "expired", now.Add(-time.Second))
	mk("h2", "boundary", now) // expires_at == now is expired (<=)
	mk("h3", "live", now.Add(time.Minute))
	if err := s.DeleteExpiredWebSessions(now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebSession("h1", SessionKindFiles, "expired"); !errors.Is(err, ErrNotFound) {
		t.Error("past-expiry row survived cleanup")
	}
	if _, err := s.GetWebSession("h2", SessionKindFiles, "boundary"); !errors.Is(err, ErrNotFound) {
		t.Error("boundary-expiry row survived cleanup")
	}
	if _, err := s.GetWebSession("h3", SessionKindFiles, "live"); err != nil {
		t.Errorf("live row removed: %v", err)
	}
}

func TestFileWorkspaces(t *testing.T) {
	s, path := mustOpen(t)
	id1, err := s.EnsureFileWorkspace("key-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(id1) < 16 {
		t.Fatalf("workspace id too short: %q", id1)
	}
	// Stable across calls and sessions: the same key keeps its entry.
	again, err := s.EnsureFileWorkspace("key-1")
	if err != nil || again != id1 {
		t.Fatalf("entry not stable: %q vs %q (%v)", again, id1, err)
	}
	id2, err := s.EnsureFileWorkspace("key-2")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("two keys share one entry id")
	}
	keyID, err := s.FileWorkspaceKey(id1)
	if err != nil || keyID != "key-1" {
		t.Fatalf("FileWorkspaceKey = %q, %v", keyID, err)
	}
	if _, err := s.FileWorkspaceKey("no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}
	// Reopen: rows persist (non-destructive migration, restart recovery).
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, err := s2.FileWorkspaceKey(id2); err != nil || got != "key-2" {
		t.Fatalf("after reopen: %q %v", got, err)
	}
}
