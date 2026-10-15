// Package auth implements the API key model of v0.4 (UPGRADE §4.1/§4.2):
// hashing, constant-time verification, the permission vocabulary, per-drive
// chroot grants, limits and the dual key-format lookup.
//
// Key formats:
//
//	sk-c2a-<8 char public id>-<secret>   new format; the public id selects a
//	                                     small candidate set, then a
//	                                     constant-time hash compare runs
//	sk-<secret>                          legacy format; scanned linearly with
//	                                     the same constant-time compare
//
// Only SHA-256 hashes are ever stored.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Permission is one verb of the v0.4 vocabulary (UPGRADE §8.2). The legacy
// config vocabulary maps onto it: list/read/transfer keep their names and
// legacy "write" expands to write+delete.
type Permission string

const (
	PermList     Permission = "list"
	PermRead     Permission = "read"
	PermWrite    Permission = "write"
	PermDelete   Permission = "delete"
	PermPurge    Permission = "purge"
	PermTransfer Permission = "transfer"
	PermLink     Permission = "link"
	PermFetch    Permission = "fetch"
	PermDelegate Permission = "delegate"
	PermRelay    Permission = "relay" // Gokapi compatibility layer (UPGRADE §4.7)
)

// AllPermissions is the full vocabulary in canonical order.
var AllPermissions = []Permission{
	PermList, PermRead, PermWrite, PermDelete, PermPurge,
	PermTransfer, PermLink, PermFetch, PermDelegate, PermRelay,
}

var validPermissions = func() map[Permission]bool {
	m := make(map[Permission]bool, len(AllPermissions))
	for _, p := range AllPermissions {
		m[p] = true
	}
	return m
}()

func ValidPermission(p Permission) bool { return validPermissions[p] }

// AllPermissionNames is the vocabulary as a comma-separated list (error
// messages).
func AllPermissionNames() string {
	names := make([]string, 0, len(AllPermissions))
	for _, p := range AllPermissions {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

// MapLegacyPermission expands a v0.3 config permission into the v0.4
// vocabulary (write -> write+delete; the other legacy names and every new
// vocabulary name pass through; unknown values are dropped).
func MapLegacyPermission(p string) []Permission {
	if Permission(p) == PermWrite {
		return []Permission{PermWrite, PermDelete}
	}
	if ValidPermission(Permission(p)) {
		return []Permission{Permission(p)}
	}
	return nil
}

// KeyDrive is the runtime form of one per-drive grant.
type KeyDrive struct {
	Drive string
	Root  string
	Perms map[Permission]bool
}

// Limits is the runtime rate/size limit set; zero values mean unlimited.
type Limits struct {
	MaxUploadBytes    int64
	RequestsPerMinute int
	ConcurrentJobs    int
}

// RelayConfig is the runtime form of the per-key Gokapi relay settings
// (UPGRADE §4.7). Zero values resolve to the documented defaults at use.
type RelayConfig struct {
	Drive      string
	QuotaBytes int64
	MaxAgeDays int
}

// Key is an authenticated API key with its full authorization state.
type Key struct {
	ID           string
	PublicID     string // non-empty for sk-c2a keys
	ParentID     string
	Static       bool
	Revoked      bool
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	LastUsedAt   *time.Time
	DefaultDrive string
	// Drives is ordered: the first entry is the implicit default drive for
	// the /v1 default-drive aliases.
	Drives      []KeyDrive
	GlobalPerms map[Permission]bool
	Limits      Limits
	Relay       RelayConfig
	TrashOn     bool
	TrashDays   int

	hash string // stored "sha256:<hex>"
}

// Hash returns the stored digest form.
func (k *Key) Hash() string { return k.hash }

// SetHash attaches the stored digest form ("sha256:<hex>"); used by the key
// manager when materializing persisted rows.
func (k *Key) SetHash(h string) { k.hash = h }

// Prefix returns the display prefix of the key ("sk-c2a-xxxxxxxx" for new
// format keys, "" for legacy ones whose plaintext shape is unknown).
func (k *Key) Prefix() string {
	if k.PublicID == "" {
		return ""
	}
	return "sk-c2a-" + k.PublicID
}

// DriveGrant returns the grant for drive.
func (k *Key) DriveGrant(drive string) (KeyDrive, bool) {
	for _, d := range k.Drives {
		if d.Drive == drive {
			return d, true
		}
	}
	return KeyDrive{}, false
}

// HasAccount reports whether the key may address the drive at all
// (legacy API compatibility).
func (k *Key) HasAccount(id string) bool {
	_, ok := k.DriveGrant(id)
	return ok
}

// HasPermission reports whether the key carries a global permission
// (legacy API compatibility).
func (k *Key) HasPermission(p Permission) bool { return k.GlobalPerms[p] }

// DrivePerms returns the permission set granted on drive.
func (k *Key) DrivePerms(drive string) map[Permission]bool {
	if g, ok := k.DriveGrant(drive); ok {
		return g.Perms
	}
	return nil
}

// CanOn reports whether p is granted on drive either globally or per-drive.
func (k *Key) CanOn(drive string, p Permission) bool {
	if k.GlobalPerms[p] {
		return true
	}
	g, ok := k.DriveGrant(drive)
	return ok && g.Perms[p]
}

// EffectiveDefaultDrive resolves the default drive of the /v1 aliases: the
// explicit default_drive, else the first whitelisted drive (config order for
// static keys, grant order for portal keys). Documented rule, UPGRADE §4.3.
func (k *Key) EffectiveDefaultDrive() string {
	if k.DefaultDrive != "" {
		return k.DefaultDrive
	}
	if len(k.Drives) > 0 {
		return k.Drives[0].Drive
	}
	return ""
}

// EffectiveRelayDrive resolves the relay drive (UPGRADE §4.7): the explicit
// relay drive, else the key's effective default drive.
func (k *Key) EffectiveRelayDrive() string {
	if k.Relay.Drive != "" {
		return k.Relay.Drive
	}
	return k.EffectiveDefaultDrive()
}

// EffectiveRelayQuota resolves the relay capacity (default 200 GiB).
func (k *Key) EffectiveRelayQuota() int64 {
	if k.Relay.QuotaBytes > 0 {
		return k.Relay.QuotaBytes
	}
	return 200 << 30
}

// EffectiveRelayMaxAge resolves the relay retention cap in days (default and
// hard maximum 7).
func (k *Key) EffectiveRelayMaxAge() int {
	if k.Relay.MaxAgeDays > 0 && k.Relay.MaxAgeDays <= 7 {
		return k.Relay.MaxAgeDays
	}
	return 7
}

// Expired reports whether the key's validity window has passed.
func (k *Key) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// Active reports whether the key may authenticate at all.
func (k *Key) Active(now time.Time) bool {
	return !k.Revoked && !k.Expired(now)
}

const hashLen = len("sha256:") + sha256.Size*2

// HashKey returns the stored form ("sha256:<hex>") of a plaintext key.
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateHash checks the stored digest form.
func ValidateHash(h string) error {
	if len(h) != hashLen || !strings.HasPrefix(h, "sha256:") {
		return fmt.Errorf("key hash must be %q", "sha256:<64 hex chars>")
	}
	if _, err := hex.DecodeString(h[len("sha256:"):]); err != nil {
		return fmt.Errorf("key hash is not valid hex: %w", err)
	}
	return nil
}

// AuthenticateOutcome explains a failed authentication for the error message.
type AuthenticateOutcome int

const (
	AuthOK AuthenticateOutcome = iota
	AuthUnknownKey
	AuthRevoked
	AuthExpired
)

// Authenticator serves constant-time key lookups from an in-memory snapshot.
// The snapshot is replaced wholesale by Reload whenever the key set changes;
// in-flight lookups keep the previous snapshot.
type Authenticator struct {
	mu         sync.RWMutex
	byPublicID map[string][]*Key // public id -> candidates
	legacy     []*Key            // keys without a public id (config format)
	byID       map[string]*Key
}

func NewAuthenticator() *Authenticator {
	return &Authenticator{
		byPublicID: map[string][]*Key{},
		byID:       map[string]*Key{},
	}
}

// Reload atomically replaces the key set.
func (a *Authenticator) Reload(keys []*Key) {
	next := NewAuthenticator()
	for _, k := range keys {
		if err := ValidateHash(k.hash); err != nil {
			continue // a corrupt row must not take the gateway down
		}
		kk := k
		if k.PublicID != "" {
			next.byPublicID[k.PublicID] = append(next.byPublicID[k.PublicID], kk)
		} else {
			next.legacy = append(next.legacy, kk)
		}
		next.byID[k.ID] = kk
	}
	a.mu.Lock()
	a.byPublicID = next.byPublicID
	a.legacy = next.legacy
	a.byID = next.byID
	a.mu.Unlock()
}

// ByID exposes a key by its logical id (admin/display use).
func (a *Authenticator) ByID(id string) (*Key, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	k, ok := a.byID[id]
	return k, ok
}

// Authenticate verifies a presented plaintext key. New-format keys are routed
// by their public id; legacy keys are scanned. All comparisons are
// constant-time over fixed-length digests.
func (a *Authenticator) Authenticate(presented string) (*Key, AuthenticateOutcome) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()

	check := func(candidates []*Key) (*Key, AuthenticateOutcome) {
		sum := sha256.Sum256([]byte(presented))
		var candidate [hashLen]byte
		copy(candidate[:], "sha256:"+hex.EncodeToString(sum[:]))
		var match *Key
		var statusFound *Key
		for _, k := range candidates {
			if subtle.ConstantTimeCompare(candidate[:], []byte(k.hash)) == 1 {
				if !k.Active(now) {
					statusFound = k
					continue
				}
				match = k
			}
		}
		if match != nil {
			return match, AuthOK
		}
		if statusFound != nil {
			if statusFound.Revoked {
				return nil, AuthRevoked
			}
			return nil, AuthExpired
		}
		return nil, AuthUnknownKey
	}

	// New format: sk-c2a-<public>-<secret>. Exact-format keys try the
	// public-id path first; everything else (including a new-format-shaped
	// key with an unknown public id, in case a legacy random key merely
	// resembles the shape) falls through to the legacy scan so no key format
	// ever stops authenticating.
	if pub, secret, ok := SplitNewFormat(presented); ok && secret != "" {
		if cands := a.byPublicID[pub]; len(cands) > 0 {
			if k, out := check(cands); out != AuthUnknownKey {
				return k, out
			}
		}
	}
	return check(a.legacy)
}

// SplitNewFormat parses sk-c2a-<8 chars>-<secret> into its public id and
// secret. ok is false for any other shape.
func SplitNewFormat(plain string) (publicID, secret string, ok bool) {
	const prefix = "sk-c2a-"
	if !strings.HasPrefix(plain, prefix) {
		return "", "", false
	}
	rest := plain[len(prefix):]
	dash := strings.IndexByte(rest, '-')
	if dash != 8 {
		return "", "", false
	}
	return rest[:dash], rest[dash+1:], true
}
