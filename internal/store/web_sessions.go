package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// Web session kinds (DESIGN-persistent-workspaces §3). The admin kind's rows
// are written by the admin package; the files kind by the workspace API.
const (
	SessionKindAdmin = "admin"
	SessionKindFiles = "files"
)

// WebSession is one persisted browser session row. TokenHash is
// SHA-256 hex of the opaque cookie token — the raw token never touches the
// database. For the files kind SubjectID is the API key id and CredentialTag
// binds the key's current credential generation; admin rows leave
// WorkspaceID empty.
type WebSession struct {
	TokenHash     string
	Kind          string
	WorkspaceID   string
	SubjectID     string
	CredentialTag string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// PutWebSession upserts one session row on its (token_hash, kind,
// workspace_id) primary key.
func (s *Store) PutWebSession(v WebSession) error {
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO web_sessions
		(token_hash, kind, workspace_id, subject_id, credential_tag, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.TokenHash, v.Kind, v.WorkspaceID, v.SubjectID, v.CredentialTag,
		v.CreatedAt.UTC().Format(tsFormat), v.ExpiresAt.UTC().Format(tsFormat))
	return err
}

// GetWebSession loads one session row; ErrNotFound when absent.
func (s *Store) GetWebSession(tokenHash, kind, workspaceID string) (WebSession, error) {
	row := s.db.QueryRow(`SELECT token_hash, kind, workspace_id, subject_id, credential_tag, created_at, expires_at
		FROM web_sessions WHERE token_hash=? AND kind=? AND workspace_id=?`, tokenHash, kind, workspaceID)
	return scanWebSession(row)
}

// ListWebSessions returns every workspace row bound to one browser token
// (one kind), oldest first.
func (s *Store) ListWebSessions(tokenHash, kind string) ([]WebSession, error) {
	rows, err := s.db.Query(`SELECT token_hash, kind, workspace_id, subject_id, credential_tag, created_at, expires_at
		FROM web_sessions WHERE token_hash=? AND kind=? ORDER BY created_at, workspace_id`, tokenHash, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebSession
	for rows.Next() {
		v, err := scanWebSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanWebSession(row interface{ Scan(...any) error }) (WebSession, error) {
	var v WebSession
	var created, expires string
	if err := row.Scan(&v.TokenHash, &v.Kind, &v.WorkspaceID, &v.SubjectID, &v.CredentialTag, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebSession{}, ErrNotFound
		}
		return WebSession{}, err
	}
	v.CreatedAt = parseTime(created)
	v.ExpiresAt = parseTime(expires)
	return v, nil
}

// DeleteWebSession removes one row; deleting an absent row is not an error
// (logout stays idempotent).
func (s *Store) DeleteWebSession(tokenHash, kind, workspaceID string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE token_hash=? AND kind=? AND workspace_id=?`,
		tokenHash, kind, workspaceID)
	return err
}

// DeleteExpiredWebSessions drops every row whose fixed expiry has passed
// (expiry itself, boundary included).
func (s *Store) DeleteExpiredWebSessions(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE expires_at <= ?`, now.UTC().Format(tsFormat))
	return err
}

// EnsureFileWorkspace returns the stable entry id of keyID's file workspace,
// creating the row (with a fresh random 128-bit non-secret id) when the key
// logs in for the first time. The entry never carries authorization; every
// use revalidates the live key.
func (s *Store) EnsureFileWorkspace(keyID string) (string, error) {
	row := s.db.QueryRow(`SELECT id FROM file_workspaces WHERE key_id=?`, keyID)
	var id string
	if err := row.Scan(&id); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	buf := make([]byte, 16) // 128 bit
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id = base64.RawURLEncoding.EncodeToString(buf)
	if _, err := s.db.Exec(`INSERT INTO file_workspaces (id, key_id, created_at) VALUES (?, ?, ?)`,
		id, keyID, now()); err != nil {
		// A concurrent creator lost nothing: the unique key_id admits exactly
		// one row, so re-read whichever id won.
		row := s.db.QueryRow(`SELECT id FROM file_workspaces WHERE key_id=?`, keyID)
		var existing string
		if err := row.Scan(&existing); err == nil {
			return existing, nil
		}
		return "", err
	}
	return id, nil
}

// FileWorkspaceKey returns the key id an entry was created for; ErrNotFound
// for an unknown id.
func (s *Store) FileWorkspaceKey(id string) (string, error) {
	var keyID string
	err := s.db.QueryRow(`SELECT key_id FROM file_workspaces WHERE id=?`, id).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return keyID, nil
}
