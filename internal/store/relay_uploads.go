package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Relay upload receipt states. A receipt exists for every authenticated
// upload attempt of the Gokapi compatibility layer; its state is only ever
// advanced from real upstream signals (fs/put HTTP+envelope success and the
// persisted relay_files row), never from byte counters.
const (
	RelayUploadUploading   = "uploading"
	RelayUploadCompleted   = "completed"
	RelayUploadFailed      = "failed"
	RelayUploadInterrupted = "interrupted"
)

// ErrUploadIDTaken is returned when a client-chosen upload id already belongs
// to a different key; ids are never reused or overwritten across keys.
var ErrUploadIDTaken = errors.New("upload id already used by another key")

// ErrUploadIDExists is returned when a client-chosen upload id was already
// used by the SAME key: an upload attempt's identity is immutable, so a retry
// must carry a fresh id (the old receipt stays queryable and unchanged).
var ErrUploadIDExists = errors.New("upload id already used; start a new attempt with a fresh id")

// ErrReceiptTerminal is returned when a receipt already reached a terminal
// state: terminal outcomes are monotone and are never overwritten, not by a
// late callback and not by a duplicate worker.
var ErrReceiptTerminal = errors.New("upload receipt already reached a terminal state")

// ErrReceiptClaimed is returned when the receipt's completion is already
// claimed by another worker: the atomic claim guarantees exactly one upstream
// upload per attempt.
var ErrReceiptClaimed = errors.New("upload receipt is already being completed by another request")

// ErrUploadAttemptMismatch is returned when a receipt belongs to a different
// upload attempt (another chunk uuid, or a direct upload): attempts cannot be
// stolen or re-bound across identities.
var ErrUploadAttemptMismatch = errors.New("upload id belongs to a different upload attempt")

// RelayUpload is one durable upload receipt.
type RelayUpload struct {
	ID        string
	KeyID     string
	UUID      string
	Filename  string
	Filesize  int64
	State     string
	Message   string
	FileID    string
	Size      int64
	CRC64     string
	SHA256    string
	Claimed   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

const relayUploadColumns = `SELECT id, key_id, uuid, filename, filesize, state, message, file_id, size, crc64, sha256, claimed, created_at, updated_at
	FROM relay_uploads`

func scanRelayUpload(row interface{ Scan(...any) error }) (RelayUpload, error) {
	var r RelayUpload
	var created, updated string
	var claimed int
	if err := row.Scan(&r.ID, &r.KeyID, &r.UUID, &r.Filename, &r.Filesize, &r.State, &r.Message,
		&r.FileID, &r.Size, &r.CRC64, &r.SHA256, &claimed, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RelayUpload{}, ErrNotFound
		}
		return RelayUpload{}, err
	}
	r.Claimed = claimed != 0
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	return r, nil
}

// ReserveRelayUpload atomically inserts a fresh receipt. A duplicate id is
// never reset — ErrUploadIDExists for the same key (retries need a new id,
// keeping prior outcomes truthful), ErrUploadIDTaken for another key. The
// single INSERT (primary key) makes the check race-free even under SQLite's
// serialized writes.
func (s *Store) ReserveRelayUpload(id, keyID, uploadUUID, filename string, filesize int64) error {
	_, err := s.db.Exec(`INSERT INTO relay_uploads (id, key_id, uuid, filename, filesize, state, message,
		file_id, size, crc64, sha256, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', 0, '', '', ?, ?)`,
		id, keyID, uploadUUID, filename, filesize, RelayUploadUploading, now(), now())
	if err == nil {
		return nil
	}
	if existing, gerr := s.GetRelayUpload(id); gerr == nil {
		if existing.KeyID != keyID {
			return ErrUploadIDTaken
		}
		return ErrUploadIDExists
	}
	return err
}

// RelayUploadTerminal reports whether state is one of the final outcomes.
func RelayUploadTerminal(state string) bool {
	switch state {
	case RelayUploadCompleted, RelayUploadFailed, RelayUploadInterrupted:
		return true
	}
	return false
}

// AdoptRelayUpload binds metadata to an EXISTING receipt of keyID (chunk
// completion flow): uuid/filename/filesize are refreshed but the state is
// never reset, so a terminal receipt cannot be revived. A receipt that
// already belongs to a different attempt (other chunk uuid, or a direct
// upload with no uuid) is refused, as is one whose completion is already
// claimed or finished. A missing row, a foreign owner and these states are
// distinct errors so callers can decide between 409, fresh reservation and
// duplicate rejection.
func (s *Store) AdoptRelayUpload(id, keyID, uploadUUID, filename string, filesize int64) error {
	r, err := s.GetRelayUpload(id)
	if err != nil {
		return err
	}
	if r.KeyID != keyID {
		return ErrUploadIDTaken
	}
	if RelayUploadTerminal(r.State) {
		return ErrReceiptTerminal
	}
	if r.Claimed {
		return ErrReceiptClaimed
	}
	if r.UUID == "" || r.UUID != uploadUUID {
		return ErrUploadAttemptMismatch
	}
	_, err = s.db.Exec(`UPDATE relay_uploads SET uuid=?, filename=?, filesize=?, updated_at=? WHERE id=? AND key_id=? AND state=?`,
		uploadUUID, filename, filesize, now(), id, keyID, RelayUploadUploading)
	return err
}

// ClaimRelayUpload atomically takes the completion claim of a receipt: the
// single compare-and-set UPDATE guarantees that of any number of concurrent
// duplicate completions exactly one proceeds to the upstream upload.
func (s *Store) ClaimRelayUpload(id, keyID string) error {
	res, err := s.db.Exec(`UPDATE relay_uploads SET claimed=1, updated_at=? WHERE id=? AND key_id=? AND state=? AND claimed=0`,
		now(), id, keyID, RelayUploadUploading)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		r, gerr := s.GetRelayUpload(id)
		if gerr != nil {
			return ErrNotFound
		}
		if RelayUploadTerminal(r.State) {
			return ErrReceiptTerminal
		}
		return ErrReceiptClaimed
	}
	return nil
}

// UpdateRelayUploadMeta refreshes the metadata of one of keyID's receipts
// without touching the state or outcome fields (repeated chunk staging).
func (s *Store) UpdateRelayUploadMeta(id, keyID, uploadUUID, filename string, filesize int64) error {
	res, err := s.db.Exec(`UPDATE relay_uploads SET uuid=?, filename=?, filesize=?, updated_at=? WHERE id=? AND key_id=?`,
		uploadUUID, filename, filesize, now(), id, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRelayUpload loads one receipt.
func (s *Store) GetRelayUpload(id string) (RelayUpload, error) {
	return scanRelayUpload(s.db.QueryRow(relayUploadColumns+` WHERE id=?`, id))
}

// RelayUploadByUUID finds the newest receipt of one key bound to a chunk
// upload uuid (the agent-side correlation key of chunked uploads).
func (s *Store) RelayUploadByUUID(keyID, uploadUUID string) (RelayUpload, error) {
	return scanRelayUpload(s.db.QueryRow(relayUploadColumns+` WHERE key_id=? AND uuid=? ORDER BY updated_at DESC LIMIT 1`,
		keyID, uploadUUID))
}

// FinishRelayUpload records the terminal outcome exactly once: the update is
// a compare-and-set from uploading, so a late callback or duplicate worker
// can never overwrite a truthful prior terminal state (monotone outcomes).
func (s *Store) FinishRelayUpload(id, state, message string, file *RelayFile) error {
	if !RelayUploadTerminal(state) {
		return fmt.Errorf("refusing to finish receipt %q with non-terminal state %q", id, state)
	}
	var fileID string
	var size int64
	var crc, sha string
	if state == RelayUploadCompleted && file != nil {
		fileID, size, crc, sha = file.ID, file.Size, file.CRC64, file.SHA256
	}
	res, err := s.db.Exec(`UPDATE relay_uploads SET state=?, message=?, file_id=?, size=?, crc64=?, sha256=?, updated_at=?
		WHERE id=? AND state=?`,
		state, message, fileID, size, crc, sha, now(), id, RelayUploadUploading)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, gerr := s.GetRelayUpload(id); gerr != nil {
			return ErrNotFound
		}
		return ErrReceiptTerminal
	}
	return nil
}

// InterruptRelayUploadsBefore moves receipts still marked uploading whose
// last update precedes the cutoff to interrupted. The boot hook passes the
// current time (no in-flight upload survives a restart); the maintenance pass
// passes a stale threshold so long-running legitimate uploads are untouched.
func (s *Store) InterruptRelayUploadsBefore(cutoff time.Time, message string) (int, error) {
	res, err := s.db.Exec(`UPDATE relay_uploads SET state=?, message=?, updated_at=? WHERE state=? AND updated_at<?`,
		RelayUploadInterrupted, message, now(), RelayUploadUploading, cutoff.UTC().Format(tsFormat))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PruneRelayUploads removes terminal receipts older than the cutoff
// (maintenance hook; receipts are diagnostics, not billing records).
func (s *Store) PruneRelayUploads(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM relay_uploads WHERE state!=? AND updated_at<?`,
		RelayUploadUploading, before.UTC().Format(tsFormat))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
