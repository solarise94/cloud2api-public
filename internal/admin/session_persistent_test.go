package admin

// Tests for the persistent administrator sessions (DESIGN-persistent-workspaces
// §3): fixed TTL, SHA-256-only storage, kind/workspace/subject/credential-tag
// verification, password-change and restart behaviour, fail-closed storage
// errors and honest logout failures. Only synthetic fixtures are used.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// --- fake session database (logic tests without SQLite) ---

type fakeSessionDB struct {
	mu     sync.Mutex
	rows   map[string]store.WebSession // "hash|kind|workspace"
	getErr error
	putErr error
	delErr error
	expErr error
}

func newFakeSessionDB() *fakeSessionDB {
	return &fakeSessionDB{rows: map[string]store.WebSession{}}
}

func (f *fakeSessionDB) PutWebSession(v store.WebSession) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[v.TokenHash+"|"+v.Kind+"|"+v.WorkspaceID] = v
	return nil
}

func (f *fakeSessionDB) GetWebSession(tokenHash, kind, workspaceID string) (store.WebSession, error) {
	if f.getErr != nil {
		return store.WebSession{}, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.rows[tokenHash+"|"+kind+"|"+workspaceID]
	if !ok {
		return store.WebSession{}, store.ErrNotFound
	}
	return v, nil
}

func (f *fakeSessionDB) DeleteWebSession(tokenHash, kind, workspaceID string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, tokenHash+"|"+kind+"|"+workspaceID)
	return nil
}

func (f *fakeSessionDB) DeleteExpiredWebSessions(now time.Time) error {
	if f.expErr != nil {
		return f.expErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.rows {
		if !v.ExpiresAt.After(now) {
			delete(f.rows, k)
		}
	}
	return nil
}

// --- helpers ---

// newSessionPortal builds a minimal Portal over one SQLite file for session
// tests (the login/logout/validate paths never touch OpenList or the data
// plane).
func newSessionPortal(t *testing.T, dbPath, password string) (*Portal, *store.Store) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSessionPortalOnStore(t, st, password), st
}

func newSessionPortalOnStore(t *testing.T, st *store.Store, password string) *Portal {
	t.Helper()
	client, err := openlist.New("http://127.0.0.1:1", "unused")
	if err != nil {
		t.Fatal(err)
	}
	return NewPortal(&config.Admin{
		Enabled:    true,
		Password:   password,
		SessionTTL: config.Duration(time.Hour),
	}, gateway.NewAccountRegistry(nil), client, st, nil, nil)
}

func portalLogin(t *testing.T, p *Portal, password string) string {
	t.Helper()
	h := p.Wrap(http.NotFoundHandler())
	rec := doPortal(t, h, http.MethodPost, "/admin/api/login", "", map[string]string{"password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatal("no session cookie on login")
	return ""
}

func pageStatus(t *testing.T, p *Portal, path, token string) int {
	t.Helper()
	rec := doPortal(t, p.Wrap(http.NotFoundHandler()), http.MethodGet, path, sessionCookieName+"="+token, nil)
	return rec.Code
}

// --- sqliteSessions unit tests ---

func TestSQLiteSessionsCreateValidateDelete(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	_, st := newSessionPortal(t, db, adminPassword)
	s := newSQLiteSessions(st, time.Hour, nil)

	token, err := s.Create("tag-1")
	if err != nil || token == "" {
		t.Fatalf("create = %q, %v", token, err)
	}
	if len(token) < 40 { // 256 bit base64url
		t.Fatalf("token entropy too small: %d chars", len(token))
	}
	if !s.Validate(token, "tag-1") {
		t.Fatal("fresh session did not validate")
	}
	if s.Validate(token, "tag-2") || s.Validate(token, "") || s.Validate("", "tag-1") {
		t.Fatal("wrong or empty credential tag validated")
	}
	// The database row carries only the SHA-256 and the fixed identity.
	row, err := st.GetWebSession(hashSessionToken(token), store.SessionKindAdmin, "")
	if err != nil {
		t.Fatalf("row missing: %v", err)
	}
	// Independently recompute the hash: the stored form is the plain hex
	// SHA-256 of the token, never the token itself.
	sum := sha256.Sum256([]byte(token))
	raw, err := hex.DecodeString(row.TokenHash)
	if err != nil || !bytes.Equal(raw, sum[:]) {
		t.Fatalf("token hash is not the hex SHA-256 of the token: %q", row.TokenHash)
	}
	if row.TokenHash == token {
		t.Fatal("raw token stored in the database")
	}
	if row.Kind != store.SessionKindAdmin || row.WorkspaceID != "" || row.SubjectID != adminSessionSubject || row.CredentialTag != "tag-1" {
		t.Fatalf("row identity wrong: %+v", row)
	}
	if row.ExpiresAt.Sub(row.CreatedAt) != time.Hour {
		t.Fatalf("fixed TTL not respected: %v", row.ExpiresAt.Sub(row.CreatedAt))
	}
	if err := s.Delete(token); err != nil {
		t.Fatalf("delete = %v", err)
	}
	if s.Validate(token, "tag-1") {
		t.Fatal("deleted session validated")
	}
	if err := s.Delete(token); err != nil { // idempotent
		t.Fatalf("second delete = %v", err)
	}
}

func TestSQLiteSessionsFixedExpiryAndCleanup(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	_, st := newSessionPortal(t, db, adminPassword)

	// A negative TTL creates an already-expired session: never valid. (The
	// production constructor normalises ttl <= 0 to the default, so the
	// struct is built directly here.)
	expired := &sqliteSessions{st: st, ttl: -time.Second, now: time.Now, log: defaultTestLogger()}
	token, err := expired.Create("tag")
	if err != nil {
		t.Fatal(err)
	}
	if expired.Validate(token, "tag") {
		t.Fatal("already-expired session validated")
	}

	live := newSQLiteSessions(st, time.Hour, nil)
	liveToken, err := live.Create("tag")
	if err != nil {
		t.Fatal(err)
	}
	if !live.Validate(liveToken, "tag") {
		t.Fatal("live session did not validate")
	}

	// Fixed expiry: advancing the clock past the TTL invalidates, no sliding.
	future := newSQLiteSessions(st, time.Hour, nil)
	later := time.Now().Add(2 * time.Hour)
	future.now = func() time.Time { return later }
	if future.Validate(liveToken, "tag") {
		t.Fatal("session survived past its fixed expiry")
	}
	future.cleanupOnce(later.UTC())
	if live.Validate(liveToken, "tag") || live.Validate(token, "tag") {
		t.Fatal("expired rows survived cleanup")
	}
	if err := st.DeleteExpiredWebSessions(time.Now()); err != nil {
		t.Fatalf("shared cleanup = %v", err)
	}
}

func TestSessionValidateVerifiesEveryRowField(t *testing.T) {
	fake := newFakeSessionDB()
	s := newSQLiteSessions(fake, time.Hour, nil)
	token, err := s.Create("tag-1")
	if err != nil {
		t.Fatal(err)
	}
	good, err := fake.GetWebSession(hashSessionToken(token), store.SessionKindAdmin, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Validate(token, "tag-1") {
		t.Fatal("untampered row did not validate")
	}
	for name, mutate := range map[string]func(*store.WebSession){
		"wrong-kind":      func(r *store.WebSession) { r.Kind = store.SessionKindFiles },
		"wrong-workspace": func(r *store.WebSession) { r.WorkspaceID = "ws1" },
		"wrong-subject":   func(r *store.WebSession) { r.SubjectID = "someone-else" },
		"wrong-tag":       func(r *store.WebSession) { r.CredentialTag = "tag-2" },
		"empty-tag":       func(r *store.WebSession) { r.CredentialTag = "" },
		"expired":         func(r *store.WebSession) { r.ExpiresAt = time.Now().Add(-time.Minute) },
	} {
		bad := good
		mutate(&bad)
		// Remove the genuine row first: wrong-kind/wrong-workspace variants
		// live under a different primary key and must never satisfy the
		// admin lookup either.
		if err := fake.DeleteWebSession(good.TokenHash, good.Kind, good.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		if err := fake.PutWebSession(bad); err != nil {
			t.Fatal(err)
		}
		if s.Validate(token, "tag-1") {
			t.Fatalf("tampered row (%s) validated", name)
		}
		if err := fake.DeleteWebSession(bad.TokenHash, bad.Kind, bad.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		if err := fake.PutWebSession(good); err != nil {
			t.Fatal(err)
		}
		if !s.Validate(token, "tag-1") {
			t.Fatalf("restored row did not validate (after %s)", name)
		}
	}
}

func TestSessionStorageFailuresFailClosed(t *testing.T) {
	boom := errors.New("db boom")

	getFail := newFakeSessionDB()
	s := newSQLiteSessions(getFail, time.Hour, nil)
	token, err := s.Create("tag")
	if err != nil {
		t.Fatal(err)
	}
	getFail.getErr = boom
	if s.Validate(token, "tag") {
		t.Fatal("read failure validated a session")
	}

	putFail := newFakeSessionDB()
	putFail.putErr = boom
	if _, err := newSQLiteSessions(putFail, time.Hour, nil).Create("tag"); err == nil {
		t.Fatal("write failure did not fail session creation")
	}

	delFail := newFakeSessionDB()
	delFail.delErr = boom
	if err := newSQLiteSessions(delFail, time.Hour, nil).Delete(token); err == nil {
		t.Fatal("delete failure not reported")
	}

	expFail := newFakeSessionDB()
	expFail.expErr = boom
	if err := newSQLiteSessions(expFail, time.Hour, nil).cleanupOnce(time.Now()); err == nil {
		t.Fatal("cleanup failure not surfaced to caller")
	}

	d := disabledSessions{reason: boom}
	if _, err := d.Create("tag"); err == nil {
		t.Fatal("disabled backend created a session")
	}
	if d.Validate("x", "y") {
		t.Fatal("disabled backend validated a session")
	}
	if err := d.Delete("x"); err == nil {
		t.Fatal("disabled backend claimed a revocation")
	}
}

// --- secret persistence ---

func TestResolveAdminSessionSecretPersistsAndFailsClosed(t *testing.T) {
	db := filepath.Join(t.TempDir(), "s.db")
	_, st := newSessionPortal(t, db, adminPassword)

	s1, err := resolveAdminSessionSecret(st)
	if err != nil || len(s1) == 0 {
		t.Fatalf("first resolve = %v (%v)", s1, err)
	}
	s2, err := resolveAdminSessionSecret(st)
	if err != nil || string(s1) != string(s2) {
		t.Fatalf("secret not stable across restarts: %q vs %q (%v)", s1, s2, err)
	}
	if v, ok := st.GetSetting("admin_session_secret"); !ok || v == "" {
		t.Fatal("secret was not persisted in settings")
	}
	// Another database gets an independent secret.
	_, st2 := newSessionPortal(t, filepath.Join(t.TempDir(), "other.db"), adminPassword)
	s3, err := resolveAdminSessionSecret(st2)
	if err != nil || string(s3) == string(s1) {
		t.Fatal("secrets are not independent per database")
	}
	// A broken database is an error, never a silent fallback.
	closed, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := resolveAdminSessionSecret(closed); err == nil {
		t.Fatal("resolve succeeded on a closed database")
	}
}

// --- portal-level behaviour ---

func TestAdminSessionSurvivesRestartLogoutAndPasswordChange(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "admin.db")

	pA, st := newSessionPortal(t, db, adminPassword)
	c1 := portalLogin(t, pA, adminPassword)
	if code := pageStatus(t, pA, "/admin/", c1); code != http.StatusOK {
		t.Fatalf("fresh session on /admin/ = %d", code)
	}

	// Restart with the same password: the session survives.
	st.Close()
	pB, stB := newSessionPortal(t, db, adminPassword)
	if code := pageStatus(t, pB, "/admin/", c1); code != http.StatusOK {
		t.Fatalf("session did not survive restart: %d", code)
	}

	// Logout persists across restarts.
	h := pB.Wrap(http.NotFoundHandler())
	if rec := doPortal(t, h, http.MethodPost, "/admin/api/logout", sessionCookieName+"="+c1, nil); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (%s)", rec.Code, rec.Body.String())
	}
	if code := pageStatus(t, pB, "/admin/", c1); code != http.StatusFound {
		t.Fatalf("logged-out session still served pages: %d", code)
	}
	stB.Close()
	pC, stC := newSessionPortal(t, db, adminPassword)
	if code := pageStatus(t, pC, "/admin/", c1); code != http.StatusFound {
		t.Fatalf("logout did not survive restart: %d", code)
	}

	// A password change fails every old session closed, across restart.
	c2 := portalLogin(t, pC, adminPassword)
	stC.Close()
	pD, _ := newSessionPortal(t, db, "changed-pw")
	if code := pageStatus(t, pD, "/admin/", c2); code != http.StatusFound {
		t.Fatalf("old password session survived password change: %d", code)
	}
	if code := pageStatus(t, pD, "/admin/", c1); code != http.StatusFound {
		t.Fatalf("long-dead session resurrected: %d", code)
	}
	// The new password works.
	c3 := portalLogin(t, pD, "changed-pw")
	if code := pageStatus(t, pD, "/admin/", c3); code != http.StatusOK {
		t.Fatalf("new password session rejected: %d", code)
	}
}

func TestAdminSessionSecretWipeFailsClosed(t *testing.T) {
	db := filepath.Join(t.TempDir(), "admin.db")
	pA, st := newSessionPortal(t, db, adminPassword)
	c1 := portalLogin(t, pA, adminPassword)

	// Wiping the persisted secret must not fall back to anything: a new
	// portal re-seals sessions under a fresh secret, old cookies die.
	if err := st.SetSetting("admin_session_secret", ""); err != nil {
		t.Fatal(err)
	}
	st.Close()
	pB, _ := newSessionPortal(t, db, adminPassword)
	if code := pageStatus(t, pB, "/admin/", c1); code != http.StatusFound {
		t.Fatalf("session survived a wiped secret: %d", code)
	}
}

func TestAdminSessionStorageFailureIsHonest(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	if rec := doPortal(t, h.handler, http.MethodGet, "/admin/", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("prerequisite: /admin/ = %d", rec.Code)
	}

	// Storage dies underneath the portal.
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}

	// Validation fails closed.
	if rec := doPortal(t, h.handler, http.MethodGet, "/admin/", cookie, nil); rec.Code != http.StatusFound {
		t.Fatalf("broken storage still served /admin/: %d", rec.Code)
	}
	if rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/settings", cookie, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("broken storage API check = %d, want 401", rec.Code)
	}
	// Login cannot mint sessions.
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", map[string]string{"password": adminPassword})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("login on broken storage = %d, want 500", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("failed login set cookies")
	}
	// Logout is fail closed too: with the storage broken the session cannot
	// be verified, so the API answers 401 and revocation is never claimed.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/logout", cookie, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout on broken storage = %d, want 401 (fail closed)", rec.Code)
	}
	// The login page itself still renders.
	if rec := doPortal(t, h.handler, http.MethodGet, "/admin/login", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("login page on broken storage = %d", rec.Code)
	}

	// The failed logout truly did not revoke: after the storage returns,
	// the same cookie is live again — the 500 was the honest answer.
	reopened, err := store.Open(h.st.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	pB := newSessionPortalOnStore(t, reopened, adminPassword)
	if code := pageStatus(t, pB, "/admin/", strings.TrimPrefix(cookie, sessionCookieName+"=")); code != http.StatusOK {
		t.Fatalf("revocation happened despite the reported failure: %d", code)
	}
}

func TestAdminLoginRedirectsValidSession(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/login", cookie, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("/admin/login with valid session = %d location=%q, want 302 /admin/", rec.Code, rec.Header().Get("Location"))
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/login", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("anonymous /admin/login = %d", rec.Code)
	}
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/login", "session=forged", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("forged cookie /admin/login = %d, want login page", rec.Code)
	}
}

func TestSessionCookieSecureAttributes(t *testing.T) {
	p, st := newSessionPortal(t, filepath.Join(t.TempDir(), "s.db"), adminPassword)
	h := p.Wrap(http.NotFoundHandler())

	do := func(setTLS bool, header map[string]string) *http.Cookie {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"password":"`+adminPassword+`"}`))
		req.Header.Set("Content-Type", "application/json")
		if setTLS {
			req.TLS = &tls.ConnectionState{}
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d (%s)", rec.Code, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookieName {
				return c
			}
		}
		t.Fatal("no session cookie")
		return nil
	}

	if c := do(false, nil); c.Secure {
		t.Error("plain LAN http login must not set Secure")
	}
	if c := do(true, nil); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/admin" {
		t.Errorf("direct TLS login cookie flags wrong: %+v", c)
	}
	// The explicitly configured https public_base_url is trusted.
	if err := st.SetSetting("public_base_url", "https://c2a.example.com"); err != nil {
		t.Fatal(err)
	}
	if c := do(false, nil); !c.Secure {
		t.Error("configured https public_base_url must set Secure")
	}
	if err := st.SetSetting("public_base_url", "http://c2a.example.com"); err != nil {
		t.Fatal(err)
	}
	if c := do(false, nil); c.Secure {
		t.Error("http public_base_url must not set Secure")
	}
	// Untrusted signals never enable Secure.
	if err := st.SetSetting("openlist_public_url", "https://openlist.example.com"); err != nil {
		t.Fatal(err)
	}
	if c := do(false, map[string]string{"X-Forwarded-Proto": "https"}); c.Secure {
		t.Error("X-Forwarded-Proto or openlist_public_url must not set Secure")
	}
}

// --- honest logout under storage failure ---

type failingSessions struct{ err error }

func (f failingSessions) Create(string) (string, error) { return "", f.err }
func (f failingSessions) Validate(string, string) bool  { return false }
func (f failingSessions) Delete(string) error           { return f.err }
func (failingSessions) StartCleanup(context.Context)    {}

func defaultTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestHandleLogoutStorageFailureDoesNotClaimRevocation proves the contract
// that a logout whose revocation write failed reports a server error, keeps
// the cookie, and never prints "success".
func TestHandleLogoutStorageFailureDoesNotClaimRevocation(t *testing.T) {
	p := &Portal{sessions: failingSessions{err: errors.New("db boom")}, log: defaultTestLogger()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/logout", nil)
	p.handleLogout(rec, req, "some-session-token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("logout = %d, want 500", rec.Code)
	}
	if m := decodePortal(t, rec); m["message"] == "success" {
		t.Fatal("logout reported success despite the storage failure")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Fatal("failed logout still cleared the session cookie")
		}
	}

	// Without a cookie there is nothing to revoke: clearing is honest.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/logout", nil)
	p.handleLogout(rec, req, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cookieless logout = %d, want 200", rec.Code)
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("cookieless logout did not clear the cookie")
	}
}

// --- kind isolation between admin and files sessions ---

func TestAdminSessionKindIsolation(t *testing.T) {
	db := filepath.Join(t.TempDir(), "s.db")
	_, st := newSessionPortal(t, db, adminPassword)
	s := newSQLiteSessions(st, time.Hour, nil)

	token, err := s.Create("tag")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The same browser token also carries a files-kind workspace row: it
	// neither authenticates admin nor is it touched by admin logout.
	filesRow := store.WebSession{
		TokenHash: hashSessionToken(token), Kind: store.SessionKindFiles,
		WorkspaceID: "ws1", SubjectID: "k1", CredentialTag: "x",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := st.PutWebSession(filesRow); err != nil {
		t.Fatal(err)
	}
	if !s.Validate(token, "tag") {
		t.Fatal("files-kind row broke the admin session")
	}
	if err := s.Delete(token); err != nil {
		t.Fatal(err)
	}
	if s.Validate(token, "tag") {
		t.Fatal("admin row survived logout")
	}
	if _, err := st.GetWebSession(hashSessionToken(token), store.SessionKindFiles, "ws1"); err != nil {
		t.Fatalf("admin logout disturbed the files-kind row: %v", err)
	}
}
