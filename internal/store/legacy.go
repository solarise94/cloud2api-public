package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// LegacyStoredAccount mirrors one account of the v0.3 admin state.json.
type LegacyStoredAccount struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	MountPath string    `json:"mount_path"`
	Enabled   bool      `json:"enabled"`
	StorageID int64     `json:"storage_id"`
	CreatedAt time.Time `json:"created_at"`
}

type legacyState struct {
	Accounts []LegacyStoredAccount `json:"accounts"`
}

// ImportLegacyState imports the v0.3 admin state.json (dynamic accounts) into
// the drives table on first start and renames the file to <path>.migrated
// (UPGRADE §5.1). A missing file is a no-op; a JSON parse error aborts
// startup so no account is silently lost.
func ImportLegacyState(s *Store, path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st legacyState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parse legacy state %s: %w", path, err)
	}
	for _, a := range st.Accounts {
		created := a.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		d := Drive{
			ID:        a.ID,
			Provider:  a.Provider,
			StorageID: a.StorageID,
			MountPath: a.MountPath,
			Enabled:   a.Enabled,
			Static:    false,
			CreatedAt: created,
		}
		if _, err := s.GetDrive(a.ID); errors.Is(err, ErrNotFound) {
			if err := s.InsertDrive(d); err != nil {
				return fmt.Errorf("import account %q: %w", a.ID, err)
			}
		}
		// An existing row (config static drive or earlier import) wins.
	}
	return os.Rename(path, path+".migrated")
}
