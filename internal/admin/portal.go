package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/reconcile"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

//go:embed web
var webFS embed.FS

const (
	sessionCookieName = "session"
	// maxAdminRequestBody bounds portal JSON request bodies.
	maxAdminRequestBody = 1 << 20
)

// Portal serves the management UI and JSON API under /admin (UPGRADE §8.2)
// and the public file workspace / relay shells.
type Portal struct {
	cfg      config.Admin
	registry *gateway.AccountRegistry
	client   *openlist.Client
	st       *store.Store
	keys     *keymgr.Manager
	sessions sessionBackend
	// sessSecret is the persistent random HMAC secret behind the session
	// credential tags; empty when session storage is unavailable, in which
	// case sessions fail closed.
	sessSecret []byte
	logins     *loginLimiter
	log        *slog.Logger

	reconciler *reconcile.Reconciler
	// guard performs the fresh per-request storage verification the reauth
	// approval needs (main wires the same guard the data plane uses).
	guard *storageid.Guard
	// maintenanceFunc runs every background maintenance loop once (relay
	// retention sweep, trash purge, log pruning); installed by main from the
	// agentapi server. POST /admin/api/maintenance triggers it (§8.2).
	maintenanceFunc func(ctx context.Context) map[string]int

	loginPage []byte
	appPage   []byte
	relayPage []byte
	homePage  []byte

	// workspaceHandler is the /files API handler installed by main
	// (agentapi WorkspaceHandler, DESIGN §5.1). While it is unset every
	// /files API request fails closed instead of falling through.
	workspaceMu     sync.RWMutex
	workspaceHandle http.Handler

	storageMu   sync.Mutex
	storageAt   time.Time
	storageSnap []openlist.StorageInfo

	// connectMu guards the cached login-module probe below (the result of
	// GET /api/admin/connect/platforms); like cachedStorages, the probe itself
	// runs outside the lock.
	connectMu   sync.Mutex
	connectAt   time.Time
	connectInfo openlist.ConnectInfo
	connectOK   bool   // the last probe found the module
	connectErr  string // the last probe's failure text ("" when fine or absent)
	connectHas  bool   // a probe result is cached

	// claimMu serialises adopt and rebind so two concurrent portal calls can
	// never both claim the same OpenList storage (IMPLEMENTATION §刷新与纳管).
	claimMu sync.Mutex
}

func NewPortal(cfg *config.Admin, registry *gateway.AccountRegistry, client *openlist.Client,
	st *store.Store, keys *keymgr.Manager, logger *slog.Logger) *Portal {
	if logger == nil {
		logger = slog.Default()
	}
	ttl := cfg.SessionTTL.Standard()
	if ttl <= 0 {
		ttl = config.DefaultSessionTTL
	}
	p := &Portal{
		cfg:      *cfg,
		registry: registry,
		client:   client,
		st:       st,
		keys:     keys,
		logins:   newLoginLimiter(),
		log:      logger,
	}
	// Admin sessions are persistent (DESIGN §3): the fixed configured TTL,
	// rows in the shared web_sessions table, credential tags under a
	// persistent random secret. A storage failure at startup disables
	// sessions entirely instead of falling back to memory.
	secret, err := resolveAdminSessionSecret(st)
	if err != nil {
		logger.Error("admin session storage unavailable; admin logins fail closed until restart", "err", err)
		p.sessions = disabledSessions{reason: err}
	} else {
		p.sessSecret = secret
		p.sessions = newSQLiteSessions(st, ttl, logger)
	}
	p.loginPage, _ = webFS.ReadFile("web/login.html")
	p.appPage, _ = webFS.ReadFile("web/app.html")
	p.relayPage, _ = webFS.ReadFile("web/relay.html")
	p.homePage, _ = webFS.ReadFile("web/home.html")
	return p
}

// SetWorkspaceHandler installs the /files API handler (agentapi's
// WorkspaceHandler, DESIGN §5.1). Until one is installed, every /files API
// request fails closed; the static public shells never need it.
func (p *Portal) SetWorkspaceHandler(h http.Handler) {
	p.workspaceMu.Lock()
	defer p.workspaceMu.Unlock()
	p.workspaceHandle = h
}

// workspaceHandler returns the installed /files API handler, if any.
func (p *Portal) workspaceHandler() http.Handler {
	p.workspaceMu.RLock()
	defer p.workspaceMu.RUnlock()
	return p.workspaceHandle
}

// sessionCredTag derives the credential tag of the current admin password;
// empty when session storage is unavailable (no session can exist then).
func (p *Portal) sessionCredTag() string {
	if len(p.sessSecret) == 0 {
		return ""
	}
	return adminCredentialTag(p.sessSecret, p.cfg.Password)
}

// cookieSecure decides the Secure attribute of session cookies: direct TLS,
// or an explicitly configured https public_base_url (the only trusted
// signals — X-Forwarded-Proto and the OpenList public URL are never
// trusted, DESIGN §4).
func (p *Portal) cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if p.st == nil {
		return false
	}
	v, ok := p.st.GetSetting("public_base_url")
	if !ok {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// StartCleanup launches the session expiry goroutines.
func (p *Portal) StartCleanup(ctx context.Context) {
	p.sessions.StartCleanup(ctx)
}

// SetReconciler installs the reconcile loop handle (Phase 1c linkage).
func (p *Portal) SetReconciler(r *reconcile.Reconciler) { p.reconciler = r }

// SetStorageGuard installs the storage verifier shared with the data plane
// (main passes the same guard the gateway and agentapi use).
func (p *Portal) SetStorageGuard(g *storageid.Guard) { p.guard = g }

// storageGuard returns the wired guard, or builds a store-backed one from the
// portal's own client/store/registry exactly the way main does. The reauth
// approval check is never skipped, so a guard is always available.
func (p *Portal) storageGuard() *storageid.Guard {
	if p.guard != nil {
		return p.guard
	}
	g := storageid.NewGuard(p.client,
		func(driveID string) (storageid.Drive, bool) {
			d, err := p.st.GetDrive(driveID)
			if err != nil {
				return storageid.Drive{}, false
			}
			return storageid.Drive{
				StorageID: d.StorageID, MountPath: d.MountPath,
				Static: d.Static, Enabled: d.Enabled, State: d.State,
			}, true
		},
		func(driveID string) (string, bool) {
			a, ok := p.registry.Get(driveID)
			return a.MountPath, ok
		}, p.log)
	if p.reconciler != nil {
		g.SetReconciler(func(ctx context.Context) { _ = p.reconciler.Reconcile(ctx) })
	}
	return g
}

// SetMaintenanceFunc installs the run-all-maintenance-now hook (Phase 1d).
func (p *Portal) SetMaintenanceFunc(f func(ctx context.Context) map[string]int) {
	p.maintenanceFunc = f
}

// invalidateStorageCache drops the 30s storage snapshot so the next reader
// sees fresh OpenList state (after writes and reconcile passes).
func (p *Portal) InvalidateStorageCache() {
	p.storageMu.Lock()
	p.storageAt = time.Time{}
	p.storageMu.Unlock()
}

// storeSnapshotCache replaces the portal storage cache with a snapshot that
// was just read (and reconciled on) — later readers see the same data without
// another OpenList pass.
func (p *Portal) storeSnapshotCache(snap []openlist.StorageInfo) {
	p.storageMu.Lock()
	p.storageSnap = snap
	p.storageAt = time.Now()
	p.storageMu.Unlock()
}

// Wrap serves the public navigation page, the file workspace shells and the
// relay page alongside /admin. File operations require a workspace session
// (or API key on the machine data plane); /admin keeps its session login.
func (p *Portal) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			p.serveHome(w, r)
		case r.URL.Path == "/files" || strings.HasPrefix(r.URL.Path, "/files/"):
			p.serveFiles(w, r)
		case r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/"):
			p.serve(w, r)
		case r.URL.Path == "/relay" || strings.HasPrefix(r.URL.Path, "/relay/"):
			p.serveRelay(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// homeCSP is the content security policy of the public navigation page: a
// static same-origin document that only loads its own assets and links to
// the file workspace and admin portal.
const homeCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// serveHome serves the public navigation page (GET only, no credentials, no
// cookies — DESIGN §2). The page file is frontend-owned; until web/home.html
// ships the route answers 404.
func (p *Portal) serveHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", homeCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if len(p.homePage) == 0 {
		respondError(w, http.StatusNotFound, "home page not installed")
		return
	}
	serveHTML(w, http.StatusOK, p.homePage)
}

// relayCSP is the content security policy of the public relay page: a static
// same-origin document that only loads its own assets and calls this
// gateway's API with the user-pasted key.
const relayCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// serveRelay serves the public, restricted relay page (GET only). The page
// file is frontend-owned; until web/relay.html ships the route answers 404.
func (p *Portal) serveRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", relayCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	switch {
	case r.URL.Path == "/relay" || r.URL.Path == "/relay/":
		if len(p.relayPage) == 0 {
			respondError(w, http.StatusNotFound, "relay page not installed")
			return
		}
		serveHTML(w, http.StatusOK, p.relayPage)
	case strings.HasPrefix(r.URL.Path, "/relay/assets/"):
		name := strings.TrimPrefix(r.URL.Path, "/relay/assets/")
		if strings.Contains(name, "..") || strings.Contains(name, "\x00") || strings.Contains(name, "/") {
			respondError(w, http.StatusBadRequest, "invalid asset path")
			return
		}
		data, err := webFS.ReadFile("web/assets/" + name)
		if err != nil {
			respondError(w, http.StatusNotFound, "asset not found")
			return
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		respondError(w, http.StatusNotFound, "not found")
	}
}

func (p *Portal) serve(w http.ResponseWriter, r *http.Request) {
	pathName := r.URL.Path
	// v0.3.1 hardening (UPGRADE §5.3): mutating /admin/api/* requests are
	// checked for cross-origin and, when carrying a body, for a JSON content
	// type. Applied before authentication.
	if strings.HasPrefix(pathName, "/admin/api/") && !p.guardAdminAPI(w, r) {
		return
	}
	switch {
	case pathName == "/admin":
		http.Redirect(w, r, "/admin/", http.StatusFound)

	case pathName == "/admin/connect-return":
		p.serveConnectReturn(w, r)

	case pathName == "/admin/login":
		if r.Method != http.MethodGet {
			respondMethodNotAllowed(w, http.MethodGet)
			return
		}
		// An already-valid session goes straight to the portal (DESIGN §5.2).
		if token := p.sessionToken(r); p.sessions.Validate(token, p.sessionCredTag()) {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		serveHTML(w, http.StatusOK, p.loginPage)

	case pathName == "/admin/api/login":
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleLogin(w, r)

	case strings.HasPrefix(pathName, "/admin/assets/"):
		p.serveAsset(w, r, strings.TrimPrefix(pathName, "/admin/assets/"))

	default:
		token := p.sessionToken(r)
		if !p.sessions.Validate(token, p.sessionCredTag()) {
			if strings.HasPrefix(pathName, "/admin/api/") {
				respondError(w, http.StatusUnauthorized, "unauthorized")
			} else {
				http.Redirect(w, r, "/admin/login", http.StatusFound)
			}
			return
		}
		p.serveAuthorized(w, r, token)
	}
}

// serveAsset serves a file from the embedded web/assets tree with the
// correct content type (frontend agent's SPA assets, §8.1).
func (p *Portal) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		respondMethodNotAllowed(w, http.MethodGet)
		return
	}
	name = strings.TrimPrefix(name, "/")
	if strings.Contains(name, "..") || strings.Contains(name, "\x00") {
		respondError(w, http.StatusBadRequest, "invalid asset path")
		return
	}
	data, err := webFS.ReadFile("web/assets/" + name)
	if err != nil {
		respondError(w, http.StatusNotFound, "asset not found")
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (p *Portal) serveAuthorized(w http.ResponseWriter, r *http.Request, token string) {
	pathName := r.URL.Path
	switch {
	// ---- pages ----
	case pathName == "/admin/" || (!strings.HasPrefix(pathName, "/admin/api/") && !strings.HasPrefix(pathName, "/admin/assets/")):
		if r.Method != http.MethodGet {
			respondMethodNotAllowed(w, http.MethodGet)
			return
		}
		// The old v0.3 accounts page moved into the SPA.
		if pathName == "/admin/accounts.html" {
			http.Redirect(w, r, "/admin/#/drives", http.StatusFound)
			return
		}
		if len(p.appPage) == 0 {
			// The SPA shell is provided by the frontend; serve 404 until it
			// is present in the embed.
			respondError(w, http.StatusNotFound, "admin SPA not installed")
			return
		}
		serveHTML(w, http.StatusOK, p.appPage)

	case pathName == "/admin/api/logout":
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleLogout(w, r, token)

	case pathName == "/admin/api/connect/handoff":
		if r.Method != http.MethodPost {
			respondMethodNotAllowed(w, http.MethodPost)
			return
		}
		p.handleConnectHandoff(w, r)

	// ---- overview ----
	case pathName == "/admin/api/overview" && r.Method == http.MethodGet:
		p.handleOverview(w, r)

	// ---- drives (+ legacy accounts alias) ----
	// GET only: drive creation lives in the OpenList admin UI now; POST on
	// either path falls through to the 404 below (IMPLEMENTATION §删除的部分).
	case pathName == "/admin/api/drives" && r.Method == http.MethodGet:
		p.handleListDrives(w, r)

	case pathName == "/admin/api/accounts" && r.Method == http.MethodGet:
		p.handleListAccounts(w, r)

	case strings.HasPrefix(pathName, "/admin/api/drives/"), strings.HasPrefix(pathName, "/admin/api/accounts/"):
		p.serveDriveItem(w, r, pathName)

	// ---- keys ----
	case pathName == "/admin/api/keys":
		switch r.Method {
		case http.MethodGet:
			p.handleListKeys(w, r)
		case http.MethodPost:
			p.handleCreateKey(w, r)
		default:
			respondMethodNotAllowed(w, http.MethodGet+", "+http.MethodPost)
		}

	case strings.HasPrefix(pathName, "/admin/api/keys/"):
		p.serveKeyItem(w, r, strings.TrimPrefix(pathName, "/admin/api/keys/"))

	// ---- audit ----
	case pathName == "/admin/api/audit" && r.Method == http.MethodGet:
		p.handleAudit(w, r)

	// ---- maintenance (Phase 1d, §8.2): run every background loop once ----
	case pathName == "/admin/api/maintenance" && r.Method == http.MethodPost:
		p.handleMaintenance(w, r)

	// ---- linkage events (Phase 1c, §8.2) ----
	case pathName == "/admin/api/events" && r.Method == http.MethodGet:
		p.handleEvents(w, r)

	// ---- settings ----
	case pathName == "/admin/api/settings":
		switch r.Method {
		case http.MethodGet:
			p.handleGetSettings(w, r)
		case http.MethodPut:
			p.handlePutSettings(w, r)
		default:
			respondMethodNotAllowed(w, http.MethodGet+", "+http.MethodPut)
		}

	default:
		respondError(w, http.StatusNotFound, "not found")
	}
}

func (p *Portal) sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// handleMaintenance runs every background maintenance loop once (Phase 1d:
// relay retention sweep + abandoned chunk purge, trash purge, audit and
// idempotency pruning) and reports what happened. This is the manual/admin
// trigger of the periodic loops — also the documented test hook for retention
// acceptance.
func (p *Portal) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	if p.maintenanceFunc == nil {
		respondError(w, http.StatusServiceUnavailable, "maintenance runner not installed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	report := p.maintenanceFunc(ctx)
	if report == nil {
		report = map[string]int{}
	}
	respondData(w, map[string]any{"ran": true, "report": report})
}

func (p *Portal) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !p.logins.allowed(ip) {
		wait := p.logins.retryAfter(ip)
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		p.log.Warn("login rate limit hit", "remote", r.RemoteAddr)
		respondError(w, http.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		respondError(w, http.StatusBadRequest, "request body must be JSON with a password field")
		return
	}
	if body.Password == "" ||
		subtle.ConstantTimeCompare([]byte(body.Password), []byte(p.cfg.Password)) != 1 {
		p.logins.recordFailure(ip)
		p.log.Warn("portal login failed", "remote", r.RemoteAddr)
		respondError(w, http.StatusUnauthorized, "invalid password")
		return
	}
	p.logins.reset(ip)
	token, err := p.sessions.Create(p.sessionCredTag())
	if err != nil {
		p.log.Error("session create failed", "err", err)
		respondError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/admin",
		MaxAge:   int(p.sessionTTL().Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   p.cookieSecure(r),
	})
	respondJSON(w, http.StatusOK, map[string]any{"code": http.StatusOK, "message": "success"})
}

// sessionTTL is the fixed admin session lifetime: the configured
// admin.session_ttl (default 24h), never extended (DESIGN §3).
func (p *Portal) sessionTTL() time.Duration {
	if ttl := p.cfg.SessionTTL.Standard(); ttl > 0 {
		return ttl
	}
	return config.DefaultSessionTTL
}

// handleLogout revokes the persistent session. A storage failure is reported
// as a server error and the cookie is kept: claiming success would pretend a
// revocation that never happened (DESIGN §3). Without a cookie there is
// nothing to revoke — clearing is honest.
func (p *Portal) handleLogout(w http.ResponseWriter, r *http.Request, token string) {
	if token != "" {
		if err := p.sessions.Delete(token); err != nil {
			p.log.Error("admin logout failed", "err", err)
			respondError(w, http.StatusInternalServerError, "logout failed: the session could not be revoked, try again")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   p.cookieSecure(r),
	})
	respondJSON(w, http.StatusOK, map[string]any{"code": http.StatusOK, "message": "success"})
}

func serveHTML(w http.ResponseWriter, status int, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(page)
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminRequestBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(out)
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]any{"code": status, "message": msg})
}

// respondData writes the success envelope {code,message,data}.
func respondData(w http.ResponseWriter, data any) {
	respondJSON(w, http.StatusOK, map[string]any{"code": http.StatusOK, "message": "success", "data": data})
}

func respondMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	respondError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// cachedStorages merges the OpenList storage list with a 30s cache.
func (p *Portal) cachedStorages(ctx context.Context) ([]openlist.StorageInfo, error) {
	p.storageMu.Lock()
	defer p.storageMu.Unlock()
	if time.Since(p.storageAt) < 30*time.Second && p.storageSnap != nil {
		return p.storageSnap, nil
	}
	list, err := p.client.ListStorages(ctx)
	if err != nil {
		if p.storageSnap != nil {
			return p.storageSnap, nil
		}
		return nil, err
	}
	p.storageSnap = list
	p.storageAt = time.Now()
	return list, nil
}

const (
	// connectProbeTTL is how long a successful login-module probe is reused;
	// a failed one only connectProbeFailureTTL, so a recovering OpenList is
	// picked up quickly without probing on every portal request.
	connectProbeTTL        = 60 * time.Second
	connectProbeFailureTTL = 15 * time.Second
)

// connectState probes the patched OpenList's login module
// (GET /api/admin/connect/platforms, openlist-patch docs §平台与应用配置) and
// caches the result: the platform list when the module answered, and the
// failure text of a real probe error. An unpatched OpenList
// (openlist.ErrConnectUnavailable) is a normal "absent" state, never an error.
// force re-probes past the TTL (sync refreshes it once per call). The probe can
// only fail itself — callers keep serving either way.
func (p *Portal) connectState(ctx context.Context, force bool) (openlist.ConnectInfo, bool, string) {
	p.connectMu.Lock()
	if !force && p.connectHas && time.Since(p.connectAt) < p.connectCacheTTL() {
		info, ok, errText := p.connectInfo, p.connectOK, p.connectErr
		p.connectMu.Unlock()
		return info, ok, errText
	}
	p.connectMu.Unlock()

	info, err := p.client.ConnectInfo(ctx)

	p.connectMu.Lock()
	defer p.connectMu.Unlock()
	p.connectAt, p.connectHas = time.Now(), true
	if err == nil {
		p.connectInfo, p.connectOK, p.connectErr = info, true, ""
		return info, true, ""
	}
	p.connectInfo, p.connectOK = openlist.ConnectInfo{}, false
	if errors.Is(err, openlist.ErrConnectUnavailable) {
		p.connectErr = ""
		return openlist.ConnectInfo{}, false, ""
	}
	p.connectErr = err.Error()
	return openlist.ConnectInfo{}, false, p.connectErr
}

// connectCacheTTL is the validity of the cached probe: a failure is retried
// sooner than a success.
func (p *Portal) connectCacheTTL() time.Duration {
	if p.connectErr != "" {
		return connectProbeFailureTTL
	}
	return connectProbeTTL
}

// connectSnapshot returns the last probe's module state without probing. Row
// builders read it, so surfaces that never probe (adopt/rebind) still link when
// a recent probe exists — a missing link there is cosmetic, the native editor
// link still works.
func (p *Portal) connectSnapshot() (openlist.ConnectInfo, bool) {
	p.connectMu.Lock()
	defer p.connectMu.Unlock()
	return p.connectInfo, p.connectOK
}
