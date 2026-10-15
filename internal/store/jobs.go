package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Job states (UPGRADE §4.10.4).
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
)

// Job kinds.
const (
	JobKindTransfer = "transfer"
	JobKindFetch    = "fetch"
	JobKindBatch    = "batch"
)

// JobTerminal reports whether state needs no further tracking.
func JobTerminal(state string) bool {
	switch state {
	case JobSucceeded, JobFailed, JobCanceled:
		return true
	}
	return false
}

// JobActive reports whether the job still counts against concurrency limits.
func JobActive(state string) bool { return state == JobQueued || state == JobRunning }

// Job is one long-running operation (transfer, fetch or batch). Request holds
// the agent-visible request JSON (never internal OpenList paths); result holds
// incremental results plus the OpenList task ids used to resume tracking
// after a cloud2api restart.
type Job struct {
	ID        string
	KeyID     string
	Kind      string
	State     string
	Progress  float64
	Request   string
	Result    string
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const jobColumns = `SELECT id, key_id, kind, state, progress, request, result, error, created_at, updated_at FROM jobs`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var created, updated string
	if err := row.Scan(&j.ID, &j.KeyID, &j.Kind, &j.State, &j.Progress, &j.Request, &j.Result, &j.Error, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, err
	}
	j.CreatedAt = parseTime(created)
	j.UpdatedAt = parseTime(updated)
	return j, nil
}

// InsertJob creates a queued job.
func (s *Store) InsertJob(j Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (id, key_id, kind, state, progress, request, result, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		j.ID, j.KeyID, j.Kind, j.State, j.Progress, j.Request, j.Result, now(), now())
	return err
}

// GetJob loads one job.
func (s *Store) GetJob(id string) (Job, error) {
	return scanJob(s.db.QueryRow(jobColumns+` WHERE id=?`, id))
}

// UpdateJobState replaces state/progress/result/error.
func (s *Store) UpdateJobState(id, state string, progress float64, result, errMsg string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state=?, progress=?, result=?, error=?, updated_at=? WHERE id=?`,
		state, progress, result, errMsg, now(), id)
	return err
}

// PatchJobResult replaces only the result JSON (used for incremental batch
// results).
func (s *Store) PatchJobResult(id, result string) error {
	_, err := s.db.Exec(`UPDATE jobs SET result=?, updated_at=? WHERE id=?`, result, now(), id)
	return err
}

// ListJobs returns jobs newest-first. keyID and state are exact filters;
// empty means any.
func (s *Store) ListJobs(keyID, state string, limit int) ([]Job, error) {
	where, args := " WHERE 1=1", []any{}
	if keyID != "" {
		where += " AND key_id=?"
		args = append(args, keyID)
	}
	if state != "" {
		where += " AND state=?"
		args = append(args, state)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	args = append(args, limit)
	rows, err := s.db.Query(jobColumns+where+` ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ActiveJobCount counts queued+running jobs of one key (concurrency limits).
func (s *Store) ActiveJobCount(keyID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE key_id=? AND state IN ('queued','running')`, keyID).Scan(&n)
	return n, err
}

// ActiveJobs returns every queued/running job regardless of key (restart
// resume).
func (s *Store) ActiveJobs() ([]Job, error) {
	rows, err := s.db.Query(jobColumns + ` WHERE state IN ('queued','running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobResultJSON is a tiny helper for unmarshalling the result field.
func JobResultJSON(j Job) map[string]any {
	out := map[string]any{}
	if j.Result != "" {
		_ = json.Unmarshal([]byte(j.Result), &out)
	}
	return out
}
