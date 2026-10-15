package store

import (
	"time"
)

// Event is one OpenList linkage event (UPGRADE §3.6.3 / §8.2 GET events).
// Kinds: mount_renamed, disabled_in_openlist, enabled_in_openlist,
// deleted_in_openlist, storage_rebound, customized, adopted, released,
// settings_changed.
type Event struct {
	ID     int64
	TS     time.Time
	Drive  string
	Kind   string
	Detail string
}

// InsertEvent appends one linkage event.
func (s *Store) InsertEvent(drive, kind, detail string) error {
	_, err := s.db.Exec(`INSERT INTO events (ts, drive, kind, detail) VALUES (?, ?, ?, ?)`,
		now(), drive, kind, detail)
	return err
}

// ListEvents returns the most recent events (newest first).
func (s *Store) ListEvents(limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, ts, drive, kind, detail FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Drive, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents drops events older than the retention window.
func (s *Store) PruneEvents(olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan).Format(tsFormat)
	_, err := s.db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	return err
}
