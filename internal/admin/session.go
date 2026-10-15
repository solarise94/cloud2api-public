package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/store"
)

// Administrator sessions used to live only in process memory (restart =
// logout everywhere). DESIGN-persistent-workspaces.md §3 moves them into the
// shared SQLite web_sessions table without changing the wire contract:
//
//   - the browser keeps an opaque 256-bit random token in the `session`
//     cookie (Path=/admin, HttpOnly, SameSite=Strict, Secure on TLS);
//   - the database only ever sees SHA-256(token), kind="admin" and an empty
//     workspace id;
//   - every row is bound to the current admin password through a
//     purpose-separated HMAC credential tag under a persistent random
//     secret, so changing the password fails all old sessions closed;
//   - expiry is fixed at login (cfg.SessionTTL, default 24h) — no sliding
//     renewal;
//   - every database error fails closed: no in-memory fallback, no ignored
//     write failures, and a logout that could not revoke reports failure
//     instead of pretending.

const (
	// sessionTokenBytes is 256 bit of cookie token entropy.
	sessionTokenBytes = 32
	// adminSessionSubject is the fixed subject_id of administrator rows.
	adminSessionSubject = "admin"
	// adminSessionSecretSetting is the settings key of the random HMAC
	// secret used only for admin session credential tags. It is
	// deliberately separate from the agentapi link/ticket secret: two
	// purposes never share a key (DESIGN §3 用途分离).
	adminSessionSecretSetting = "admin_session_secret"
	// adminCredDomain domain-separates this HMAC from every other use of
	// any secret in the process.
	adminCredDomain = "c2a-admin-session-cred\x00"
	// sessionCleanupInterval is how often expired web session rows are
	// purged (both admin and files kinds share the table).
	sessionCleanupInterval = time.Minute
)

// webSessionStore is the admin-relevant slice of the shared SQLite web
// session contract (DESIGN §3). *store.Store implements it once
// internal/store/web_sessions.go lands; tests use fakes.
type webSessionStore interface {
	PutWebSession(v store.WebSession) error
	GetWebSession(tokenHash, kind, workspaceID string) (store.WebSession, error)
	DeleteWebSession(tokenHash, kind, workspaceID string) error
	DeleteExpiredWebSessions(now time.Time) error
}

// hashSessionToken maps a bearer token to its stored form. Only the SHA-256
// of the token is persisted, so a database leak never yields a usable cookie.
func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// resolveAdminSessionSecret loads the persistent admin-session HMAC secret
// from the settings table, creating and persisting a fresh random secret on
// first use. A database failure is returned as an error — the caller must
// fail closed (no ephemeral fallback secret, DESIGN §3).
func resolveAdminSessionSecret(st *store.Store) ([]byte, error) {
	settings, err := st.AllSettings()
	if err != nil {
		return nil, fmt.Errorf("read admin session secret: %w", err)
	}
	if v := strings.TrimSpace(settings[adminSessionSecretSetting]); v != "" {
		return []byte(v), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate admin session secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	if err := st.SetSetting(adminSessionSecretSetting, secret); err != nil {
		return nil, fmt.Errorf("persist admin session secret: %w", err)
	}
	return []byte(secret), nil
}

// adminCredentialTag derives the purpose-separated HMAC fingerprint of the
// current admin password. Sessions record only this tag — never the password
// itself and nothing that could log in. Changing the password changes every
// tag, so old sessions fail closed, including across restarts.
func adminCredentialTag(secret []byte, password string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(adminCredDomain + password))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// sessionBackend is everything the Portal needs from its session store. The
// production Portal always uses sqliteSessions (or disabledSessions when the
// database is unavailable); the in-memory SessionStore below is kept for the
// existing unit tests only.
type sessionBackend interface {
	// Create mints a token bound to the given credential tag.
	Create(credTag string) (string, error)
	// Validate reports whether the token is known, unexpired and still
	// bound to the given credential tag. Any storage error is fail closed.
	Validate(token, credTag string) bool
	// Delete revokes a session; the error is reported to the user.
	Delete(token string) error
	StartCleanup(ctx context.Context)
}

// sqliteSessions persists admin sessions in the shared web_sessions table.
type sqliteSessions struct {
	st  webSessionStore
	ttl time.Duration
	now func() time.Time // test hook
	log *slog.Logger
}

// newSQLiteSessions builds the persistent admin session store over st with
// the configured fixed TTL.
func newSQLiteSessions(st webSessionStore, ttl time.Duration, log *slog.Logger) *sqliteSessions {
	if ttl <= 0 {
		ttl = config.DefaultSessionTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &sqliteSessions{st: st, ttl: ttl, now: time.Now, log: log}
}

// Create mints a fresh 256-bit token and stores only its SHA-256 alongside
// kind="admin", an empty workspace id and the credential tag of the current
// password. The absolute expiry is fixed at creation time.
func (s *sqliteSessions) Create(credTag string) (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	now := s.now().UTC()
	if err := s.st.PutWebSession(store.WebSession{
		TokenHash:     hashSessionToken(token),
		Kind:          store.SessionKindAdmin,
		WorkspaceID:   "",
		SubjectID:     adminSessionSubject,
		CredentialTag: credTag,
		CreatedAt:     now,
		ExpiresAt:     now.Add(s.ttl),
	}); err != nil {
		return "", fmt.Errorf("persist session: %w", err)
	}
	return token, nil
}

// Validate re-verifies every stored dimension (§4): kind, workspace id,
// subject id, credential tag and expiry. A missing row or any storage error
// fails closed.
func (s *sqliteSessions) Validate(token, credTag string) bool {
	if token == "" || credTag == "" {
		return false
	}
	row, err := s.st.GetWebSession(hashSessionToken(token), store.SessionKindAdmin, "")
	if err != nil {
		return false
	}
	if row.Kind != store.SessionKindAdmin || row.WorkspaceID != "" || row.SubjectID != adminSessionSubject {
		return false
	}
	if s.now().After(row.ExpiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(row.CredentialTag), []byte(credTag)) == 1
}

// Delete revokes the session row. The error travels to the caller so a
// failing logout is never reported as success.
func (s *sqliteSessions) Delete(token string) error {
	if token == "" {
		return errors.New("logout without a session token")
	}
	return s.st.DeleteWebSession(hashSessionToken(token), store.SessionKindAdmin, "")
}

// StartCleanup purges expired rows (all kinds share the table) until ctx is
// cancelled. Cleanup failures are logged and retried on the next tick.
func (s *sqliteSessions) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(sessionCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.cleanupOnce(s.now().UTC()); err != nil {
					s.log.Warn("expired session cleanup failed", "err", err)
				}
			}
		}
	}()
}

// cleanupOnce runs one expired-session purge (the test hook behind
// StartCleanup) and surfaces storage failures to the caller.
func (s *sqliteSessions) cleanupOnce(now time.Time) error {
	return s.st.DeleteExpiredWebSessions(now)
}

// disabledSessions is the fail-closed backend used when the session storage
// is unavailable at startup: no session can be created or validated, and
// logout reports failure instead of claiming a revocation that never
// happened (DESIGN §3: 数据库错误一律 fail closed).
type disabledSessions struct{ reason error }

func (d disabledSessions) Create(string) (string, error) {
	return "", fmt.Errorf("admin session storage unavailable: %w", d.reason)
}

func (d disabledSessions) Validate(string, string) bool { return false }

func (d disabledSessions) Delete(string) error {
	return fmt.Errorf("admin session storage unavailable: %w", d.reason)
}

func (disabledSessions) StartCleanup(context.Context) {}

// SessionStore keeps admin session tokens in memory only: restarting the
// process invalidates every session, and nothing session-related is
// persisted. It remains for the existing unit tests; the production Portal
// uses the SQLite-backed store above.
type SessionStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	sessions map[string]time.Time // token -> expiry
}

func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{
		ttl:      ttl,
		sessions: make(map[string]time.Time),
	}
}

// Create mints a fresh random session token valid for the configured TTL.
func (s *SessionStore) Create() (string, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(s.ttl)
	return token, nil
}

// Validate reports whether the token is known and unexpired; expired entries
// are dropped lazily.
func (s *SessionStore) Validate(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Delete removes a session (logout).
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// StartCleanup launches a background goroutine that drops expired sessions
// until ctx is cancelled.
func (s *SessionStore) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				s.mu.Lock()
				for token, expiry := range s.sessions {
					if now.After(expiry) {
						delete(s.sessions, token)
					}
				}
				s.mu.Unlock()
			}
		}
	}()
}
