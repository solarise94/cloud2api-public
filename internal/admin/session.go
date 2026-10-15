package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionStore keeps admin session tokens in memory only: restarting the
// process invalidates every session, and nothing session-related is
// persisted.
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

const sessionTokenBytes = 32

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
