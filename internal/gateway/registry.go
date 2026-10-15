package gateway

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"cloud2api/internal/config"
)

// AccountRegistry is the thread-safe runtime account table backing the
// gateway's routing decisions. Static accounts come from config.yaml (they
// can never be modified through the portal); dynamic accounts are created by
// the admin portal and live in the state file. Any mutation rebuilds the
// mount prefix list used for path rewriting.
type AccountRegistry struct {
	mu     sync.RWMutex
	accts  map[string]config.Account
	static map[string]bool
	mounts []string // distinct mount paths, longest first
}

func NewAccountRegistry(static []config.Account) *AccountRegistry {
	r := &AccountRegistry{
		accts:  make(map[string]config.Account, len(static)),
		static: make(map[string]bool, len(static)),
	}
	for _, a := range static {
		r.accts[a.ID] = a
		r.static[a.ID] = true
		r.mounts = append(r.mounts, a.MountPath)
	}
	r.rebuildMountsLocked()
	return r
}

func (r *AccountRegistry) rebuildMountsLocked() {
	seen := make(map[string]bool, len(r.accts))
	r.mounts = r.mounts[:0]
	for _, a := range r.accts {
		if !seen[a.MountPath] {
			seen[a.MountPath] = true
			r.mounts = append(r.mounts, a.MountPath)
		}
	}
	sort.Slice(r.mounts, func(i, j int) bool { return len(r.mounts[i]) > len(r.mounts[j]) })
}

func (r *AccountRegistry) Get(id string) (config.Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.accts[id]
	return a, ok
}

// Static reports whether the account was defined in config.yaml and is
// therefore protected from portal mutations.
func (r *AccountRegistry) Static(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.static[id]
}

// List returns all accounts sorted by id.
func (r *AccountRegistry) List() []config.Account {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]config.Account, 0, len(r.accts))
	for _, a := range r.accts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Mounts returns a copy of the distinct mount paths, longest first.
func (r *AccountRegistry) Mounts() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.mounts...)
}

// HasMount reports whether any registered account already uses mp.
func (r *AccountRegistry) HasMount(mp string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.accts {
		if a.MountPath == mp {
			return true
		}
	}
	return false
}

// Add registers a new dynamic (portal-created) account. It applies the same
// validation rules as config and rejects conflicts with any existing account
// id or mount path.
func (r *AccountRegistry) Add(a config.Account) error {
	if err := config.ValidateAccountID(a.ID); err != nil {
		return err
	}
	if err := config.ValidateMountPath(a.MountPath); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.accts[a.ID]; exists {
		return fmt.Errorf("account id %q already exists", a.ID)
	}
	for _, other := range r.accts {
		if other.MountPath == a.MountPath {
			return fmt.Errorf("mount_path %q already in use by account %q", a.MountPath, other.ID)
		}
	}
	r.accts[a.ID] = a
	r.rebuildMountsLocked()
	return nil
}

// SetEnabled flips an account's enabled flag.
func (r *AccountRegistry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accts[id]
	if !ok {
		return fmt.Errorf("account %q not found", id)
	}
	a.Enabled = enabled
	r.accts[id] = a
	return nil
}

// SetMount updates an account's mount path. Used by the reconcile loop when
// the storage's mount was renamed in OpenList (cloud2api follows; static
// config accounts too — config.yaml stays the source of their id only,
// UPGRADE §3.7.4).
func (r *AccountRegistry) SetMount(id string, mount string) error {
	if err := config.ValidateMountPath(mount); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accts[id]
	if !ok {
		return fmt.Errorf("account %q not found", id)
	}
	if a.MountPath == mount {
		return nil
	}
	for other, o := range r.accts {
		if other != id && o.MountPath == mount {
			return fmt.Errorf("mount_path %q already in use by account %q", mount, other)
		}
	}
	a.MountPath = mount
	r.accts[id] = a
	r.rebuildMountsLocked()
	return nil
}

// Remove deletes a dynamic account; static accounts are refused.
func (r *AccountRegistry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.static[id] {
		return fmt.Errorf("account %q is static (config.yaml) and cannot be removed via the portal", id)
	}
	if _, ok := r.accts[id]; !ok {
		return errors.New("account not found")
	}
	delete(r.accts, id)
	r.rebuildMountsLocked()
	return nil
}
