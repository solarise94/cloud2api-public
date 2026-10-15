package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// KeyDrive is one per-drive grant of a key: the chroot root (logical "/" of
// the agent maps to this directory inside the drive) and the permission set.
type KeyDrive struct {
	Drive string   `json:"drive"`
	Root  string   `json:"root"`
	Perms []string `json:"perms"`
}

// Limits mirrors the admin API limits object. nil pointer = unlimited /
// unset (JSON null).
type Limits struct {
	MaxUploadBytes    *int64 `json:"max_upload_bytes"`
	RequestsPerMinute *int   `json:"requests_per_minute"`
	ConcurrentJobs    *int   `json:"concurrent_jobs"`
}

// TrashSettings is the per-key recycle-bin configuration.
type TrashSettings struct {
	Enabled bool `json:"enabled"`
	Days    int  `json:"days"`
}

// RelaySettings is the per-key Gokapi compatibility layer configuration
// (UPGRADE §4.7). Zero values mean "default": drive falls back to the key's
// default drive, quota to 200 GiB and max age to 7 days (the hard cap).
type RelaySettings struct {
	Drive      string `json:"drive"`
	QuotaBytes int64  `json:"quota_bytes"`
	MaxAgeDays int    `json:"max_age_days"`
}

// DefaultRelayQuota is the per-key relay capacity default (200 GiB, Gokapi
// relay semantics).
const DefaultRelayQuota = 200 << 30

// RelayMaxAgeDays is the hard retention cap of the relay layer (Gokapi
// semantics: files never outlive seven days).
const RelayMaxAgeDays = 7

// APIKey is the persisted key record. The plaintext secret is never stored,
// only its SHA-256 ("sha256:<hex>").
type APIKey struct {
	ID           string
	PublicID     string // 8 chars for sk-c2a-<public>-<secret> keys; "" for legacy keys
	Hash         string
	ParentID     string
	Static       bool
	Revoked      bool
	CreatedAt    time.Time
	ExpiresAt    *time.Time // nil = never expires
	LastUsedAt   *time.Time
	LastSelftest *time.Time
	DefaultDrive string
	Drives       []KeyDrive // ordered; the first entry is the implicit default
	GlobalPerms  []string
	Limits       Limits
	Trash        TrashSettings
	Relay        RelaySettings
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(tsFormat)
}

func marshalStrings(v []string) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func unmarshalStrings(s string) []string {
	var out []string
	if strings.TrimSpace(s) != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// InsertKey stores a new key together with its per-drive grants.
func (s *Store) InsertKey(k APIKey) error {
	if strings.TrimSpace(k.ID) == "" {
		// An empty id would poison the revocation cascade: parent_id uses ''
		// for "no parent", so a key with id '' would make every top-level
		// key look like its child (2026-10-12 incident).
		return errors.New("key id must not be empty")
	}
	limits := "{}"
	if b, err := json.Marshal(k.Limits); err == nil {
		limits = string(b)
	}
	relay := "{}"
	if b, err := json.Marshal(k.Relay); err == nil {
		relay = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO api_keys (id, public_id, hash, parent_id, static, revoked, created_at,
		expires_at, last_used_at, default_drive, global_perms, limits, trash_enabled, trash_days, relay)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.PublicID, k.Hash, k.ParentID, boolInt(k.Static), boolInt(k.Revoked), now(),
		formatTimePtr(k.ExpiresAt), formatTimePtr(k.LastUsedAt), k.DefaultDrive,
		marshalStrings(k.GlobalPerms), limits, boolInt(k.Trash.Enabled), k.Trash.Days, relay); err != nil {
		return err
	}
	if err := replaceKeyDrivesTx(tx, k.ID, k.Drives); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateKey replaces the mutable fields of a key. The row identity (id,
// created_at, static flag) stays; hash/public_id/parent_id move with the
// object (the static config sync replaces them wholesale; portal patches
// carry the loaded values through unchanged).
func (s *Store) UpdateKey(k APIKey) error {
	limits := "{}"
	if b, err := json.Marshal(k.Limits); err == nil {
		limits = string(b)
	}
	relay := "{}"
	if b, err := json.Marshal(k.Relay); err == nil {
		relay = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE api_keys SET expires_at=?, last_used_at=?, default_drive=?,
		global_perms=?, limits=?, trash_enabled=?, trash_days=?, revoked=?, public_id=?, hash=?, parent_id=?, relay=? WHERE id=?`,
		formatTimePtr(k.ExpiresAt), formatTimePtr(k.LastUsedAt), k.DefaultDrive,
		marshalStrings(k.GlobalPerms), limits, boolInt(k.Trash.Enabled), k.Trash.Days, boolInt(k.Revoked),
		k.PublicID, k.Hash, k.ParentID, relay, k.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := replaceKeyDrivesTx(tx, k.ID, k.Drives); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceKeyDrivesTx(tx *sql.Tx, keyID string, drives []KeyDrive) error {
	if _, err := tx.Exec(`DELETE FROM key_drives WHERE key_id=?`, keyID); err != nil {
		return err
	}
	for i, d := range drives {
		if _, err := tx.Exec(`INSERT INTO key_drives (key_id, drive, root, perms, pos) VALUES (?, ?, ?, ?, ?)`,
			keyID, d.Drive, d.Root, marshalStrings(d.Perms), i); err != nil {
			return err
		}
	}
	return nil
}

// DeleteKey removes a key row and its grants entirely (recycling a revoked
// id); audit rows keep their plain key_id strings.
func (s *Store) DeleteKey(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM key_drives WHERE key_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// RotateKeyHash replaces the secret hash (and public id) of a key.
func (s *Store) RotateKeyHash(id, publicID, hash string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET public_id=?, hash=?, revoked=0 WHERE id=?`, publicID, hash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeKey marks a key revoked and cascades to every descendant child key.
func (s *Store) RevokeKey(id string) error {
	// Collect the subtree iteratively ( SQLite without recursive CTE would be
	// simpler, but a CTE works fine too; keep it explicit and testable).
	if strings.TrimSpace(id) == "" {
		return errors.New("cannot revoke a key with an empty id")
	}
	toRevoke := []string{id}
	seen := map[string]bool{id: true}
	for i := 0; i < len(toRevoke); i++ {
		if toRevoke[i] == "" {
			// parent_id='' means "no parent"; never traverse it.
			continue
		}
		rows, err := s.db.Query(`SELECT id FROM api_keys WHERE parent_id=? AND parent_id!=''`, toRevoke[i])
		if err != nil {
			return err
		}
		var children []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			children = append(children, c)
		}
		rows.Close()
		for _, c := range children {
			if c != "" && !seen[c] {
				seen[c] = true
				toRevoke = append(toRevoke, c)
			}
		}
	}
	// Static keys must never be revoked by any path, not even a cascade.
	keep := toRevoke[:0]
	for _, k := range toRevoke {
		row, err := s.GetKey(k)
		if err == nil && row.Static {
			continue
		}
		keep = append(keep, k)
	}
	toRevoke = keep
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range toRevoke {
		if _, err := tx.Exec(`UPDATE api_keys SET revoked=1 WHERE id=?`, k); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM idempotency WHERE key_id=?`, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TouchKeyUsed records the last authentication time (best effort).
func (s *Store) TouchKeyUsed(id string) {
	_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, now(), id)
}

// TouchKeySelftest records the last selftest time of a key.
func (s *Store) TouchKeySelftest(id string) error {
	_, err := s.db.Exec(`UPDATE api_keys SET last_selftest_at=? WHERE id=?`, now(), id)
	return err
}

// GetKey loads one key with its grants.
func (s *Store) GetKey(id string) (APIKey, error) {
	row := s.db.QueryRow(keyColumns+` WHERE id=?`, id)
	return scanKey(row)
}

// ListKeys loads every key with its grants, sorted by id.
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query(keyColumns + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

const keyColumns = `SELECT id, public_id, hash, parent_id, static, revoked, created_at,
	COALESCE(expires_at,''), COALESCE(last_used_at,''), COALESCE(last_selftest_at,''), default_drive, global_perms, limits, trash_enabled, trash_days, relay
	FROM api_keys`

func scanKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var static, revoked, trashEnabled, trashDays int
	var created, expires, lastUsed, lastSelftest, globalPerms, limits, relay string
	if err := row.Scan(&k.ID, &k.PublicID, &k.Hash, &k.ParentID, &static, &revoked, &created,
		&expires, &lastUsed, &lastSelftest, &k.DefaultDrive, &globalPerms, &limits, &trashEnabled, &trashDays, &relay); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	k.Static = static != 0
	k.Revoked = revoked != 0
	k.CreatedAt = parseTime(created)
	if expires != "" {
		t := parseTime(expires)
		k.ExpiresAt = &t
	}
	if lastUsed != "" {
		t := parseTime(lastUsed)
		k.LastUsedAt = &t
	}
	if lastSelftest != "" {
		t := parseTime(lastSelftest)
		k.LastSelftest = &t
	}
	k.GlobalPerms = unmarshalStrings(globalPerms)
	if err := json.Unmarshal([]byte(limits), &k.Limits); err != nil {
		k.Limits = Limits{}
	}
	if err := json.Unmarshal([]byte(relay), &k.Relay); err != nil {
		k.Relay = RelaySettings{}
	}
	k.Trash = TrashSettings{Enabled: trashEnabled != 0, Days: trashDays}
	return k, nil
}

// KeyGrants loads the ordered per-drive grants of a key.
func (s *Store) KeyGrants(keyID string) ([]KeyDrive, error) {
	rows, err := s.db.Query(`SELECT drive, root, perms FROM key_drives WHERE key_id=? ORDER BY pos`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyDrive
	for rows.Next() {
		var d KeyDrive
		var perms string
		if err := rows.Scan(&d.Drive, &d.Root, &perms); err != nil {
			return nil, err
		}
		d.Perms = unmarshalStrings(perms)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) scanKeysWithGrants(keys []APIKey) ([]APIKey, error) {
	for i := range keys {
		grants, err := s.KeyGrants(keys[i].ID)
		if err != nil {
			return nil, err
		}
		keys[i].Drives = grants
	}
	return keys, nil
}

// ListKeysWithGrants is ListKeys plus per-drive grants.
func (s *Store) ListKeysWithGrants() ([]APIKey, error) {
	keys, err := s.ListKeys()
	if err != nil {
		return nil, err
	}
	return s.scanKeysWithGrants(keys)
}

// GetKeyWithGrants is GetKey plus per-drive grants.
func (s *Store) GetKeyWithGrants(id string) (APIKey, error) {
	k, err := s.GetKey(id)
	if err != nil {
		return APIKey{}, err
	}
	grants, err := s.KeyGrants(id)
	if err != nil {
		return APIKey{}, err
	}
	k.Drives = grants
	return k, nil
}

// SyncStaticKey upserts a static (config.yaml) key row: config is the source
// of truth for static keys, so hash and grants are replaced wholesale.
func (s *Store) SyncStaticKey(k APIKey) error {
	old, err := s.GetKey(k.ID)
	if errors.Is(err, ErrNotFound) {
		k.Static = true
		return s.InsertKey(k)
	}
	if err != nil {
		return err
	}
	old.Static = true
	// config.yaml is the source of truth for static keys: a revoked flag
	// that somehow landed on the row (bad cascade, manual edit) is healed
	// on every boot.
	old.Revoked = false
	old.Hash = k.Hash
	old.PublicID = k.PublicID
	old.ParentID = k.ParentID
	old.DefaultDrive = k.DefaultDrive
	old.GlobalPerms = k.GlobalPerms
	old.Limits = k.Limits
	old.Trash = k.Trash
	old.Drives = k.Drives
	return s.UpdateKey(old)
}

// DeleteStaticKeysNotIn removes static key rows whose ids are not in keep.
func (s *Store) DeleteStaticKeysNotIn(keep []string) error {
	rows, err := s.db.Query(`SELECT id FROM api_keys WHERE static=1`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	for _, id := range ids {
		if !keepSet[id] {
			if _, err := s.db.Exec(`DELETE FROM key_drives WHERE key_id=?`, id); err != nil {
				return err
			}
			if _, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Exec runs one raw statement (test/maintenance hooks only).
func (s *Store) Exec(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)
	return err
}
