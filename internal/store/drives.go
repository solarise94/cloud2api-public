package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Drive is a cloud2api drive: the mapping between a logical drive id and an
// OpenList storage (by storage id, not mount path — the mount path is only an
// attribute read back from OpenList, UPGRADE §3.7.1).
type Drive struct {
	ID        string
	Provider  string
	Driver    string
	StorageID int64
	MountPath string
	Enabled   bool
	Static    bool
	CreatedAt time.Time
	// Phase 1c reconcile columns: the last computed state, the linked fields
	// whose OpenList value drifted from cloud2api's last-written value, and
	// the OpenList mount path last observed (suppresses repeat mount_renamed
	// events for static drives after a restart re-applies config.yaml).
	State            string
	CustomizedFields []string
	MountSeen        string
}

var ErrNotFound = errors.New("not found")

func (s *Store) InsertDrive(d Drive) error {
	_, err := s.db.Exec(`INSERT INTO drives (id, provider, driver, storage_id, mount_path, enabled, static, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Provider, d.Driver, d.StorageID, d.MountPath, boolInt(d.Enabled), boolInt(d.Static), now())
	return err
}

func (s *Store) UpdateDrive(d Drive) error {
	_, err := s.db.Exec(`UPDATE drives SET provider=?, driver=?, storage_id=?, mount_path=?, enabled=?, static=? WHERE id=?`,
		d.Provider, d.Driver, d.StorageID, d.MountPath, boolInt(d.Enabled), boolInt(d.Static), d.ID)
	return err
}

func (s *Store) GetDrive(id string) (Drive, error) {
	row := s.db.QueryRow(`SELECT id, provider, driver, storage_id, mount_path, enabled, static, created_at, state, customized_fields, mount_seen FROM drives WHERE id=?`, id)
	return scanDrive(row)
}

func (s *Store) DeleteDrive(id string) error {
	if _, err := s.db.Exec(`DELETE FROM drive_field_values WHERE drive=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM drives WHERE id=?`, id)
	return err
}

func (s *Store) ListDrives() ([]Drive, error) {
	rows, err := s.db.Query(`SELECT id, provider, driver, storage_id, mount_path, enabled, static, created_at, state, customized_fields, mount_seen FROM drives ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Drive
	for rows.Next() {
		d, err := scanDrive(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ApplyReconcile persists one reconcile step for a drive (Phase 1c): the
// computed state and the OpenList mount last observed; mount/storageID, when
// non-nil, follow the OpenList storage (mount renamed in OpenList →
// cloud2api follows, §3.7.4). The legacy customized_fields column is no
// longer written (IMPLEMENTATION §删除的部分) — the schema keeps it, this
// code ignores it.
func (s *Store) ApplyReconcile(id, state string, mountSeen string, mount *string, storageID *int64) error {
	var err error
	switch {
	case mount != nil && storageID != nil:
		_, err = s.db.Exec(`UPDATE drives SET state=?, mount_seen=?, mount_path=?, storage_id=? WHERE id=?`,
			state, mountSeen, *mount, *storageID, id)
	case mount != nil:
		_, err = s.db.Exec(`UPDATE drives SET state=?, mount_seen=?, mount_path=? WHERE id=?`,
			state, mountSeen, *mount, id)
	case storageID != nil:
		_, err = s.db.Exec(`UPDATE drives SET state=?, mount_seen=?, storage_id=? WHERE id=?`,
			state, mountSeen, *storageID, id)
	default:
		_, err = s.db.Exec(`UPDATE drives SET state=?, mount_seen=? WHERE id=?`,
			state, mountSeen, id)
	}
	return err
}

// SetDriveDriver updates the drive's OpenList driver (reconcile follows the
// storage's real driver, Phase 2 fix).
func (s *Store) SetDriveDriver(id, driver string) error {
	res, err := s.db.Exec(`UPDATE drives SET driver=? WHERE id=?`, driver, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDriveEnabled flips cloud2api's own enabled switch (whether the drive is
// open to /v1 and the Gokapi layer; OpenList is never touched).
func (s *Store) SetDriveEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE drives SET enabled=? WHERE id=?`, boolInt(enabled), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDriveStorageID records the OpenList storage id once it is known.
func (s *Store) SetDriveStorageID(id string, storageID int64) error {
	_, err := s.db.Exec(`UPDATE drives SET storage_id=? WHERE id=?`, storageID, id)
	return err
}

// DriveKeyCount returns how many non-revoked keys grant access to drive.
func (s *Store) DriveKeyCount(drive string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM key_drives kd
		JOIN api_keys k ON k.id = kd.key_id
		WHERE kd.drive=? AND k.revoked=0`, drive).Scan(&n)
	return n, err
}

func scanDrive(row interface{ Scan(...any) error }) (Drive, error) {
	var d Drive
	var enabled, static int
	var created, state, customized, mountSeen string
	if err := row.Scan(&d.ID, &d.Provider, &d.Driver, &d.StorageID, &d.MountPath, &enabled, &static, &created, &state, &customized, &mountSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Drive{}, ErrNotFound
		}
		return Drive{}, err
	}
	d.Enabled = enabled != 0
	d.Static = static != 0
	d.CreatedAt = parseTime(created)
	d.State = state
	d.MountSeen = mountSeen
	d.CustomizedFields = []string{}
	if customized != "" {
		_ = json.Unmarshal([]byte(customized), &d.CustomizedFields)
	}
	return d, nil
}
