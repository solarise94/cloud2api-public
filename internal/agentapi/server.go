package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// idempotencyTTL bounds how long a stored response is replayed (§4.10.2).
const idempotencyTTL = 24 * time.Hour

// Server is the agent data plane (UPGRADE §4).
type Server struct {
	keys     *keymgr.Manager
	st       *store.Store
	client   *openlist.Client
	registry *gateway.AccountRegistry
	log      *slog.Logger

	limiter     *rateLimiter
	healthCache *healthCache
	linkSecret  []byte
	jobman      *jobManager

	// guard verifies each drive's backing OpenList storage (fresh, per
	// request) before any file access is made (IMPLEMENTATION
	// §存储被删除与替代存储).
	guard *storageid.Guard

	// reconcileNow runs one synchronous reconcile pass (Phase 1c, §3.7.4);
	// installed by main from the reconcile.Reconciler.
	reconcileNow func(context.Context)

	searchMu    sync.Mutex
	searchProbe searchState
}

// New builds the agent API server.
func New(keys *keymgr.Manager, st *store.Store, client *openlist.Client, registry *gateway.AccountRegistry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	guard := storageid.NewGuard(client, storeDriveLookup(st), registryMount(registry), logger)
	return &Server{
		keys:        keys,
		st:          st,
		client:      client,
		registry:    registry,
		log:         logger,
		limiter:     newRateLimiter(),
		healthCache: newHealthCache(client),
		linkSecret:  resolveLinkSecret(st),
		jobman:      newJobManager(),
		guard:       guard,
	}
}

// storeDriveLookup adapts the store to the guard's identity lookup.
func storeDriveLookup(st *store.Store) func(string) (storageid.Drive, bool) {
	return func(driveID string) (storageid.Drive, bool) {
		d, err := st.GetDrive(driveID)
		if err != nil {
			return storageid.Drive{}, false
		}
		return storageid.Drive{
			StorageID: d.StorageID, MountPath: d.MountPath,
			Static: d.Static, Enabled: d.Enabled, State: d.State,
		}, true
	}
}

// registryMount adapts the account registry to the guard's mount lookup (the
// mount the data plane will actually address, following reconcile renames).
func registryMount(reg *gateway.AccountRegistry) func(string) (string, bool) {
	return func(driveID string) (string, bool) {
		a, ok := reg.Get(driveID)
		return a.MountPath, ok
	}
}

// SetReconciler installs the reconcile callback used when a drive's backing
// storage disappears or its mount changes in OpenList (Phase 1c, §3.7.4).
// The same hook feeds the storage guard.
func (s *Server) SetReconciler(f func(context.Context)) {
	s.reconcileNow = f
	s.guard.SetReconciler(f)
}

// SetStorageGuard replaces the storage guard (main installs the shared,
// store-backed one so /v1 and the legacy gateway verify identically).
func (s *Server) SetStorageGuard(g *storageid.Guard) {
	if g != nil {
		if s.reconcileNow != nil {
			g.SetReconciler(s.reconcileNow)
		}
		s.guard = g
	}
}

// InvalidateHealth drops the 30s health snapshot (called after reconcile
// passes so the data plane sees fresh storage state immediately).
func (s *Server) InvalidateHealth() { s.healthCache.invalidate() }

// Handler returns the agent API HTTP handler. Everything not under its own
// prefixes falls through to next.
func (s *Server) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/healthz":
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		case p == "/readyz":
			s.handleReadyz(w, r)
		case p == "/v1" || strings.HasPrefix(p, "/v1/"):
			s.routeV1(w, r)
		case strings.HasPrefix(p, "/l/"):
			s.handleLinkDownload(w, r, strings.TrimPrefix(p, "/l/"))

		// UI-only large-download ticket redemption. Deliberately not part of
		// the documented/basic surface (routeTable); the token is the whole
		// path, so no query string ever carries it into an access log.
		case strings.HasPrefix(p, "/download/"):
			s.handleTicketDownload(w, r, strings.TrimPrefix(p, "/download/"))

		// ---- Gokapi compatibility layer (UPGRADE §4.7, Phase 1d) ----
		case isGokapiAPIPath(p):
			s.routeGokapi(w, r)
		case p == "/downloadFile":
			s.handleGokapiDownloadFile(w, r)
		case p == "/d":
			s.handleGokapiDownloadPage(w, r)
		case strings.HasPrefix(p, "/h/"):
			s.handleGokapiHotlink(w, r, strings.TrimPrefix(p, "/h/"))

		case p == "/custom/agent-instructions.txt":
			s.serveText(w, r, "text/plain; charset=utf-8", s.renderInstructions(r))
		case p == "/custom/gokapi-agent-instructions.txt":
			s.serveText(w, r, "text/plain; charset=utf-8", s.renderGokapiInstructions(r))
		case p == "/custom/c2a.py":
			s.serveText(w, r, "text/x-python; charset=utf-8", c2aPySource)
		case p == "/custom/SKILL.md":
			s.serveText(w, r, "text/markdown; charset=utf-8", renderSKILL(s.baseURL(r), s.policyText()))
		case p == "/apidocumentation/openapi.json":
			s.handleOpenAPI(w, r)
		case p == "/apidocumentation" || p == "/apidocumentation/":
			s.serveText(w, r, "text/html; charset=utf-8", apiDocsIndexHTML(s.baseURL(r)))
		default:
			if next == nil {
				writeJSON(w, http.StatusNotFound, errNotFoundRoute())
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

// handleReadyz reports liveness of the OpenList dependency: reachable (public
// settings answer) and token_ok (an admin-API call with the configured token
// succeeds). An invalid admin token fails readiness (§3.7.6).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	reachable := false
	if _, err := s.client.GetPublicSettings(ctx); err == nil {
		reachable = true
	}
	if _, err := s.client.ListStorages(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "error": err.Error(),
			"openlist_reachable": reachable, "token_ok": false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "openlist_reachable": reachable, "token_ok": true})
}

func (s *Server) serveText(w http.ResponseWriter, r *http.Request, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// ---- authentication ----

// extractKey pulls the presented key from the apikey header (preferred) or
// Authorization: Bearer. The query string is never consulted (§4.1).
func extractKey(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("apikey")); k != "" {
		return k
	}
	h := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(h) > len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		return strings.TrimSpace(h[len(scheme):])
	}
	return ""
}

// respRecorder captures status and body for audit and idempotency replay.
type respRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	buf    *bytes.Buffer // non-nil while the response is being captured
}

func (rr *respRecorder) WriteHeader(code int) {
	if rr.status == 0 {
		rr.status = code
	}
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *respRecorder) Write(p []byte) (int, error) {
	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	if rr.buf != nil {
		rr.buf.Write(p)
	}
	n, err := rr.ResponseWriter.Write(p)
	rr.bytes += int64(n)
	return n, err
}

func (rr *respRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// call carries one request's context through the handlers.
type call struct {
	w          *respRecorder
	r          *http.Request
	key        *auth.Key
	base       string
	auditKeyID string // key id for unauthenticated flows (/l/ links)
	op         string
	drive      string
	root       string // chroot root of the key on drive ("" until resolved)
	logical    string // agent-visible path for the audit row
	bytes      int64  // transferred payload bytes (upload/download)
	errMsg     string
	start      time.Time
}

func (c *call) setPath(p string)  { c.logical = p }
func (c *call) setDrive(d string) { c.drive = d }

// fail writes a typed error and remembers it for the audit row.
func (c *call) fail(e *APIError) {
	c.errMsg = e.Code + ": " + e.Message
	e.withDocs(c.base)
	if e.Code == CodeRateLimited && e.RetryAfter > 0 {
		c.w.Header().Set("Retry-After", fmt.Sprintf("%d", e.RetryAfter))
	}
	writeJSON(c.w, e.status, map[string]any{"error": e})
}

func (c *call) ok(v any) { writeJSON(c.w, http.StatusOK, v) }

// ---- /v1 dispatch ----

func (s *Server) routeV1(w http.ResponseWriter, r *http.Request) {
	rec := &respRecorder{ResponseWriter: w}
	// Relay hop guard first: a malformed or over-limit chain is a cheap,
	// key-independent rejection.
	r, hopErr := hopGuard(r)
	if hopErr != nil {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "hop", start: time.Now()}
		c.fail(hopErr)
		s.audit(c)
		return
	}
	presented := extractKey(r)
	if presented == "" {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "auth", start: time.Now()}
		c.fail(errUnauthorized(""))
		s.audit(c)
		return
	}
	key, outcome := s.keys.Authenticate(presented)
	if outcome != auth.AuthOK || key == nil {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "auth", start: time.Now()}
		switch outcome {
		case auth.AuthRevoked:
			c.fail(errUnauthorized("API key has been revoked"))
		case auth.AuthExpired:
			c.fail(errUnauthorized("API key has expired"))
		default:
			c.fail(errUnauthorized(""))
		}
		s.audit(c)
		return
	}

	// Per-key rate limit (token bucket, §5.2).
	if ok, wait := s.limiter.allow(key.ID, key.Limits.RequestsPerMinute); !ok {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "ratelimit", key: key, start: time.Now()}
		c.fail(errRateLimited(wait))
		s.audit(c)
		return
	}

	s.dispatchV1(rec, r, key)
}

func (s *Server) dispatchV1(rec *respRecorder, r *http.Request, key *auth.Key) {
	c := &call{w: rec, r: r, key: key, base: s.baseURL(r), start: time.Now()}
	defer func() { s.audit(c) }()

	segs := splitPath(r.URL.Path)
	switch {
	case len(segs) == 1 && r.Method == http.MethodGet:
		c.op = "index"
		s.handleIndex(c)

	case len(segs) == 2 && segs[1] == "me" && r.Method == http.MethodGet:
		c.op = "me"
		s.handleMe(c)

	case len(segs) == 2 && segs[1] == "drives" && r.Method == http.MethodGet:
		c.op = "drives"
		s.handleDrives(c)

	case len(segs) >= 2 && segs[1] == "trash":
		s.runTrash(c, segs)

	case len(segs) == 2 && segs[1] == "links" && r.Method == http.MethodPost:
		c.op = "link_create"
		s.withIdempotency(c, true, func(c *call) { s.handleLinkCreate(c) })

	// ---- Phase 1b: agent autonomy (UPGRADE §4.10) ----

	case len(segs) == 2 && segs[1] == "selftest" && r.Method == http.MethodPost:
		c.op = "selftest"
		s.handleSelftest(c)

	case len(segs) == 2 && segs[1] == "transfers" && r.Method == http.MethodPost:
		c.op = "transfer_create"
		s.withIdempotency(c, true, func(c *call) { s.handleTransferCreate(c) })

	case len(segs) == 2 && segs[1] == "fetch" && r.Method == http.MethodPost:
		c.op = "fetch_create"
		s.withIdempotency(c, true, func(c *call) { s.handleFetchCreate(c) })

	case len(segs) == 2 && segs[1] == "batch" && r.Method == http.MethodPost:
		c.op = "batch"
		s.withIdempotency(c, true, func(c *call) { s.handleBatch(c) })

	case len(segs) == 2 && segs[1] == "jobs" && r.Method == http.MethodGet:
		c.op = "jobs_list"
		s.handleJobsList(c)

	case len(segs) >= 3 && segs[1] == "jobs":
		s.runJobItem(c, strings.Join(segs[2:], "/"))

	case len(segs) == 2 && segs[1] == "keys":
		switch r.Method {
		case http.MethodGet:
			c.op = "keys_list"
			s.handleChildKeysList(c)
		case http.MethodPost:
			c.op = "key_create"
			s.withIdempotency(c, true, func(c *call) { s.handleChildKeyCreate(c) })
		default:
			s.methodNotAllowed(c, http.MethodGet+", "+http.MethodPost)
		}

	case len(segs) == 3 && segs[1] == "keys" && r.Method == http.MethodDelete:
		c.op = "key_delete"
		s.handleChildKeyDelete(c, segs[2])

	case len(segs) == 2 && segs[1] == "activity" && r.Method == http.MethodGet:
		c.op = "activity"
		s.handleActivity(c)

	case len(segs) == 4 && segs[1] == "drives":
		s.runDriveOp(c, segs[2], segs[3])

	case len(segs) == 2:
		s.runDriveOp(c, "", segs[1])

	default:
		c.fail(errNotFoundRoute())
	}
}

// splitPath turns "/v1/a/b" into ["v1","a","b"].
func splitPath(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// runTrash routes GET /v1/trash, POST /v1/trash/restore, POST /v1/trash/purge.
func (s *Server) runTrash(c *call, segs []string) {
	switch {
	case len(segs) == 2 && c.r.Method == http.MethodGet:
		c.op = "trash_list"
		s.handleTrashList(c)

	case len(segs) == 3 && segs[2] == "restore" && c.r.Method == http.MethodPost:
		c.op = "trash_restore"
		s.withIdempotency(c, true, func(c *call) { s.handleTrashRestore(c) })

	case len(segs) == 3 && segs[2] == "purge" && c.r.Method == http.MethodPost:
		c.op = "trash_purge"
		s.withIdempotency(c, true, func(c *call) { s.handleTrashPurge(c) })

	default:
		c.fail(errNotFoundRoute())
	}
}

// runDriveOp routes /v1/drives/{drive}/{op} and the /v1/{op} default-drive
// aliases (§4.3). The alias form resolves the effective default drive first.
func (s *Server) runDriveOp(c *call, drive, op string) {
	if drive == "" {
		drive = c.key.EffectiveDefaultDrive()
		if drive == "" {
			e := errInvalidRequest("this key has no default drive; address /v1/drives/{drive}/... explicitly")
			e.Code = "drive_required"
			var avail []string
			for _, g := range c.key.Drives {
				avail = append(avail, g.Drive)
			}
			e.Hint = "available drives: " + joinQuoted(avail)
			c.fail(e)
			return
		}
	}
	c.drive = drive

	switch op {
	case "list":
		if !s.requireMethod(c, http.MethodGet) {
			return
		}
		c.op = "list"
		s.driveOp(c, auth.PermList, func(c *call) { s.handleList(c) })

	case "stat":
		if !s.requireMethod(c, http.MethodGet) {
			return
		}
		c.op = "stat"
		s.driveOp(c, auth.PermList, func(c *call) { s.handleStat(c) })

	case "download-ticket":
		if !s.requireMethod(c, http.MethodPost) {
			return
		}
		// Requires read (never link): the ticket is a same-origin streaming
		// shortcut for an already-authorized download, not a share.
		c.op = "download_ticket"
		s.driveOp(c, auth.PermRead, func(c *call) { s.handleDownloadTicket(c) })

	case "files":
		switch c.r.Method {
		case http.MethodGet:
			c.op = "download"
			s.driveOp(c, auth.PermRead, func(c *call) { s.handleDownload(c) })
		case http.MethodPut:
			c.op = "upload"
			s.withIdempotency(c, true, func(c *call) {
				s.driveOp(c, auth.PermWrite, func(c *call) { s.handleUpload(c) })
			})
		default:
			s.methodNotAllowed(c, http.MethodGet+", "+http.MethodPut)
		}

	case "mkdir":
		if !s.requireMethod(c, http.MethodPost) {
			return
		}
		c.op = "mkdir"
		s.withIdempotency(c, true, func(c *call) {
			s.driveOp(c, auth.PermWrite, func(c *call) { s.handleMkdir(c) })
		})

	case "delete":
		if !s.requireMethod(c, http.MethodPost) {
			return
		}
		c.op = "delete"
		s.withIdempotency(c, true, func(c *call) {
			s.driveOp(c, auth.PermDelete, func(c *call) { s.handleDelete(c) })
		})

	case "move":
		if !s.requireMethod(c, http.MethodPost) {
			return
		}
		c.op = "move"
		s.withIdempotency(c, true, func(c *call) {
			s.driveOp(c, auth.PermWrite, func(c *call) { s.handleMove(c) })
		})

	case "tree":
		if !s.requireMethod(c, http.MethodGet) {
			return
		}
		c.op = "tree"
		s.driveOp(c, auth.PermList, func(c *call) { s.handleTree(c) })

	case "search":
		if !s.requireMethod(c, http.MethodGet) {
			return
		}
		c.op = "search"
		s.driveOp(c, auth.PermList, func(c *call) { s.handleSearch(c) })

	default:
		c.fail(errNotFoundRoute())
	}
}

func (s *Server) requireMethod(c *call, method string) bool {
	if c.r.Method != method {
		s.methodNotAllowed(c, method)
		return false
	}
	return true
}

func (s *Server) methodNotAllowed(c *call, allow string) {
	e := newAPIError(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed; allow: "+allow)
	c.w.Header().Set("Allow", allow)
	c.fail(e)
}

// driveOp verifies the drive exists, is granted and enabled, then runs the
// permission-checked handler.
func (s *Server) driveOp(c *call, perm auth.Permission, h func(*call)) {
	drive := c.drive
	grant, granted := c.key.DriveGrant(drive)
	if !granted {
		if _, known := s.registry.Get(drive); known {
			c.fail(errForbidden("drive \"" + drive + "\" is not granted to this key"))
		} else {
			var avail []string
			for _, g := range c.key.Drives {
				avail = append(avail, g.Drive)
			}
			c.fail(errDriveNotFound(drive, avail))
		}
		return
	}
	if !c.key.CanOn(drive, perm) {
		c.fail(errForbidden("missing \"" + string(perm) + "\" permission on drive \"" + drive + "\""))
		return
	}
	acct, known := s.registry.Get(drive)
	if !known {
		c.fail(errDriveNotFound(drive, nil))
		return
	}
	if !acct.Enabled {
		c.fail(driveUnavailableErr(drive, "the drive is disabled; ask the administrator to re-enable it"))
		return
	}
	// Storage guard (IMPLEMENTATION §存储被删除与替代存储): a FRESH
	// verification of the drive's backing OpenList storage — by recorded
	// storage id, never by mount fallback — runs before the request proceeds.
	// A storage disabled or deleted in OpenList, one whose credentials were
	// rejected, or an unconfirmed replacement storage answers 503
	// drive_unavailable instead of leaking upstream errors.
	verdict := s.guard.Check(c.r.Context(), drive)
	if !verdict.Allowed() {
		c.fail(driveUnavailableErr(drive, verdict.Message))
		return
	}
	c.drive = drive
	c.root = grant.Root
	s.runDriveHandler(c, h)
}

// runDriveHandler executes the drive op with the §3.7.4 retry: when an
// attempt fails with a storage-not-found class error before anything was
// committed to the client (mount renamed in OpenList inside the 30s health
// cache window), reconcile once and retry the op once — the resolve step then
// picks up the followed mount. Uploads are never retried (their body cannot
// be replayed); they only trigger an asynchronous reconcile.
func (s *Server) runDriveHandler(c *call, h func(*call)) {
	if c.r.Method == http.MethodPut || c.r.ContentLength > 4<<20 {
		h(c)
		if c.errMsg != "" && storageMissingErrText(c.errMsg) {
			s.triggerReconcile()
		}
		return
	}
	// Snapshot small JSON bodies so a retry can decode them again.
	var bodySnapshot []byte
	if c.r.Body != nil && c.r.ContentLength > 0 {
		bodySnapshot, _ = io.ReadAll(c.r.Body)
	}
	real := c.w.ResponseWriter
	attempt := func() (*call, *retryGate) {
		if bodySnapshot != nil {
			c.r.Body = io.NopCloser(bytes.NewReader(bodySnapshot))
		}
		g := &retryGate{real: real}
		ac := *c
		ac.w = &respRecorder{ResponseWriter: g}
		ac.errMsg = ""
		h(&ac)
		return &ac, g
	}
	final, g := attempt()
	if final.errMsg != "" && storageMissingErrText(final.errMsg) {
		if !g.committed {
			s.runReconcile()
			s.healthCache.invalidate()
			final, g = attempt()
		} else {
			s.triggerReconcile()
		}
	}
	g.commit()
	// Adopt the final attempt's outcome into the original call (audit and
	// idempotency caching read the original recorder).
	c.w.status = final.w.status
	c.w.bytes = final.w.bytes
	if c.w.buf != nil && !g.committed {
		c.w.buf.Reset()
		c.w.buf.Write(g.pending.Bytes())
	}
	c.errMsg = final.errMsg
	c.bytes = final.bytes
	c.logical, c.drive, c.root = final.logical, final.drive, final.root
}

// retryGate buffers a response attempt (status + up to 64 KiB of body) so a
// storage-not-found failure can be discarded and retried. The first Flush or
// an oversized body commits the gate to direct pass-through, after which no
// retry is possible.
type retryGate struct {
	real      http.ResponseWriter
	pending   bytes.Buffer
	status    int
	committed bool
}

const retryGateBufferLimit = 64 << 10

func (g *retryGate) Header() http.Header { return g.real.Header() }

func (g *retryGate) WriteHeader(code int) {
	if g.committed {
		g.real.WriteHeader(code)
		return
	}
	if g.status == 0 {
		g.status = code
	}
}

func (g *retryGate) Write(p []byte) (int, error) {
	if g.committed {
		return g.real.Write(p)
	}
	if g.pending.Len()+len(p) > retryGateBufferLimit {
		g.commit()
		return g.real.Write(p)
	}
	return g.pending.Write(p)
}

func (g *retryGate) Flush() {
	if !g.committed {
		g.commit()
	}
	if f, ok := g.real.(http.Flusher); ok {
		f.Flush()
	}
}

// commit flushes the buffered attempt to the client (direct mode from here on).
func (g *retryGate) commit() {
	if g.committed {
		return
	}
	g.committed = true
	if g.status != 0 {
		g.real.WriteHeader(g.status)
	}
	if g.pending.Len() > 0 {
		_, _ = g.real.Write(g.pending.Bytes())
		g.pending.Reset()
	}
}

// runReconcile executes one bounded synchronous reconcile pass.
func (s *Server) runReconcile() {
	if s.reconcileNow == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	s.reconcileNow(ctx)
}

// triggerReconcile fires one asynchronous pass (deduplicated inside the
// reconciler).
func (s *Server) triggerReconcile() {
	if s.reconcileNow == nil {
		return
	}
	go s.runReconcile()
}

func storageMissingErrText(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "storage not found") || strings.Contains(m, "failed get storage")
}

// withIdempotency replays a stored response when the same key presents the
// same Idempotency-Key within the TTL; otherwise it runs the handler and
// stores the response. Only successful (2xx) responses are cached, so failed
// operations — including failed uploads — are retried for real (§4.10.2).
func (s *Server) withIdempotency(c *call, mutating bool, h func(*call)) {
	idem := strings.TrimSpace(c.r.Header.Get("Idempotency-Key"))
	if !mutating || idem == "" {
		h(c)
		return
	}
	if rec, err := s.st.GetIdempotency(c.key.ID, idem, idempotencyTTL); err == nil {
		if rec.Method != c.r.Method || rec.Route != c.r.URL.Path {
			c.fail(newAPIError(http.StatusConflict, CodeIdempMismatch,
				"this Idempotency-Key was already used for "+rec.Method+" "+rec.Route))
			return
		}
		if rec.ContentType != "" {
			c.w.Header().Set("Content-Type", rec.ContentType)
		}
		c.w.Header().Set("X-Idempotent-Replay", "true")
		c.w.WriteHeader(rec.Status)
		_, _ = c.w.Write(rec.Body)
		c.errMsg = "idempotent replay"
		return
	}
	c.w.buf = &bytes.Buffer{}
	h(c)
	status := c.w.status
	if status == 0 {
		status = http.StatusOK
	}
	cacheable := status < 400
	if cacheable && c.w.buf != nil {
		_ = s.st.PutIdempotency(store.IdempotencyRecord{
			KeyID:       c.key.ID,
			IdemKey:     idem,
			Method:      c.r.Method,
			Route:       c.r.URL.Path,
			Status:      status,
			Body:        c.w.buf.Bytes(),
			ContentType: "application/json; charset=utf-8",
		})
	}
}

// audit writes one audit row for the request. Never records key material
// (§5.2): key id, drive, op, visible path, bytes, status, duration, summary.
func (s *Server) audit(c *call) {
	status := c.w.status
	if status == 0 {
		status = http.StatusOK
	}
	keyID := c.auditKeyID
	if c.key != nil {
		keyID = c.key.ID
	}
	bytes := c.bytes
	if bytes == 0 {
		bytes = c.w.bytes
	}
	if err := s.st.InsertAudit(store.AuditEntry{
		KeyID:      keyID,
		Drive:      c.drive,
		Op:         c.op,
		Path:       c.logical,
		Bytes:      bytes,
		Status:     status,
		DurationMS: time.Since(c.start).Milliseconds(),
		Error:      c.errMsg,
	}); err != nil {
		s.log.Warn("audit insert failed", "err", err)
	}
}

// baseURL resolves the public base URL for rendered assets: the
// public_base_url setting when set, else the request Host (§5.3).
func (s *Server) baseURL(r *http.Request) string {
	if v, ok := s.st.GetSetting("public_base_url"); ok && v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// policyText returns the instance policy text (setting or built-in default).
func (s *Server) policyText() string {
	if v, ok := s.st.GetSetting("policy_text"); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return "max upload 8 GiB per file; /.c2a-trash trash retention 7 days by default; other paths are durable netdisk storage — do not delete files you did not create. Traffic passes through the gateway host."
}

// ---- small JSON helpers ----

func decodeBody(c *call, out any) bool {
	c.r.Body = http.MaxBytesReader(c.w, c.r.Body, 4<<20)
	data, err := io.ReadAll(c.r.Body)
	if err != nil {
		c.fail(errInvalidRequest("failed to read request body"))
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		c.fail(errInvalidRequest("request body must be JSON"))
		return false
	}
	if err := json.Unmarshal(data, out); err != nil {
		c.fail(errInvalidRequest("request body must be valid JSON: " + err.Error()))
		return false
	}
	return true
}

// fsError converts an OpenList business error into a typed API error.
func fsError(err error) *APIError {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "object not found") || strings.Contains(lower, "no such file"):
		return errPathNotFound("")
	case strings.Contains(lower, "storage not found") || strings.Contains(lower, "failed get storage"):
		e := newAPIError(http.StatusServiceUnavailable, CodeDriveUnavailable, "the backing storage is unavailable: "+msg)
		e.Retryable = true
		e.Hint = "the drive's OpenList storage may have been renamed, disabled or deleted; an administrator can fix it from the portal"
		return e
	case strings.Contains(lower, "exists") || strings.Contains(lower, "already exist"):
		return errAlreadyExists("")
	default:
		return errUpstream(msg)
	}
}
