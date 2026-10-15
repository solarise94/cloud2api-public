// Package keymgr owns the API key lifecycle: creation, rotation, patching,
// revocation and authentication. It keeps an in-memory authenticator snapshot
// in sync with the SQLite store; every mutation rewrites the snapshot.
package keymgr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/store"
)

// ErrStaticKey is returned when a portal mutation targets a static
// (config.yaml) key.
var ErrStaticKey = errors.New("static keys (config.yaml) are read-only")

// ErrNotFound wraps store.ErrNotFound for callers that check errors.As.
var ErrNotFound = store.ErrNotFound

// publicIDAlphabet is the candidate alphabet for the 8-char public id;
// deliberately lowercase and unambiguous.
const publicIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

const secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Manager ties the store to the in-memory authenticator.
type Manager struct {
	st    *store.Store
	au    *auth.Authenticator
	mu    sync.Mutex // serializes mutations + reload
	touch map[string]time.Time
}

func New(st *store.Store) (*Manager, error) {
	m := &Manager{st: st, au: auth.NewAuthenticator(), touch: map[string]time.Time{}}
	if err := m.Reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// Authenticator exposes the lookup structure (read-only use).
func (m *Manager) Authenticator() *auth.Authenticator { return m.au }

// Reload rebuilds the in-memory key snapshot from the store.
func (m *Manager) Reload() error {
	keys, err := m.st.ListKeysWithGrants()
	if err != nil {
		return err
	}
	out := make([]*auth.Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, runtimeKey(k))
	}
	m.au.Reload(out)
	return nil
}

// runtimeKey converts a persisted key into the runtime representation.
func runtimeKey(k store.APIKey) *auth.Key {
	rk := &auth.Key{
		ID:           k.ID,
		PublicID:     k.PublicID,
		ParentID:     k.ParentID,
		Static:       k.Static,
		Revoked:      k.Revoked,
		CreatedAt:    k.CreatedAt,
		ExpiresAt:    k.ExpiresAt,
		LastUsedAt:   k.LastUsedAt,
		DefaultDrive: k.DefaultDrive,
		GlobalPerms:  map[auth.Permission]bool{},
		Relay: auth.RelayConfig{
			Drive:      k.Relay.Drive,
			QuotaBytes: k.Relay.QuotaBytes,
			MaxAgeDays: k.Relay.MaxAgeDays,
		},
		TrashOn:   k.Trash.Enabled,
		TrashDays: k.Trash.Days,
	}
	rk.SetHash(k.Hash)
	for _, g := range k.Drives {
		perms := map[auth.Permission]bool{}
		for _, p := range g.Perms {
			perms[auth.Permission(p)] = true
		}
		rk.Drives = append(rk.Drives, auth.KeyDrive{Drive: g.Drive, Root: g.Root, Perms: perms})
	}
	for _, p := range k.GlobalPerms {
		rk.GlobalPerms[auth.Permission(p)] = true
	}
	if k.Limits.MaxUploadBytes != nil {
		rk.Limits.MaxUploadBytes = *k.Limits.MaxUploadBytes
	}
	if k.Limits.RequestsPerMinute != nil {
		rk.Limits.RequestsPerMinute = *k.Limits.RequestsPerMinute
	}
	if k.Limits.ConcurrentJobs != nil {
		rk.Limits.ConcurrentJobs = *k.Limits.ConcurrentJobs
	}
	return rk
}

// Authenticate verifies a presented key.
func (m *Manager) Authenticate(presented string) (*auth.Key, auth.AuthenticateOutcome) {
	k, out := m.au.Authenticate(presented)
	if out == auth.AuthOK && k != nil {
		m.touchUsed(k.ID)
	}
	return k, out
}

// touchUsed persists last_used_at at most once per minute per key.
func (m *Manager) touchUsed(id string) {
	m.mu.Lock()
	last, seen := m.touch[id]
	if seen && time.Since(last) < time.Minute {
		m.mu.Unlock()
		return
	}
	m.touch[id] = time.Now()
	m.mu.Unlock()
	m.st.TouchKeyUsed(id)
}

func randomString(alphabet string, n int) (string, error) {
	var sb strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String(), nil
}

// GeneratePlaintext mints a new sk-c2a-<public>-<secret> plaintext together
// with its public id.
func GeneratePlaintext() (plain, publicID string, err error) {
	publicID, err = randomString(publicIDAlphabet, 8)
	if err != nil {
		return "", "", err
	}
	secret, err := randomString(secretAlphabet, 32)
	if err != nil {
		return "", "", err
	}
	return "sk-c2a-" + publicID + "-" + secret, publicID, nil
}

// ValidateKeySpec checks an admin-submitted key definition. Known drives
// bounds the drive whitelist; root must be a clean absolute path.
func ValidateKeySpec(spec store.APIKey, knownDrives map[string]bool) error {
	if len(spec.Drives) == 0 {
		return errors.New("at least one drive grant is required")
	}
	seen := map[string]bool{}
	for _, g := range spec.Drives {
		if !knownDrives[g.Drive] {
			return fmt.Errorf("unknown drive %q", g.Drive)
		}
		if seen[g.Drive] {
			return fmt.Errorf("drive %q granted twice", g.Drive)
		}
		seen[g.Drive] = true
		if g.Root == "" {
			g.Root = "/"
		}
		if g.Root != "/" && (!strings.HasPrefix(g.Root, "/") || strings.Contains(g.Root, "..") || strings.Contains(g.Root, "\x00") || strings.Contains(g.Root, "\\")) {
			return fmt.Errorf("drive %q: root must be a clean absolute path inside the drive", g.Drive)
		}
		for _, p := range g.Perms {
			if !auth.ValidPermission(auth.Permission(p)) {
				return fmt.Errorf("drive %q: unknown permission %q", g.Drive, p)
			}
		}
	}
	for _, p := range spec.GlobalPerms {
		if !auth.ValidPermission(auth.Permission(p)) {
			return fmt.Errorf("unknown global permission %q", p)
		}
	}
	if spec.DefaultDrive != "" && !seen[spec.DefaultDrive] {
		return fmt.Errorf("default_drive %q is not among the granted drives", spec.DefaultDrive)
	}
	if spec.Trash.Days < 0 || spec.Trash.Days > 90 {
		return errors.New("trash.days must be between 0 and 90")
	}
	// Relay settings (UPGRADE §4.7): the drive must be granted when set,
	// quota must be positive-or-default and the retention cap is 7 days
	// (0 selects the default of 7).
	if spec.Relay.Drive != "" && !seen[spec.Relay.Drive] {
		return fmt.Errorf("relay.drive %q is not among the granted drives", spec.Relay.Drive)
	}
	if spec.Relay.QuotaBytes < 0 {
		return errors.New("relay.quota_bytes must be >= 0 (0 selects the 200 GiB default)")
	}
	if spec.Relay.MaxAgeDays < 0 || spec.Relay.MaxAgeDays > store.RelayMaxAgeDays {
		return fmt.Errorf("relay.max_age_days must be between 0 and %d (0 selects the default of %d)",
			store.RelayMaxAgeDays, store.RelayMaxAgeDays)
	}
	if spec.Limits.MaxUploadBytes != nil && *spec.Limits.MaxUploadBytes < 0 {
		return errors.New("limits.max_upload_bytes must be >= 0")
	}
	if spec.Limits.RequestsPerMinute != nil && *spec.Limits.RequestsPerMinute < 0 {
		return errors.New("limits.requests_per_minute must be >= 0")
	}
	if spec.Limits.ConcurrentJobs != nil && *spec.Limits.ConcurrentJobs < 0 {
		return errors.New("limits.concurrent_jobs must be >= 0")
	}
	return nil
}

// Create stores a new key and returns it with the one-time plaintext.
func (m *Manager) Create(spec store.APIKey) (store.APIKey, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(spec.ID) == "" {
		return store.APIKey{}, "", errors.New("key id must not be empty")
	}
	if existing, err := m.st.GetKey(spec.ID); err == nil {
		if !existing.Revoked {
			return store.APIKey{}, "", fmt.Errorf("key %q already exists", spec.ID)
		}
		// A revoked key's id may be recycled by a fresh create.
		if err := m.st.DeleteKey(spec.ID); err != nil {
			return store.APIKey{}, "", err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.APIKey{}, "", err
	}
	plain, publicID, err := GeneratePlaintext()
	if err != nil {
		return store.APIKey{}, "", err
	}
	spec.PublicID = publicID
	spec.Hash = auth.HashKey(plain)
	spec.Static = false
	spec.Revoked = false
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now().UTC()
	}
	if spec.Trash.Days == 0 && spec.Trash.Enabled {
		spec.Trash.Days = 7
	}
	if err := m.st.InsertKey(spec); err != nil {
		return store.APIKey{}, "", err
	}
	created, err := m.st.GetKeyWithGrants(spec.ID)
	if err != nil {
		return store.APIKey{}, "", err
	}
	if err := m.Reload(); err != nil {
		return store.APIKey{}, "", err
	}
	return created, plain, nil
}

// Rotate replaces the secret of a key (keeping its id, grants and settings)
// and returns the new one-time plaintext.
func (m *Manager) Rotate(id string) (store.APIKey, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, err := m.st.GetKeyWithGrants(id)
	if err != nil {
		return store.APIKey{}, "", err
	}
	if k.Static {
		return store.APIKey{}, "", ErrStaticKey
	}
	plain, publicID, err := GeneratePlaintext()
	if err != nil {
		return store.APIKey{}, "", err
	}
	if err := m.st.RotateKeyHash(id, publicID, auth.HashKey(plain)); err != nil {
		return store.APIKey{}, "", err
	}
	if err := m.Reload(); err != nil {
		return store.APIKey{}, "", err
	}
	out, err := m.st.GetKeyWithGrants(id)
	return out, plain, err
}

// Revoke revokes a key and cascades to its children. Revoking an
// already-revoked key hard-deletes it instead (idempotent cleanup of test
// keys without leaving clutter in the portal).
func (m *Manager) Revoke(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, err := m.st.GetKey(id)
	if err != nil {
		return err
	}
	if k.Static {
		return ErrStaticKey
	}
	if k.Revoked {
		if err := m.st.DeleteKey(id); err != nil {
			return err
		}
		return m.Reload()
	}
	if err := m.st.RevokeKey(id); err != nil {
		return err
	}
	return m.Reload()
}

// Patch applies mutate to the stored key and reloads.
func (m *Manager) Patch(id string, mutate func(*store.APIKey) error) (store.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, err := m.st.GetKeyWithGrants(id)
	if err != nil {
		return store.APIKey{}, err
	}
	if k.Static {
		return store.APIKey{}, ErrStaticKey
	}
	if k.Revoked {
		return store.APIKey{}, errors.New("revoked keys cannot be modified")
	}
	if err := mutate(&k); err != nil {
		return store.APIKey{}, err
	}
	if err := m.st.UpdateKey(k); err != nil {
		return store.APIKey{}, err
	}
	if err := m.Reload(); err != nil {
		return store.APIKey{}, err
	}
	return m.st.GetKeyWithGrants(id)
}

// Get exposes one stored key.
func (m *Manager) Get(id string) (store.APIKey, error) { return m.st.GetKeyWithGrants(id) }

// List exposes every stored key.
func (m *Manager) List() ([]store.APIKey, error) { return m.st.ListKeysWithGrants() }
