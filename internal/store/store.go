// Package store implements the cloud2api SQLite persistence layer
// (UPGRADE v0.4 §5.1): drives, API keys with per-drive grants, links, upload
// checksum records, idempotency cache, trash items, audit log and settings.
//
// Netdisk credentials are never stored (§2 principle 1): OpenList remains the
// only place where driver credentials live; this database holds metadata
// only. All timestamps are stored as RFC3339 UTC strings.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db     *sql.DB
	dbPath string
}

// Open opens (creating if needed) the database at path and runs migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// modernc sqlite serializes writes; one connection avoids SQLITE_BUSY
	// between the gateway's own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dbPath: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path returns the database file path (derived artifact roots, e.g. the relay
// chunk staging directory, live next to it).
func (s *Store) Path() string { return s.dbPath }

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	// v1: Phase 1a initial schema.
	`
CREATE TABLE drives (
	id         TEXT PRIMARY KEY,
	provider   TEXT NOT NULL,
	driver     TEXT NOT NULL DEFAULT '',
	storage_id INTEGER NOT NULL DEFAULT 0,
	mount_path TEXT NOT NULL,
	enabled    INTEGER NOT NULL DEFAULT 1,
	static     INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE TABLE api_keys (
	id            TEXT PRIMARY KEY,
	public_id     TEXT NOT NULL DEFAULT '',
	hash          TEXT NOT NULL,
	parent_id     TEXT NOT NULL DEFAULT '',
	static        INTEGER NOT NULL DEFAULT 0,
	revoked       INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL,
	expires_at    TEXT NOT NULL DEFAULT '',
	last_used_at  TEXT NOT NULL DEFAULT '',
	default_drive TEXT NOT NULL DEFAULT '',
	global_perms  TEXT NOT NULL DEFAULT '[]',
	limits        TEXT NOT NULL DEFAULT '{}',
	trash_enabled INTEGER NOT NULL DEFAULT 1,
	trash_days    INTEGER NOT NULL DEFAULT 7
);
CREATE INDEX idx_api_keys_public_id ON api_keys(public_id);
CREATE INDEX idx_api_keys_parent ON api_keys(parent_id);
CREATE TABLE key_drives (
	key_id TEXT NOT NULL,
	drive  TEXT NOT NULL,
	root   TEXT NOT NULL DEFAULT '/',
	perms  TEXT NOT NULL DEFAULT '[]',
	pos    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key_id, drive)
);
CREATE TABLE links (
	token      TEXT PRIMARY KEY,
	key_id     TEXT NOT NULL,
	drive      TEXT NOT NULL,
	path       TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE uploads (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	drive       TEXT NOT NULL,
	path        TEXT NOT NULL,
	size        INTEGER NOT NULL,
	crc64       TEXT NOT NULL,
	sha256      TEXT NOT NULL,
	mtime       TEXT NOT NULL DEFAULT '',
	key_id      TEXT NOT NULL DEFAULT '',
	uploaded_at TEXT NOT NULL
);
CREATE INDEX idx_uploads_drive_path ON uploads(drive, path);
CREATE TABLE idempotency (
	key_id       TEXT NOT NULL,
	idem_key     TEXT NOT NULL,
	method       TEXT NOT NULL,
	route        TEXT NOT NULL,
	status       INTEGER NOT NULL,
	body         BLOB NOT NULL,
	content_type TEXT NOT NULL DEFAULT 'application/json',
	created_at   TEXT NOT NULL,
	PRIMARY KEY (key_id, idem_key)
);
CREATE TABLE trash_items (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	key_id      TEXT NOT NULL,
	drive       TEXT NOT NULL,
	orig_path   TEXT NOT NULL,
	trash_path  TEXT NOT NULL,
	is_dir      INTEGER NOT NULL DEFAULT 0,
	size        INTEGER NOT NULL DEFAULT 0,
	deleted_at  TEXT NOT NULL,
	purge_after TEXT NOT NULL
);
CREATE INDEX idx_trash_purge ON trash_items(purge_after);
CREATE TABLE audit_log (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          TEXT NOT NULL,
	key_id      TEXT NOT NULL DEFAULT '',
	drive       TEXT NOT NULL DEFAULT '',
	op          TEXT NOT NULL DEFAULT '',
	path        TEXT NOT NULL DEFAULT '',
	bytes       INTEGER NOT NULL DEFAULT 0,
	status      INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_ts ON audit_log(ts);
CREATE INDEX idx_audit_key ON audit_log(key_id);
CREATE TABLE settings (
	name  TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`,
	// v2: Phase 1b — agent autonomy (UPGRADE §4.10): jobs and the per-key
	// last selftest timestamp. The agent permission-request approval module
	// (requests table) was removed again: existing databases keep their
	// dormant requests table untouched (no destructive migration), fresh
	// databases simply never create it.
	`
ALTER TABLE api_keys ADD COLUMN last_selftest_at TEXT NOT NULL DEFAULT '';
CREATE TABLE jobs (
	id         TEXT PRIMARY KEY,
	key_id     TEXT NOT NULL,
	kind       TEXT NOT NULL,              -- transfer | fetch | batch
	state      TEXT NOT NULL,              -- queued | running | succeeded | failed | canceled
	progress   REAL NOT NULL DEFAULT 0,
	request    TEXT NOT NULL DEFAULT '{}', -- agent-visible request JSON
	result     TEXT NOT NULL DEFAULT '{}', -- incremental/final result JSON (includes openlist task ids)
	error      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX idx_jobs_key ON jobs(key_id);
CREATE INDEX idx_jobs_state ON jobs(state);
`,
	// v3: Phase 1c — OpenList configuration linkage (UPGRADE §3.7): the
	// linkage event log, the per-drive last-written values of linked fields
	// (three-way merge baseline) and the reconcile-computed drive columns
	// (state, customized_fields, mount_seen).
	`
ALTER TABLE drives ADD COLUMN state TEXT NOT NULL DEFAULT '';
ALTER TABLE drives ADD COLUMN customized_fields TEXT NOT NULL DEFAULT '[]';
ALTER TABLE drives ADD COLUMN mount_seen TEXT NOT NULL DEFAULT '';
CREATE TABLE events (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     TEXT NOT NULL,
	drive  TEXT NOT NULL DEFAULT '',
	kind   TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_events_id ON events(id);
CREATE TABLE drive_field_values (
	drive      TEXT NOT NULL,
	field      TEXT NOT NULL,
	value      TEXT NOT NULL, -- JSON-encoded typed value cloud2api last wrote
	updated_at TEXT NOT NULL,
	PRIMARY KEY (drive, field)
);
`,
	// v4: Phase 1d — Gokapi compatibility layer (UPGRADE §4.7): per-key relay
	// settings and the relay file metadata table. Files live under
	// <key root>/.relay/<Id>/<Name> on the relay drive; this table is the
	// only place Id/HotlinkId/expiry/download counters are tracked.
	`
ALTER TABLE api_keys ADD COLUMN relay TEXT NOT NULL DEFAULT '{}';
CREATE TABLE relay_files (
	id                 TEXT PRIMARY KEY,
	hotlink_id         TEXT NOT NULL,
	key_id             TEXT NOT NULL,
	drive              TEXT NOT NULL,
	name               TEXT NOT NULL,
	size               INTEGER NOT NULL,
	crc64              TEXT NOT NULL,
	sha256             TEXT NOT NULL DEFAULT '',
	content_type       TEXT NOT NULL DEFAULT 'application/octet-stream',
	uploaded_at        TEXT NOT NULL,
	expire_at          TEXT NOT NULL,
	downloads_remaining INTEGER NOT NULL DEFAULT -1, -- -1 = unlimited
	download_count     INTEGER NOT NULL DEFAULT 0,
	pending_deletion   TEXT NOT NULL DEFAULT ''      -- '' = not pending; else purge-at timestamp
);
CREATE UNIQUE INDEX idx_relay_hotlink ON relay_files(hotlink_id);
CREATE INDEX idx_relay_key ON relay_files(key_id);
CREATE INDEX idx_relay_expire ON relay_files(expire_at);
`,
	// v5: durable relay upload receipts. One row per authenticated upload
	// attempt (/api/files/add, /api/chunk/complete) recording the terminal
	// outcome derived from real upstream signals, so clients (and the /relay/
	// page) can query what actually happened — including uploads whose client
	// disconnected mid-body.
	`
CREATE TABLE relay_uploads (
	id          TEXT PRIMARY KEY,          -- client X-Upload-ID or server-minted
	key_id      TEXT NOT NULL,
	uuid        TEXT NOT NULL DEFAULT '',  -- chunk upload uuid when known
	filename    TEXT NOT NULL DEFAULT '',
	filesize    INTEGER NOT NULL DEFAULT 0,
	state       TEXT NOT NULL DEFAULT 'uploading', -- uploading | completed | failed | interrupted
	message     TEXT NOT NULL DEFAULT '',
	file_id     TEXT NOT NULL DEFAULT '',  -- relay_files.id once completed
	size        INTEGER NOT NULL DEFAULT 0,
	crc64       TEXT NOT NULL DEFAULT '',
	sha256      TEXT NOT NULL DEFAULT '',
	claimed     INTEGER NOT NULL DEFAULT 0, -- atomic completion claim: exactly one worker may run the upload
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE INDEX idx_relay_uploads_key ON relay_uploads(key_id);
CREATE INDEX idx_relay_uploads_updated ON relay_uploads(updated_at);
`,
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			i+1, now()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// tsFormat writes timestamps with a fixed-width fractional part so SQL
// string comparisons (ts < ?, purge_after <= ?) are lexicographically
// correct. RFC3339Nano trims trailing zeros and would compare wrongly.
const tsFormat = "2006-01-02T15:04:05.000000000Z"

func now() string { return time.Now().UTC().Format(tsFormat) }
