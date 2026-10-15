package agentapi

// Browser file-workspace sessions (DESIGN-persistent-workspaces §2/§3/§4).
// The handler owns exactly four route families under /files:
//
//	POST /files/api/login            {api_key, workspace_id?} -> {workspace_id,url,expires_at}
//	GET  /files/api/workspaces       -> {workspaces:[{id,url,expires_at}]}
//	POST /files/w/<id>/api/logout    -> {ok:true}
//	*    /files/w/<id>/api/v1/<sfx>  same-origin adapter onto the existing /v1 dispatch
//
// The cookie (c2a_files, Path=/files) carries a random 256-bit token; the
// database stores only SHA-256(token). The token is not authorization: every
// request revalidates kind, workspace binding, subject key, credential tag
// and expiry against the fresh store state, then reuses the key's own
// limiter, audit row, storage guard and the /v1 dispatch unchanged. The
// machine surface (/v1, legacy API, relay) never accepts these cookies.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/store"
)

const (
	filesCookieName     = "c2a_files"
	filesCookiePath     = "/files"
	workspaceHeaderName = "X-C2A-Workspace"

	// workspaceTokenBytes is the cookie token entropy (256 bit).
	workspaceTokenBytes = 32
	// workspaceTTL is the fixed absolute validity of one workspace member
	// row (and of the cookie's send period after the latest explicit login);
	// there is no sliding renewal on use.
	workspaceTTL = 30 * 24 * time.Hour
	// maxBrowserWorkspaces caps how many valid workspace rows one browser
	// token may hold; further logins are rejected without touching the
	// existing members.
	maxBrowserWorkspaces = 16
	// wsLoginBodyLimit bounds the login JSON body.
	wsLoginBodyLimit = 64 << 10
)

// workspaceOpAliases are the /v1 ops the workspace UI surface may call via
// the same-origin adapter (default-drive alias or drives/{drive}/{op} form).
// Deliberately narrower than the machine surface: no jobs, child keys,
// transfers/fetch/batch, selftest, activity, trash or relay endpoints.
var workspaceOpAliases = map[string]bool{
	"list":            true,
	"stat":            true,
	"tree":            true,
	"files":           true,
	"mkdir":           true,
	"delete":          true,
	"move":            true,
	"links":           true,
	"download-ticket": true,
}

// WorkspaceAPIPath reports whether p is one of the workspace API routes this
// handler serves. The portal hands exactly these paths to WorkspaceHandler
// and serves the /files pages and assets itself; anything else stays out of
// this surface (fail closed).
func WorkspaceAPIPath(p string) bool {
	if p == "/files/api/login" || p == "/files/api/workspaces" {
		return true
	}
	const wp = "/files/w/"
	if !strings.HasPrefix(p, wp) {
		return false
	}
	rest := strings.TrimPrefix(p, wp)
	id, api, ok := strings.Cut(rest, "/")
	return ok && validWorkspaceID(id) && (api == "api/logout" || api == "api/v1" || strings.HasPrefix(api, "api/v1/"))
}

// workspaceAPI implements http.Handler. All per-instance state lives here so
// server.go stays untouched.
type workspaceAPI struct {
	srv    *Server
	logins *wsLoginLimiter
	// capMu serialises the 16-workspace cap check with the row insert so
	// concurrent logins of one browser cannot exceed the limit.
	capMu sync.Mutex
}

// WorkspaceHandler returns the browser workspace API handler. Installed into
// the portal via SetWorkspaceHandler (fail closed when absent).
func (s *Server) WorkspaceHandler() http.Handler {
	return &workspaceAPI{srv: s, logins: newWSLoginLimiter()}
}

func (w *workspaceAPI) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// Sensitive surface: never cached, never referrer-leaked, never sniffed.
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	rw.Header().Set("X-Content-Type-Options", "nosniff")

	p := r.URL.Path
	switch {
	case p == "/files/api/login":
		w.requireMethod(rw, r, http.MethodPost, w.handleLogin)
	case p == "/files/api/workspaces":
		w.requireMethod(rw, r, http.MethodGet, w.handleList)
	case WorkspaceAPIPath(p):
		w.serveWorkspace(rw, r)
	default:
		writeWSError(rw, newAPIError(http.StatusNotFound, CodeNotFound, "not found"))
	}
}

func (w *workspaceAPI) requireMethod(rw http.ResponseWriter, r *http.Request, method string, h func(http.ResponseWriter, *http.Request)) {
	if r.Method != method {
		rw.Header().Set("Allow", method)
		writeWSError(rw, newAPIError(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method not allowed; allow: "+method))
		return
	}
	h(rw, r)
}

// serveWorkspace dispatches /files/w/<id>/api/logout and /api/v1/<suffix>.
func (w *workspaceAPI) serveWorkspace(rw http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/files/w/")
	id, api, _ := strings.Cut(rest, "/")
	switch {
	case api == "api/logout":
		w.requireMethod(rw, r, http.MethodPost, func(rw http.ResponseWriter, r *http.Request) {
			w.handleLogout(rw, r, id)
		})
	case api == "api/v1":
		w.handleV1(rw, r, id, "")
	case strings.HasPrefix(api, "api/v1/"):
		w.handleV1(rw, r, id, strings.TrimPrefix(api, "api/v1/"))
	default:
		writeWSError(rw, newAPIError(http.StatusNotFound, CodeNotFound, "not found"))
	}
}

// ---- request security (§4) ----

// checkRequestSecurity enforces the dedicated header and the same-origin
// rules on every workspace API request, including reads. X-C2A-Workspace: 1
// is always required. A present Origin must match the trusted effective
// scheme and the request host:port exactly (default ports normalised);
// "null", userinfo, path/query/fragment suffixes, repeated values and any
// cross-scheme (downgrade) origin are rejected. A present Sec-Fetch-Site
// must be same-origin or none. Absence of Origin is fine — the dedicated
// header and the session cookie still carry the request.
func (w *workspaceAPI) checkRequestSecurity(r *http.Request) *APIError {
	bad := func(msg string) *APIError {
		return newAPIError(http.StatusForbidden, CodeForbidden, msg)
	}
	if vals := r.Header.Values(workspaceHeaderName); len(vals) != 1 || vals[0] != "1" {
		return bad("missing or invalid " + workspaceHeaderName + " header")
	}
	if sites := r.Header.Values("Sec-Fetch-Site"); len(sites) > 0 {
		if len(sites) > 1 {
			return bad("repeated Sec-Fetch-Site header")
		}
		site := strings.ToLower(strings.TrimSpace(sites[0]))
		if site != "same-origin" && site != "none" {
			return bad("cross-site requests are not accepted")
		}
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		return bad("repeated Origin header")
	}
	if len(origins) == 1 && strings.TrimSpace(origins[0]) != "" {
		if e := w.checkOrigin(origins[0], r); e != nil {
			return e
		}
	}
	return nil
}

// checkOrigin validates one Origin value: absolute http/https with host:port
// only (no userinfo, path, query or fragment), never "null", scheme equal to
// the trusted effective scheme (rejects HTTPS downgrade) and host:port equal
// to the request's (scheme-default ports normalised).
func (w *workspaceAPI) checkOrigin(raw string, r *http.Request) *APIError {
	bad := func(msg string) *APIError {
		return newAPIError(http.StatusForbidden, CodeForbidden, msg)
	}
	if strings.EqualFold(strings.TrimSpace(raw), "null") {
		return bad("null Origin is not accepted")
	}
	o, err := url.Parse(raw)
	if err != nil {
		return bad("invalid Origin header")
	}
	if o.Scheme != "http" && o.Scheme != "https" {
		return bad("invalid Origin scheme")
	}
	if o.User != nil || o.Opaque != "" || (o.Path != "" && o.Path != "/") ||
		o.RawQuery != "" || o.Fragment != "" || o.Host == "" {
		return bad("invalid Origin header")
	}
	scheme := w.srv.wsScheme(r)
	if o.Scheme != scheme {
		return bad("origin scheme does not match this endpoint")
	}
	if !strings.EqualFold(originHostPort(o, scheme), requestHostPort(r.Host, scheme)) {
		return bad("cross-origin requests are not allowed")
	}
	return nil
}

// originHostPort normalises scheme-default ports away so "https://h:443"
// equals "https://h".
func originHostPort(u *url.URL, scheme string) string {
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		return u.Hostname()
	}
	return u.Hostname() + ":" + port
}

func requestHostPort(host, scheme string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return strings.Trim(host, "[]")
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		return h
	}
	return h + ":" + port
}

// wsScheme resolves the trusted effective scheme: TLS on this connection, or
// an explicitly configured https public_base_url origin. X-Forwarded-Proto
// is deliberately never consulted (untrusted).
func (s *Server) wsScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if v, ok := s.st.GetSetting("public_base_url"); ok {
		if u, err := url.Parse(strings.TrimSpace(v)); err == nil && u.Scheme == "https" && u.Host != "" {
			return "https"
		}
	}
	return "http"
}

func (s *Server) wsSecureCookies(r *http.Request) bool {
	return s.wsScheme(r) == "https"
}

// ---- cookie token handling ----

// filesCookieToken returns the single well-formed c2a_files cookie value.
// Missing, repeated or malformed cookies all report ok=false — a
// browser-provided value is never adopted unless it is exactly the shape
// this server mints.
func filesCookieToken(r *http.Request) (string, bool) {
	var found []string
	for _, c := range r.Cookies() {
		if c.Name == filesCookieName {
			found = append(found, c.Value)
		}
	}
	if len(found) != 1 || !validSessionToken(found[0]) {
		return "", false
	}
	return found[0], true
}

func validSessionToken(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == workspaceTokenBytes
}

func newSessionToken() (string, error) {
	buf := make([]byte, workspaceTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validWorkspaceID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func writeWSError(rw http.ResponseWriter, e *APIError) {
	writeJSON(rw, e.Status(), map[string]any{"error": e})
}

// storeFailed answers a storage failure with a fixed, non-sensitive client
// message — sqlite error text carries internal details such as the database
// path, and the generic upstream error text would misattribute the failure
// to OpenList. The error itself only reaches the server log (op name and
// error; never the request's key, cookie or body).
func (w *workspaceAPI) storeFailed(rw http.ResponseWriter, op string, err error) {
	w.srv.log.Warn("workspace storage failure", "op", op, "err", err)
	writeWSError(rw, newAPIError(http.StatusServiceUnavailable, CodeUpstreamError,
		"the workspace service is temporarily unavailable; try again"))
}

// ---- login ----

func (w *workspaceAPI) handleLogin(rw http.ResponseWriter, r *http.Request) {
	s := w.srv
	if e := w.checkRequestSecurity(r); e != nil {
		s.log.Warn("workspace login rejected", "remote", r.RemoteAddr, "path", r.URL.Path)
		writeWSError(rw, e)
		return
	}
	ip := clientIP(r)
	if !w.logins.allowed(ip) {
		wait := w.logins.retryAfter(ip)
		rw.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeWSError(rw, errRateLimited(wait))
		return
	}

	var body struct {
		APIKey      string `json:"api_key"`
		WorkspaceID string `json:"workspace_id"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, wsLoginBodyLimit))
	if err != nil {
		writeWSError(rw, errInvalidRequest("failed to read request body"))
		return
	}
	if err := json.Unmarshal(data, &body); err != nil || strings.TrimSpace(body.APIKey) == "" {
		writeWSError(rw, errInvalidRequest("request body must be JSON with an api_key field"))
		return
	}

	key, outcome := s.keys.Authenticate(strings.TrimSpace(body.APIKey))
	if outcome != auth.AuthOK || key == nil {
		w.logins.recordFailure(ip)
		w.auditLogin("", http.StatusUnauthorized, "invalid credentials")
		// Generic message: no hint whether the key exists, is revoked or expired.
		writeWSError(rw, newAPIError(http.StatusUnauthorized, CodeUnauthorized,
			"invalid api key or workspace"))
		return
	}

	// Resolve the entry. A presented workspace_id must already belong to this
	// key — another key can never take over an existing entry, and unknown
	// ids answer the same generic mismatch error.
	wsID := strings.TrimSpace(body.WorkspaceID)
	if wsID != "" {
		if !validWorkspaceID(wsID) {
			writeWSError(rw, errInvalidRequest("invalid workspace_id"))
			return
		}
		boundKey, err := s.st.FileWorkspaceKey(wsID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && boundKey != key.ID) {
			w.logins.recordFailure(ip)
			w.auditLogin("", http.StatusForbidden, "workspace/key mismatch")
			writeWSError(rw, newAPIError(http.StatusForbidden, CodeForbidden,
				"api_key and workspace_id do not match"))
			return
		}
		if err != nil {
			w.storeFailed(rw, "login_workspace_lookup", err)
			return
		}
	} else {
		id, err := s.st.EnsureFileWorkspace(key.ID)
		if err != nil {
			w.storeFailed(rw, "login_ensure_workspace", err)
			return
		}
		wsID = id
	}

	// Reuse the browser's valid random token — but only one this server
	// issued before (it already owns session rows). A missing, malformed or
	// unknown token is replaced by a fresh random one: client-chosen session
	// ids are never adopted, which defeats cookie-fixation logins.
	token, ok := filesCookieToken(r)
	if ok {
		known, err := s.st.ListWebSessions(sessionTokenHash(token), store.SessionKindFiles)
		if err != nil {
			w.storeFailed(rw, "login_known_token", err)
			return
		}
		if len(known) == 0 {
			ok = false
		}
	}
	if !ok {
		var err error
		token, err = newSessionToken()
		if err != nil {
			writeWSError(rw, errUpstream("session token generation failed"))
			return
		}
	}
	hash := sessionTokenHash(token)

	// Cap check + insert under one lock so concurrent logins of this browser
	// cannot push past the member limit. Existing members are untouched on
	// rejection.
	now := time.Now()
	exp := now.Add(workspaceTTL)
	w.capMu.Lock()
	rows, err := s.st.ListWebSessions(hash, store.SessionKindFiles)
	if err != nil {
		w.capMu.Unlock()
		w.storeFailed(rw, "login_cap_check", err)
		return
	}
	has := false
	valid := 0
	for _, row := range rows {
		if row.WorkspaceID == wsID {
			has = true
			continue
		}
		if now.Before(row.ExpiresAt) {
			valid++
		}
	}
	if !has && valid >= maxBrowserWorkspaces {
		w.capMu.Unlock()
		// Not a credential failure: do not feed the login limiter.
		writeWSError(rw, newAPIError(http.StatusForbidden, CodeForbidden,
			"this browser already holds the maximum number of workspace sessions"))
		return
	}
	credTag := s.keyCredTag(key.Hash())
	err = s.st.PutWebSession(store.WebSession{
		TokenHash:     hash,
		Kind:          store.SessionKindFiles,
		WorkspaceID:   wsID,
		SubjectID:     key.ID,
		CredentialTag: credTag,
		CreatedAt:     now,
		ExpiresAt:     exp,
	})
	w.capMu.Unlock()
	if err != nil {
		w.storeFailed(rw, "login_session_put", err)
		return
	}

	w.logins.reset(ip)
	w.auditLogin(key.ID, http.StatusOK, "")

	http.SetCookie(rw, &http.Cookie{
		Name:     filesCookieName,
		Value:    token,
		Path:     filesCookiePath,
		Expires:  exp,
		MaxAge:   int(workspaceTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.wsSecureCookies(r),
	})
	writeJSON(rw, http.StatusOK, map[string]any{
		"workspace_id": wsID,
		"url":          "/files/w/" + wsID + "/",
		"expires_at":   exp.UTC().Format(time.RFC3339),
	})
}

func (w *workspaceAPI) auditLogin(keyID string, status int, errMsg string) {
	// Never logs the presented key or the cookie (§4).
	_ = w.srv.st.InsertAudit(store.AuditEntry{
		KeyID:  keyID,
		Op:     "ws_login",
		Status: status,
		Error:  errMsg,
	})
}

// ---- workspace listing ----

// handleList answers GET /files/api/workspaces with the entries the current
// browser still owns. Every member is revalidated against the live key
// state; a failed member is dropped without touching the others.
func (w *workspaceAPI) handleList(rw http.ResponseWriter, r *http.Request) {
	s := w.srv
	if e := w.checkRequestSecurity(r); e != nil {
		writeWSError(rw, e)
		return
	}
	token, ok := filesCookieToken(r)
	if !ok {
		writeJSON(rw, http.StatusOK, map[string]any{"workspaces": []any{}})
		return
	}
	rows, err := s.st.ListWebSessions(sessionTokenHash(token), store.SessionKindFiles)
	if err != nil {
		// DB failure fails closed: no listing, no fallback.
		w.storeFailed(rw, "list_sessions", err)
		return
	}
	now := time.Now()
	out := []map[string]any{}
	for _, row := range rows {
		if !now.Before(row.ExpiresAt) {
			_ = s.st.DeleteWebSession(row.TokenHash, row.Kind, row.WorkspaceID)
			continue
		}
		if _, valid := w.validateMember(row, now); !valid {
			_ = s.st.DeleteWebSession(row.TokenHash, row.Kind, row.WorkspaceID)
			continue
		}
		out = append(out, map[string]any{
			"id":         row.WorkspaceID,
			"url":        "/files/w/" + row.WorkspaceID + "/",
			"expires_at": row.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(rw, http.StatusOK, map[string]any{"workspaces": out})
}

// validateMember re-checks one session row against current state: the entry
// still exists, is bound to the row's subject key, the key exists, is not
// revoked or expired, and its stored hash still matches the session's
// credential tag (rotation, revocation-cascade, deletion and expiry all
// invalidate exactly this member).
func (w *workspaceAPI) validateMember(row store.WebSession, now time.Time) (*auth.Key, bool) {
	s := w.srv
	if row.Kind != store.SessionKindFiles {
		return nil, false
	}
	boundKey, err := s.st.FileWorkspaceKey(row.WorkspaceID)
	if err != nil || boundKey != row.SubjectID {
		return nil, false
	}
	keyRow, err := s.keys.Get(row.SubjectID)
	if err != nil {
		return nil, false
	}
	if keyRow.Revoked || (keyRow.ExpiresAt != nil && !now.Before(*keyRow.ExpiresAt)) {
		return nil, false
	}
	if s.keyCredTag(keyRow.Hash) != row.CredentialTag {
		return nil, false
	}
	rk, ok := s.keys.Authenticator().ByID(row.SubjectID)
	if !ok {
		return nil, false
	}
	// The runtime snapshot that dispatch will use must carry the same
	// credential generation; a rotation between the store read and the
	// snapshot read must not let an old session ride a new key.
	if s.keyCredTag(rk.Hash()) != row.CredentialTag {
		return nil, false
	}
	return rk, true
}

// ---- logout ----

// handleLogout deletes only the presented browser's row for this entry.
// Other workspace members of the same cookie (and any admin session) stay;
// the cookie itself is kept because it may still carry other members.
func (w *workspaceAPI) handleLogout(rw http.ResponseWriter, r *http.Request, id string) {
	s := w.srv
	if e := w.checkRequestSecurity(r); e != nil {
		writeWSError(rw, e)
		return
	}
	if token, ok := filesCookieToken(r); ok {
		if err := s.st.DeleteWebSession(sessionTokenHash(token), store.SessionKindFiles, id); err != nil {
			w.storeFailed(rw, "logout_delete", err)
			return
		}
	}
	_ = s.st.InsertAudit(store.AuditEntry{Op: "ws_logout", Status: http.StatusOK})
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// ---- same-origin /v1 adapter ----

// workspacePathAllowed is the UI op whitelist (§2): me, the drive list and
// the directory/file operations plus links and download-ticket. Everything
// else the machine surface offers (jobs, child keys, transfers, fetch,
// batch, selftest, activity, trash, relay/gokapi, the entry index) answers
// 404 here — the cookie must never become a proxy for the full API.
func workspacePathAllowed(p string) bool {
	segs := splitPath(p) // ["v1", ...]
	if len(segs) < 2 {
		return false
	}
	switch {
	case segs[1] == "me":
		return len(segs) == 2
	case segs[1] == "drives":
		return len(segs) == 2 || (len(segs) == 4 && workspaceOpAliases[segs[3]])
	default:
		return len(segs) == 2 && workspaceOpAliases[segs[1]]
	}
}

// handleV1 adapts /files/w/<id>/api/v1/<suffix> onto the internal /v1
// dispatch with the session's freshly revalidated key. No plaintext key is
// injected anywhere: the runtime *auth.Key goes straight into dispatchV1.
func (w *workspaceAPI) handleV1(rw http.ResponseWriter, r *http.Request, id, suffix string) {
	s := w.srv
	rec := &respRecorder{ResponseWriter: rw}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "ws_api", start: time.Now()}
	dispatched := false
	defer func() {
		// dispatchV1 audits the data operation itself; only failures before
		// dispatch add their own row.
		if !dispatched {
			s.audit(c)
		}
	}()

	fail := func(e *APIError) {
		c.errMsg = e.Code + ": " + e.Message
		writeJSON(rec, e.status, map[string]any{"error": e})
	}

	if e := w.checkRequestSecurity(r); e != nil {
		c.errMsg = CodeForbidden + ": " + e.Message
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}
	// Hop guard first, exactly like /v1 ingress.
	r, hopErr := hopGuard(r)
	if hopErr != nil {
		fail(hopErr)
		return
	}
	// Surface whitelist before any session work: the adapter must not even
	// look up sessions for non-UI endpoints.
	if !workspacePathAllowed("/v1/" + suffix) {
		fail(errNotFoundRoute())
		return
	}

	token, ok := filesCookieToken(r)
	if !ok {
		fail(newAPIError(http.StatusUnauthorized, CodeUnauthorized,
			"workspace session missing or invalid"))
		return
	}
	hash := sessionTokenHash(token)
	sess, err := s.st.GetWebSession(hash, store.SessionKindFiles, id)
	if err == nil && !time.Now().Before(sess.ExpiresAt) {
		// Boundary-inclusive expiry.
		_ = s.st.DeleteWebSession(hash, store.SessionKindFiles, id)
		err = store.ErrNotFound
	}
	if err != nil {
		fail(newAPIError(http.StatusUnauthorized, CodeUnauthorized,
			"workspace session missing or invalid"))
		return
	}
	c.auditKeyID = sess.SubjectID

	rk, valid := w.validateMember(sess, time.Now())
	if !valid {
		// This member alone is dead; other workspaces of the cookie are
		// untouched. Drop the stale row so it stops counting against the
		// member cap.
		_ = s.st.DeleteWebSession(hash, store.SessionKindFiles, id)
		fail(newAPIError(http.StatusUnauthorized, CodeUnauthorized,
			"workspace session is no longer valid"))
		return
	}
	c.key = rk

	// The session's data operations consume the original key's own bucket.
	if allow, wait := s.limiter.allow(rk.ID, rk.Limits.RequestsPerMinute); !allow {
		fail(errRateLimited(wait))
		return
	}

	rewritten := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/v1/" + suffix
	rewritten.URL = &u
	dispatched = true
	s.dispatchV1(rec, rewritten, rk)
}

// ---- per-source login rate limit (§4) ----

const (
	wsLoginMaxFailures = 5
	wsLoginWindow      = time.Minute
)

// wsLoginLimiter counts failed workspace logins per client IP in a sliding
// window (mirrors the portal's limiter; a success clears the count).
type wsLoginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newWSLoginLimiter() *wsLoginLimiter {
	return &wsLoginLimiter{failures: map[string][]time.Time{}}
}

func (l *wsLoginLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.live(ip, time.Now())) < wsLoginMaxFailures
}

func (l *wsLoginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip] = append(l.live(ip, time.Now()), time.Now())
}

func (l *wsLoginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

func (l *wsLoginLimiter) retryAfter(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	live := l.live(ip, time.Now())
	if len(live) < wsLoginMaxFailures {
		return time.Second
	}
	wait := wsLoginWindow - time.Since(live[0])
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

func (l *wsLoginLimiter) live(ip string, now time.Time) []time.Time {
	hits := l.failures[ip]
	live := hits[:0]
	for _, t := range hits {
		if now.Sub(t) < wsLoginWindow {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = live
	}
	return live
}

// clientIP extracts the remote address host for rate-limit keying; the raw
// string when unparsable so no client escapes the limiter. Proxy headers are
// never trusted here.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
