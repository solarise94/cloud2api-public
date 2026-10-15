// Package gateway implements the cloud2api HTTP surface: Bearer
// authentication, account whitelist and permission checks, path sanitation
// (DESIGN.md §4.3) and streaming proxying to OpenList (§5).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

const (
	// maxJSONRequestBody bounds client JSON metadata bodies.
	maxJSONRequestBody = 8 << 20
	// maxUpstreamJSONBody bounds upstream JSON responses held in memory for
	// prefix rewriting; file payloads are never buffered.
	maxUpstreamJSONBody = 32 << 20
)

type operation int

const (
	opList operation = iota
	opGet
	opPut
	opMkdir
	opRemove
	opCopy
	opMove
	opRaw
)

// opPermission is the route-to-permission mapping of DESIGN.md §4.2/§5 under
// the v0.4 vocabulary: remove needs the dedicated delete permission (legacy
// config keys with "write" were expanded to write+delete at import).
var opPermission = map[operation]auth.Permission{
	opList:   auth.PermList,
	opGet:    auth.PermRead,
	opRaw:    auth.PermRead,
	opPut:    auth.PermWrite,
	opMkdir:  auth.PermWrite,
	opRemove: auth.PermDelete,
	opCopy:   auth.PermWrite,
	opMove:   auth.PermWrite,
}

var opAPIPath = map[operation]string{
	opList:   "/api/fs/list",
	opGet:    "/api/fs/get",
	opMkdir:  "/api/fs/mkdir",
	opRemove: "/api/fs/remove",
	opCopy:   "/api/fs/copy",
	opMove:   "/api/fs/move",
}

type Server struct {
	cfg      *config.Config
	keys     *keymgr.Manager
	client   *openlist.Client
	registry *AccountRegistry
	log      *slog.Logger
	audit    func(store.AuditEntry)
	// onStorageMissing fires (async reconcile) when the legacy data plane
	// hits a storage-not-found class upstream error (Phase 1c, §3.7.4).
	onStorageMissing func()
	// storageGuard verifies each drive's backing OpenList storage before any
	// file access is proxied (IMPLEMENTATION §存储被删除与替代存储). New
	// installs a verifying default so a forgotten wiring can never mean "no
	// check"; SetStorageGuard installs the store-backed one.
	storageGuard *storageid.Guard
}

// New builds the gateway server. A nil registry falls back to one built
// from cfg.Accounts (static accounts only).
func New(cfg *config.Config, keys *keymgr.Manager, client *openlist.Client, reg *AccountRegistry, logger *slog.Logger) *Server {
	if reg == nil {
		reg = NewAccountRegistry(cfg.Accounts)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cfg:      cfg,
		keys:     keys,
		client:   client,
		registry: reg,
		log:      logger,
		storageGuard: storageid.NewGuard(client, nil, func(driveID string) (string, bool) {
			a, ok := reg.Get(driveID)
			return a.MountPath, ok
		}, logger),
	}
}

// SetAudit installs the data-plane audit sink (v0.4 §5.2).
func (s *Server) SetAudit(f func(store.AuditEntry)) { s.audit = f }

// SetStorageMissingHook installs the async reconcile trigger fired when the
// legacy data plane observes storage-not-found class upstream errors.
func (s *Server) SetStorageMissingHook(f func()) { s.onStorageMissing = f }

// SetStorageGuard installs the store-backed storage guard (main wires the one
// that resolves drive identity from the store and the reconcile hook).
func (s *Server) SetStorageGuard(g *storageid.Guard) {
	if g != nil {
		s.storageGuard = g
	}
}

// guardDrive verifies each distinct drive's backing OpenList storage right
// before the request is proxied (after path validation, so hostile paths
// never reach OpenList at all). A denied verdict answers 503 and stops the
// request.
func (s *Server) guardDrive(w http.ResponseWriter, r *http.Request, drives ...string) bool {
	seen := map[string]bool{}
	for _, id := range drives {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		verdict := s.storageGuard.Check(r.Context(), id)
		if verdict.Allowed() {
			continue
		}
		msg := verdict.Message
		if msg == "" {
			msg = "drive " + id + " is currently unavailable (" + verdict.State + ")"
		}
		setAuditErr(r, verdict.State+": "+msg)
		writeError(w, http.StatusServiceUnavailable, msg)
		return false
	}
	return true
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.audited(w, r)
	})
}

// statusRecorder captures the response status for the audit row.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(p []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(p)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditRecord is the mutable per-request audit context; handlers fill in the
// fields as they parse (never key material, only ids and logical paths).
type auditRecord struct {
	KeyID string
	Drive string
	Op    string
	Path  string
	Error string
}

type auditCtxKey struct{}

func auditFrom(r *http.Request) *auditRecord {
	rec, _ := r.Context().Value(auditCtxKey{}).(*auditRecord)
	return rec
}

func setAuditPath(r *http.Request, p string) {
	if rec := auditFrom(r); rec != nil {
		rec.Path = p
	}
}

func setAuditErr(r *http.Request, msg string) {
	if rec := auditFrom(r); rec != nil {
		rec.Error = msg
	}
}

// audited runs route with an audit record around it.
func (s *Server) audited(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		s.route(w, r)
		return
	}
	rec := &auditRecord{}
	r = r.WithContext(context.WithValue(r.Context(), auditCtxKey{}, rec))
	srw := &statusRecorder{ResponseWriter: w}
	start := time.Now()
	s.route(srw, r)
	status := srw.status
	if status == 0 {
		status = http.StatusOK
	}
	s.audit(store.AuditEntry{
		TS:         start.UTC(),
		KeyID:      rec.KeyID,
		Drive:      rec.Drive,
		Op:         rec.Op,
		Path:       rec.Path,
		Status:     status,
		DurationMS: time.Since(start).Milliseconds(),
		Error:      rec.Error,
	})
}

// route dispatches manually on the already-decoded r.URL.Path instead of
// http.ServeMux: the mux would clean and 301-redirect malformed paths,
// while §4.3 requires rejecting them outright.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/transfer":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleTransfer(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/storage/"):
		s.handleStorage(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if rec := auditFrom(r); rec != nil {
		rec.KeyID = key.ID
		rec.Op = opAuditName(r.URL.Path)
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/storage/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	acctID, tail := rest[:slash], rest[slash+1:]
	if rec := auditFrom(r); rec != nil {
		rec.Drive = acctID
	}

	// Account whitelist (§4.2) before any permission or path work.
	acct, known := s.registry.Get(acctID)
	if !known {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if !key.HasAccount(acctID) {
		writeError(w, http.StatusForbidden, "account not in key whitelist")
		return
	}
	if !acct.Enabled {
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}

	var op operation
	var rawTail string
	switch {
	case tail == "list":
		op = opList
	case tail == "get":
		op = opGet
	case tail == "put":
		op = opPut
	case tail == "mkdir":
		op = opMkdir
	case tail == "remove":
		op = opRemove
	case tail == "copy":
		op = opCopy
	case tail == "move":
		op = opMove
	case strings.HasPrefix(tail, "raw/"):
		op = opRaw
		rawTail = tail[len("raw/"):]
	default:
		// Unknown operation — but if the tail is hostile (net/http decodes
		// the URL path once before routing, so encoded dot-dot lands here),
		// answer 400 like the cleaner would instead of 404.
		if LooksHostPath(tail) || LooksHostPath(UnescapeOnce(tail)) {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		writeError(w, http.StatusNotFound, "unknown operation")
		return
	}

	switch op {
	case opList, opGet, opMkdir, opRemove, opCopy, opMove:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
	case opPut:
		if r.Method != http.MethodPut {
			methodNotAllowed(w, http.MethodPut)
			return
		}
	case opRaw:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
	}

	// Permission check (§4.2) before touching any path; every authorization
	// decision happens before proxying.
	if !key.HasPermission(opPermission[op]) {
		writeError(w, http.StatusForbidden, "missing permission "+string(opPermission[op]))
		return
	}

	switch op {
	case opRaw:
		s.handleRaw(w, r, acct, rawTail)
	case opPut:
		s.handlePut(w, r, acct)
	default:
		s.handleJSONOp(w, r, key, acct, op)
	}
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*auth.Key, bool) {
	presented := ""
	if k := strings.TrimSpace(r.Header.Get("apikey")); k != "" {
		presented = k
	} else {
		const scheme = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) > len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
			presented = strings.TrimSpace(h[len(scheme):])
		}
	}
	if presented == "" {
		writeError(w, http.StatusUnauthorized, "missing or malformed bearer token")
		return nil, false
	}
	// The response deliberately does not distinguish unknown keys from
	// malformed ones (§4.1).
	key, outcome := s.keys.Authenticate(presented)
	if outcome != auth.AuthOK || key == nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return nil, false
	}
	return key, true
}

// opAuditName derives the audit op from the legacy URL.
func opAuditName(p string) string {
	rest := strings.TrimPrefix(p, "/api/storage/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i+1:]
	}
	if strings.HasPrefix(rest, "raw/") {
		return "raw"
	}
	return strings.Trim(rest, "/")
}

// mountOf re-reads the drive's live mount path. The storage guard's
// reconcile hook may have followed a mount rename during the check, so the
// translation into an internal path must not use a stale snapshot.
func (s *Server) mountOf(driveID string) string {
	if a, ok := s.registry.Get(driveID); ok {
		return a.MountPath
	}
	return ""
}

// guardTranslate validates a logical path, runs the storage guard and only
// then joins the drive's (live) mount path. Validation always precedes the
// guard so hostile input never reaches OpenList, not even its admin API.
func (s *Server) guardTranslate(w http.ResponseWriter, r *http.Request, driveID, logical string) (string, bool) {
	cleaned, err := CleanLogicalPath(logical)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return "", false
	}
	if !s.guardDrive(w, r, driveID) {
		return "", false
	}
	internal, err := MountJoin(s.mountOf(driveID), cleaned)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return "", false
	}
	return internal, true
}

// handleJSONOp proxies the fs/list|get|mkdir|remove|copy|move family. The
// body is OpenList's native format; only the path fields are translated to
// internal mount paths and the dst_account extension is removed before
// forwarding (§5). Every drive involved is storage-guarded before anything
// is proxied.
func (s *Server) handleJSONOp(w http.ResponseWriter, r *http.Request, key *auth.Key, acct config.Account, op operation) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}

	var apiPath string
	switch op {
	case opList, opGet, opMkdir:
		p, err := pathField(body, "path", op != opMkdir)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		internal, ok := s.guardTranslate(w, r, acct.ID, p)
		if !ok {
			return
		}
		body["path"] = internal
		apiPath = opAPIPath[op]
		setAuditPath(r, p)

	case opRemove:
		dir, err := pathField(body, "dir", false)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !s.cleanNamesField(w, body) {
			return
		}
		internal, ok := s.guardTranslate(w, r, acct.ID, dir)
		if !ok {
			return
		}
		body["dir"] = internal
		setAuditPath(r, dir)
		apiPath = opAPIPath[op]

	case opCopy, opMove:
		// dst_account names the destination account logically; it is
		// validated and stripped here and translated after the guard.
		dstID := acct.ID
		if raw, present := body["dst_account"]; present {
			delete(body, "dst_account")
			id, isStr := raw.(string)
			if !isStr || id == "" {
				writeError(w, http.StatusBadRequest, "dst_account must be a non-empty account id")
				return
			}
			if id != acct.ID {
				da, known := s.registry.Get(id)
				if !known {
					writeError(w, http.StatusNotFound, "account not found")
					return
				}
				if !key.HasAccount(id) {
					writeError(w, http.StatusForbidden, "account not in key whitelist")
					return
				}
				if !key.HasPermission(auth.PermTransfer) {
					writeError(w, http.StatusForbidden, "missing permission "+string(auth.PermTransfer))
					return
				}
				if !da.Enabled {
					writeError(w, http.StatusForbidden, "account disabled")
					return
				}
				dstID = da.ID
			}
		}
		srcDir, err := pathField(body, "src_dir", false)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		dstDir, err := pathField(body, "dst_dir", false)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		cleanedSrc, err := CleanLogicalPath(srcDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		cleanedDst, err := CleanLogicalPath(dstDir)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		if !s.cleanNamesField(w, body) {
			return
		}
		// Both endpoints are guarded (source and destination drive).
		if !s.guardDrive(w, r, acct.ID, dstID) {
			return
		}
		srcInternal, err := MountJoin(s.mountOf(acct.ID), cleanedSrc)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		dstInternal, err := MountJoin(s.mountOf(dstID), cleanedDst)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid path")
			return
		}
		body["src_dir"], body["dst_dir"] = srcInternal, dstInternal
		apiPath = opAPIPath[op]

	default:
		writeError(w, http.StatusInternalServerError, "unhandled operation")
		return
	}

	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode request body")
		return
	}
	s.forwardJSON(w, r, http.MethodPost, apiPath, payload)
}

// handleTransfer is the cross-account convenience entry (§5): it expands to
// an OpenList fs/copy call with both endpoints translated to internal paths.
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if rec := auditFrom(r); rec != nil {
		rec.KeyID = key.ID
		rec.Op = "transfer"
	}
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}

	srcID, err := requiredString(body, "src_account")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dstID, err := requiredString(body, "dst_account")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	srcAcct, known := s.registry.Get(srcID)
	if !known {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	dstAcct, known := s.registry.Get(dstID)
	if !known {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if !key.HasAccount(srcID) || !key.HasAccount(dstID) {
		writeError(w, http.StatusForbidden, "account not in key whitelist")
		return
	}
	if !key.HasPermission(auth.PermTransfer) {
		writeError(w, http.StatusForbidden, "missing permission "+string(auth.PermTransfer))
		return
	}
	if !srcAcct.Enabled || !dstAcct.Enabled {
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}

	delete(body, "src_account")
	delete(body, "dst_account")

	srcDir, err := pathField(body, "src_dir", true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dstDir, err := pathField(body, "dst_dir", true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cleanedSrc, err := CleanLogicalPath(srcDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	cleanedDst, err := CleanLogicalPath(dstDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	setAuditPath(r, srcDir)
	if !s.cleanNamesField(w, body) {
		return
	}

	// Storage guard: both endpoints must be backed by their confirmed
	// OpenList storages before the copy is proxied; the translation below
	// uses the live mounts (the hook may have followed a rename).
	if !s.guardDrive(w, r, srcID, dstID) {
		return
	}
	srcInternal, err := MountJoin(s.mountOf(srcID), cleanedSrc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	dstInternal, err := MountJoin(s.mountOf(dstID), cleanedDst)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}
	body["src_dir"], body["dst_dir"] = srcInternal, dstInternal

	payload, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode request body")
		return
	}
	s.forwardJSON(w, r, http.MethodPost, "/api/fs/copy", payload)
}

// handlePut streams an upload body straight through to PUT /api/fs/put,
// translating the File-Path header to the internal mount path.
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, acct config.Account) {
	fp := r.Header.Get("File-Path")
	if fp == "" {
		writeError(w, http.StatusBadRequest, "missing File-Path header")
		return
	}
	decoded := UnescapeOnce(fp)
	setAuditPath(r, decoded)
	internal, ok := s.guardTranslate(w, r, acct.ID, decoded)
	if !ok {
		return
	}

	hdr := http.Header{}
	hdr.Set("File-Path", url.QueryEscape(internal))
	for _, h := range []string{"Content-Type", "As-Task", "Overwrite"} {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), openlist.RelaxedTimeout)
	defer cancel()
	req, err := s.client.NewRequest(ctx, http.MethodPut, url.URL{Path: "/api/fs/put"}, r.Body, r.ContentLength, hdr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	// r.Body is relayed verbatim: io.Copy streaming, nothing buffered.
	s.doUpstreamJSON(w, req)
}

// handleRaw streams a download from OpenList's /d download endpoint; the body
// is relayed with io.Copy and never buffered (§7). OpenList requires a
// per-file sign parameter on /d, so the sign is fetched server-side with a
// fs/get call first; any client-supplied sign is discarded. The query is a
// strict whitelist — the server-fetched sign plus the caller's own `type`
// selector when one was supplied — so a client key, path or any other query
// parameter is never forwarded upstream. DoDownload follows OpenList's
// direct-link 302 server-side, so the upstream URL never reaches the client.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request, acct config.Account, logicalTail string) {
	setAuditPath(r, "/"+logicalTail)
	internal, ok := s.guardTranslate(w, r, acct.ID, "/"+logicalTail)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), openlist.RelaxedTimeout)
	defer cancel()

	sign, err := s.fetchSign(ctx, internal)
	if err != nil {
		s.log.Warn("sign fetch failed", "err", err)
		writeError(w, http.StatusBadGateway, "failed to resolve download URL")
		return
	}

	q := url.Values{}
	q.Set("sign", sign)
	if t := r.URL.Query().Get("type"); t != "" {
		q.Set("type", t)
	}
	u := url.URL{Path: "/d" + internal, RawQuery: q.Encode()}
	hdr := http.Header{}
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}

	resp, err := s.client.DoDownload(ctx, http.MethodGet, u, hdr)
	if err != nil {
		// Generic on purpose: a transport error's text embeds the (redirect)
		// URL, which must not leak into the response.
		s.log.Warn("raw upstream request failed", "err", err)
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	// A redirect the helper should have followed server-side is an upstream
	// failure: forwarding it would leak the direct-link URL via Location.
	if rawRedirect(resp.StatusCode) {
		writeError(w, http.StatusBadGateway, "upstream returned "+resp.Status)
		return
	}

	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.log.Warn("raw response copy interrupted", "err", err)
	}
}

// rawRedirect reports whether status is one of the redirect codes the openlist
// DoDownload helper follows server-side; a residual redirect is a failure.
func rawRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// fetchSign resolves the download sign for an internal path via fs/get.
func (s *Server) fetchSign(ctx context.Context, internal string) (string, error) {
	payload, err := json.Marshal(map[string]string{"path": internal})
	if err != nil {
		return "", err
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	req, err := s.client.NewRequest(ctx, http.MethodPost, url.URL{Path: "/api/fs/get"}, bytes.NewReader(payload), int64(len(payload)), hdr)
	if err != nil {
		return "", err
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Sign string `json:"sign"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamJSONBody)).Decode(&doc); err != nil {
		return "", err
	}
	if doc.Code != http.StatusOK {
		return "", fmt.Errorf("fs/get code=%d message=%s", doc.Code, doc.Message)
	}
	return doc.Data.Sign, nil
}

func (s *Server) forwardJSON(w http.ResponseWriter, r *http.Request, method, apiPath string, payload []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), openlist.DefaultTimeout)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	req, err := s.client.NewRequest(ctx, method, url.URL{Path: apiPath}, bytes.NewReader(payload), int64(len(payload)), hdr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	s.doUpstreamJSON(w, req)
}

// doUpstreamJSON relays an OpenList JSON API response. Business errors
// (non-200 body code, possibly behind HTTP 200) and transport-level errors
// are surfaced with the upstream's native code/message; success bodies have
// internal mount prefixes stripped back to logical paths and the leak keys
// raw_url/sign/provider removed (v0.3.1). Rewriting never blocks the
// response: on failure the body is passed through unmodified and the problem
// is logged (§1).
func (s *Server) doUpstreamJSON(w http.ResponseWriter, req *http.Request) {
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		s.log.Warn("upstream request failed", "err", err)
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamJSONBody))
	if err != nil {
		s.log.Warn("upstream response read failed", "err", err)
		writeError(w, http.StatusBadGateway, "upstream response read failed")
		return
	}

	var doc any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	parseErr := dec.Decode(&doc)
	obj, isObj := doc.(map[string]any)

	if parseErr == nil && isObj {
		if cv, present := obj["code"]; present {
			if code, ok := jsonInt(cv); ok && code != 0 && code != http.StatusOK {
				status := code
				if status < 100 || status > 599 {
					status = http.StatusBadGateway
				}
				msg, _ := obj["message"].(string)
				if msg == "" {
					msg = "upstream error"
				}
				if s.onStorageMissing != nil && storageMissingText(msg) {
					s.onStorageMissing()
				}
				writeJSON(w, status, map[string]any{"code": code, "message": s.stripMount(msg)})
				return
			}
		}
	}
	if resp.StatusCode >= 400 {
		msg := http.StatusText(resp.StatusCode)
		if parseErr == nil && isObj {
			if m, _ := obj["message"].(string); m != "" {
				msg = m
			}
		}
		writeJSON(w, resp.StatusCode, map[string]any{"code": resp.StatusCode, "message": s.stripMount(msg)})
		return
	}

	if parseErr != nil {
		s.log.Warn("upstream response is not JSON; passing through unmodified")
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}

	out, err := json.Marshal(scrubSensitive(rewriteJSON(doc, s.registry.Mounts())))
	if err != nil {
		s.log.Warn("mount prefix rewrite failed; passing response through unmodified", "err", err)
		out = data
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// storageMissingText matches the upstream error texts OpenList answers when
// the addressed storage does not exist or its mount changed (Phase 1c
// reconcile trigger, §3.7.4).
func storageMissingText(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "storage not found") || strings.Contains(m, "failed get storage")
}

func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONRequestBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "failed to read request body")
		}
		return nil, false
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			writeError(w, http.StatusBadRequest, "request body must be a JSON object")
			return nil, false
		}
	}
	return doc, true
}

func (s *Server) cleanNamesField(w http.ResponseWriter, doc map[string]any) bool {
	raw, present := doc["names"]
	if !present {
		writeError(w, http.StatusBadRequest, "missing names field")
		return false
	}
	arr, ok := raw.([]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "names must be an array")
		return false
	}
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		name, isStr := e.(string)
		if !isStr {
			writeError(w, http.StatusBadRequest, "names entries must be strings")
			return false
		}
		clean, err := CleanName(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid name")
			return false
		}
		out = append(out, clean)
	}
	doc["names"] = out
	return true
}

func (s *Server) stripMount(msg string) string {
	return StripMountPrefixes(msg, s.registry.Mounts())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": status, "message": msg})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// pathField extracts a string path field. allowMissing substitutes "/".
func pathField(doc map[string]any, field string, allowMissing bool) (string, error) {
	v, present := doc[field]
	if !present {
		if allowMissing {
			return "/", nil
		}
		return "", errors.New("missing " + field + " field")
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New(field + " must be a string")
	}
	return s, nil
}

func requiredString(doc map[string]any, field string) (string, error) {
	v, present := doc[field]
	if !present {
		return "", errors.New("missing " + field + " field")
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", errors.New(field + " must be a non-empty string")
	}
	return s, nil
}

func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}
