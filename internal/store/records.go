package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ---- links ----

// Link records a limited-time direct download link (validation itself is
// HMAC-based; the row exists for audit and future listing).
type Link struct {
	Token     string
	KeyID     string
	Drive     string
	Path      string // full logical path inside the drive (post-chroot)
	ExpiresAt time.Time
	CreatedAt time.Time
}

func (s *Store) InsertLink(l Link) error {
	_, err := s.db.Exec(`INSERT INTO links (token, key_id, drive, path, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.Token, l.KeyID, l.Drive, l.Path, l.ExpiresAt.UTC().Format(tsFormat), now())
	return err
}

func (s *Store) GetLink(token string) (Link, error) {
	var l Link
	var exp, created string
	err := s.db.QueryRow(`SELECT token, key_id, drive, path, expires_at, created_at FROM links WHERE token=?`, token).
		Scan(&l.Token, &l.KeyID, &l.Drive, &l.Path, &exp, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, err
	}
	l.ExpiresAt = parseTime(exp)
	l.CreatedAt = parseTime(created)
	return l, nil
}

// ---- uploads (integrity records) ----

// Upload is the checksum record of one gateway upload (UPGRADE §4.4).
type Upload struct {
	ID         int64
	Drive      string
	Path       string
	Size       int64
	CRC64      string // decimal string, CRC-64/XZ
	SHA256     string
	Mtime      string // upstream modified time when recorded
	KeyID      string
	UploadedAt time.Time
}

func (s *Store) RecordUpload(u Upload) error {
	_, err := s.db.Exec(`INSERT INTO uploads (drive, path, size, crc64, sha256, mtime, key_id, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Drive, u.Path, u.Size, u.CRC64, u.SHA256, u.Mtime, u.KeyID, now())
	return err
}

// LatestUploadFor returns the newest checksum record for a drive path.
func (s *Store) LatestUploadFor(drive, path string) (Upload, error) {
	var u Upload
	var uploaded string
	err := s.db.QueryRow(`SELECT id, drive, path, size, crc64, sha256, COALESCE(mtime,''), key_id, uploaded_at
		FROM uploads WHERE drive=? AND path=? ORDER BY id DESC LIMIT 1`, drive, path).
		Scan(&u.ID, &u.Drive, &u.Path, &u.Size, &u.CRC64, &u.SHA256, &u.Mtime, &u.KeyID, &uploaded)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if err != nil {
		return Upload{}, err
	}
	u.UploadedAt = parseTime(uploaded)
	return u, nil
}

// ---- idempotency ----

// IdempotencyRecord caches one mutating request's response for 24h replay.
type IdempotencyRecord struct {
	KeyID       string
	IdemKey     string
	Method      string
	Route       string
	Status      int
	Body        []byte
	ContentType string
	CreatedAt   time.Time
}

// GetIdempotency returns the stored response when it is still within the TTL.
func (s *Store) GetIdempotency(keyID, idemKey string, ttl time.Duration) (IdempotencyRecord, error) {
	var r IdempotencyRecord
	var created string
	err := s.db.QueryRow(`SELECT key_id, idem_key, method, route, status, body, content_type, created_at
		FROM idempotency WHERE key_id=? AND idem_key=?`, keyID, idemKey).
		Scan(&r.KeyID, &r.IdemKey, &r.Method, &r.Route, &r.Status, &r.Body, &r.ContentType, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyRecord{}, ErrNotFound
	}
	if err != nil {
		return IdempotencyRecord{}, err
	}
	r.CreatedAt = parseTime(created)
	if time.Since(r.CreatedAt) > ttl {
		return IdempotencyRecord{}, ErrNotFound
	}
	return r, nil
}

func (s *Store) PutIdempotency(r IdempotencyRecord) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO idempotency (key_id, idem_key, method, route, status, body, content_type, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.KeyID, r.IdemKey, r.Method, r.Route, r.Status, r.Body, r.ContentType, idemCreatedAt(r))
	return err
}

func idemCreatedAt(r IdempotencyRecord) string {
	if !r.CreatedAt.IsZero() {
		return r.CreatedAt.UTC().Format(tsFormat)
	}
	return now()
}

// ---- trash ----

// TrashItem is one entry of a key's recycle bin.
type TrashItem struct {
	ID         int64
	KeyID      string
	Drive      string
	OrigPath   string // logical path inside the key chroot where the item lived
	TrashPath  string // full logical path inside the drive where it lives now
	IsDir      bool
	Size       int64
	DeletedAt  time.Time
	PurgeAfter time.Time
}

func (s *Store) InsertTrashItem(t TrashItem) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO trash_items (key_id, drive, orig_path, trash_path, is_dir, size, deleted_at, purge_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.KeyID, t.Drive, t.OrigPath, t.TrashPath, boolInt(t.IsDir), t.Size, now(),
		t.PurgeAfter.UTC().Format(tsFormat))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanTrash(row interface{ Scan(...any) error }) (TrashItem, error) {
	var t TrashItem
	var isDir int
	var deleted, purgeAfter string
	if err := row.Scan(&t.ID, &t.KeyID, &t.Drive, &t.OrigPath, &t.TrashPath, &isDir, &t.Size, &deleted, &purgeAfter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrashItem{}, ErrNotFound
		}
		return TrashItem{}, err
	}
	t.IsDir = isDir != 0
	t.DeletedAt = parseTime(deleted)
	t.PurgeAfter = parseTime(purgeAfter)
	return t, nil
}

func (s *Store) ListTrashForKey(keyID string) ([]TrashItem, error) {
	rows, err := s.db.Query(`SELECT id, key_id, drive, orig_path, trash_path, is_dir, size, deleted_at, purge_after
		FROM trash_items WHERE key_id=? ORDER BY id DESC`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTrashItem(id int64, keyID string) (TrashItem, error) {
	row := s.db.QueryRow(`SELECT id, key_id, drive, orig_path, trash_path, is_dir, size, deleted_at, purge_after
		FROM trash_items WHERE id=? AND key_id=?`, id, keyID)
	return scanTrash(row)
}

func (s *Store) DeleteTrashItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM trash_items WHERE id=?`, id)
	return err
}

// ExpiredTrash lists items whose retention window has elapsed.
func (s *Store) ExpiredTrash(now time.Time) ([]TrashItem, error) {
	rows, err := s.db.Query(`SELECT id, key_id, drive, orig_path, trash_path, is_dir, size, deleted_at, purge_after
		FROM trash_items WHERE purge_after <= ? ORDER BY id`, now.UTC().Format(tsFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- audit log ----

// AuditEntry is one data-plane request record (never keys or credentials).
type AuditEntry struct {
	ID         int64
	TS         time.Time
	KeyID      string
	Drive      string
	Op         string
	Path       string
	Bytes      int64
	Status     int
	DurationMS int64
	Error      string
}

// InsertAudit records one request (best effort, errors are the caller's to log).
func (s *Store) InsertAudit(e AuditEntry) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO audit_log (ts, key_id, drive, op, path, bytes, status, duration_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS.UTC().Format(tsFormat), e.KeyID, e.Drive, e.Op, e.Path, e.Bytes, e.Status, e.DurationMS, e.Error)
	return err
}

// AuditFilter narrows a QueryAudit call. Empty string fields match anything;
// Before pages by exclusive timestamp. KeyExact overrides Key.
type AuditFilter struct {
	Key      string
	KeyExact string
	Drive    string
	Op       string
	Status   string
	Before   string // RFC3339 timestamp
	Limit    int
}

// QueryAudit returns entries newest-first plus the next_before cursor.
func (s *Store) QueryAudit(f AuditFilter) ([]AuditEntry, string, error) {
	where := " WHERE 1=1"
	args := []any{}
	if f.KeyExact != "" {
		where += " AND key_id = ?"
		args = append(args, f.KeyExact)
	} else if f.Key != "" {
		where += " AND key_id LIKE ?"
		args = append(args, "%"+f.Key+"%")
	}
	if f.Drive != "" {
		where += " AND drive LIKE ?"
		args = append(args, "%"+f.Drive+"%")
	}
	if f.Op != "" {
		where += " AND op = ?"
		args = append(args, f.Op)
	}
	if f.Status != "" {
		where += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Before != "" {
		where += " AND ts < ?"
		args = append(args, f.Before)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT id, ts, key_id, drive, op, path, bytes, status, duration_ms, error FROM audit_log` + where +
		` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.KeyID, &e.Drive, &e.Op, &e.Path, &e.Bytes, &e.Status, &e.DurationMS, &e.Error); err != nil {
			return nil, "", err
		}
		e.TS = parseTime(ts)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	nextBefore := ""
	if len(out) > limit {
		out = out[:limit]
		nextBefore = out[len(out)-1].TS.UTC().Format(tsFormat)
	}
	return out, nextBefore, nil
}

// PruneAudit deletes entries older than the retention window.
func (s *Store) PruneAudit(olderThan time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM audit_log WHERE ts < ?`, time.Now().UTC().Add(-olderThan).Format(tsFormat))
	return err
}

// PruneIdempotency drops cached responses past the TTL.
func (s *Store) PruneIdempotency(ttl time.Duration) error {
	_, err := s.db.Exec(`DELETE FROM idempotency WHERE created_at < ?`, time.Now().UTC().Add(-ttl).Format(tsFormat))
	return err
}

// ---- settings ----

func (s *Store) GetSetting(name string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE name=?`, name).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) SetSetting(name, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value=excluded.value`, name, value)
	return err
}

// AllSettings returns every stored setting (values are opaque strings).
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}

// MarshalJSON helper for Limits is unnecessary: the struct tags already
// produce {null, ...} for nil pointers.
var _ = json.Marshal
