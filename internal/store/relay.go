package store

import (
	"database/sql"
	"errors"
	"time"
)

// RelayFile is one Gokapi-compat relay file (UPGRADE §4.7). The bytes live on
// the relay drive at <key root>/.relay/<Id>/<Name>; this row carries the
// identity, checksums and Gokapi semantics (expiry, download counters,
// delayed deletion).
type RelayFile struct {
	ID                 string
	HotlinkID          string
	KeyID              string
	Drive              string
	Name               string
	Size               int64
	CRC64              string // decimal string, CRC-64/XZ
	SHA256             string
	ContentType        string
	UploadedAt         time.Time
	ExpireAt           time.Time
	DownloadsRemaining int64 // -1 = unlimited
	DownloadCount      int64
	PendingDeletion    time.Time // zero = not pending
}

const relayFileColumns = `id, hotlink_id, key_id, drive, name, size, crc64, sha256, content_type, uploaded_at, expire_at, downloads_remaining, download_count, pending_deletion`

func scanRelayFile(row interface{ Scan(...any) error }) (RelayFile, error) {
	var f RelayFile
	var uploaded, expire, pending string
	if err := row.Scan(&f.ID, &f.HotlinkID, &f.KeyID, &f.Drive, &f.Name, &f.Size, &f.CRC64, &f.SHA256,
		&f.ContentType, &uploaded, &expire, &f.DownloadsRemaining, &f.DownloadCount, &pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RelayFile{}, ErrNotFound
		}
		return RelayFile{}, err
	}
	f.UploadedAt = parseTime(uploaded)
	f.ExpireAt = parseTime(expire)
	if pending != "" {
		f.PendingDeletion = parseTime(pending)
	}
	return f, nil
}

// InsertRelayFile records a completed relay upload.
func (s *Store) InsertRelayFile(f RelayFile) error {
	pending := ""
	if !f.PendingDeletion.IsZero() {
		pending = f.PendingDeletion.UTC().Format(tsFormat)
	}
	_, err := s.db.Exec(`INSERT INTO relay_files (`+relayFileColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.HotlinkID, f.KeyID, f.Drive, f.Name, f.Size, f.CRC64, f.SHA256, f.ContentType,
		f.UploadedAt.UTC().Format(tsFormat), f.ExpireAt.UTC().Format(tsFormat),
		f.DownloadsRemaining, f.DownloadCount, pending)
	return err
}

// GetRelayFile loads one relay file by its Gokapi id.
func (s *Store) GetRelayFile(id string) (RelayFile, error) {
	return scanRelayFile(s.db.QueryRow(`SELECT `+relayFileColumns+` FROM relay_files WHERE id=?`, id))
}

// GetRelayFileByHotlink loads one relay file by its hotlink id.
func (s *Store) GetRelayFileByHotlink(hotlinkID string) (RelayFile, error) {
	return scanRelayFile(s.db.QueryRow(`SELECT `+relayFileColumns+` FROM relay_files WHERE hotlink_id=?`, hotlinkID))
}

// ListRelayFiles returns every relay file of one key, oldest upload first
// (Gokapi eviction order).
func (s *Store) ListRelayFiles(keyID string) ([]RelayFile, error) {
	rows, err := s.db.Query(`SELECT `+relayFileColumns+` FROM relay_files WHERE key_id=? ORDER BY uploaded_at, id`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelayFile
	for rows.Next() {
		f, err := scanRelayFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RelayUsage sums the stored sizes of one key's relay files.
func (s *Store) RelayUsage(keyID string) (int64, error) {
	var used int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM relay_files WHERE key_id=?`, keyID).Scan(&used)
	return used, err
}

// DeleteRelayFile drops the metadata row (the caller removes the bytes).
func (s *Store) DeleteRelayFile(id string) error {
	_, err := s.db.Exec(`DELETE FROM relay_files WHERE id=?`, id)
	return err
}

// ConsumeRelayDownload decrements downloads_remaining (unlimited files only
// bump the counter). Returns false when the file has no downloads left.
func (s *Store) ConsumeRelayDownload(id string) bool {
	res, err := s.db.Exec(`UPDATE relay_files
		SET download_count = download_count + 1,
		    downloads_remaining = CASE WHEN downloads_remaining > 0 THEN downloads_remaining - 1 ELSE downloads_remaining END
		WHERE id=? AND (downloads_remaining > 0 OR downloads_remaining < 0)`, id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// SetRelayPendingDeletion arms (or cancels, when zero) the delayed deletion.
func (s *Store) SetRelayPendingDeletion(id string, at time.Time) error {
	v := ""
	if !at.IsZero() {
		v = at.UTC().Format(tsFormat)
	}
	_, err := s.db.Exec(`UPDATE relay_files SET pending_deletion=? WHERE id=?`, v, id)
	return err
}

// DueRelayFiles lists files whose expiry or pending deletion has elapsed and
// that must be purged permanently (never into the trash; Gokapi semantics).
func (s *Store) DueRelayFiles(now time.Time) ([]RelayFile, error) {
	ts := now.UTC().Format(tsFormat)
	rows, err := s.db.Query(`SELECT `+relayFileColumns+` FROM relay_files
		WHERE expire_at <= ? OR (pending_deletion != '' AND pending_deletion <= ?)`, ts, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelayFile
	for rows.Next() {
		f, err := scanRelayFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RelayFileCounts summarizes the relay table for /v1/me and the maintenance
// report.
func (s *Store) RelayFileCounts(keyID string) (files int64, bytes int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM relay_files WHERE key_id=?`, keyID).Scan(&files, &bytes)
	return
}
