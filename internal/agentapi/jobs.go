package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/store"
)

// ---- shared request shapes ----

// drivePathRef addresses one path on one drive (drive optional: the key's
// default drive).
type drivePathRef struct {
	Drive string `json:"drive"`
	Path  string `json:"path"`
}

// UnmarshalJSON accepts the object form and the shorthand string form
// ({"from":"/a"} instead of {"from":{"path":"/a"}}).
func (r *drivePathRef) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		r.Path = s
		return nil
	}
	type alias drivePathRef
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = drivePathRef(a)
	return nil
}

func (r drivePathRef) driveOf(def string) string {
	if r.Drive != "" {
		return r.Drive
	}
	return def
}

// transferRequest is the POST /v1/transfers body (§4.10.4).
type transferRequest struct {
	From       drivePathRef `json:"from"`
	To         drivePathRef `json:"to"`
	Mode       string       `json:"mode"` // copy | move
	OnConflict string       `json:"on_conflict"`
	DryRun     bool         `json:"dry_run"`
}

// fetchRequest is the POST /v1/fetch body.
type fetchRequest struct {
	URL        string `json:"url"`
	Drive      string `json:"drive"`
	Path       string `json:"path"`
	OnConflict string `json:"on_conflict"`
	DryRun     bool   `json:"dry_run"`
}

// batchOp is one operation of POST /v1/batch (§4.10.5).
type batchOp struct {
	Op         string       `json:"op"` // mkdir | delete | move | copy
	Drive      string       `json:"drive"`
	Path       string       `json:"path"`  // mkdir
	Paths      []string     `json:"paths"` // delete
	From       drivePathRef `json:"from"`  // move | copy
	To         drivePathRef `json:"to"`    // move | copy
	OnConflict string       `json:"on_conflict"`
}

type batchRequest struct {
	Ops    []batchOp `json:"ops"`
	DryRun bool      `json:"dry_run"`
}

func normalizeOnConflict(c *call, v string) (string, bool) {
	switch v {
	case "":
		return "fail", true
	case "fail", "overwrite", "rename":
		return v, true
	}
	c.fail(errInvalidRequest("on_conflict must be one of fail, overwrite, rename"))
	return "", false
}

// checkJobDrive validates that the key may use drive with perms and returns
// its chroot root. Mirrors driveOp's checks for the job endpoints (both the
// synchronous validation and the runner re-validation), including the storage
// guard: both the from and to drives of every transfer/batch/fetch op pass
// through here, at submit time and again when the job runner re-validates.
func (s *Server) checkJobDrive(c *call, key *auth.Key, drive string, perms ...auth.Permission) (string, *APIError) {
	grant, granted := key.DriveGrant(drive)
	if !granted {
		if _, known := s.registry.Get(drive); known {
			return "", errForbidden("drive \"" + drive + "\" is not granted to this key")
		}
		var avail []string
		for _, g := range key.Drives {
			avail = append(avail, g.Drive)
		}
		return "", errDriveNotFound(drive, avail)
	}
	for _, p := range perms {
		if !key.CanOn(drive, p) {
			return "", errForbidden("missing \"" + string(p) + "\" permission on drive \"" + drive + "\"")
		}
	}
	acct, known := s.registry.Get(drive)
	if !known {
		return "", errDriveNotFound(drive, nil)
	}
	if !acct.Enabled {
		return "", driveUnavailableErr(drive, "the drive is disabled; ask the administrator to re-enable it")
	}
	ctx := context.Background()
	if c != nil && c.r != nil {
		ctx = c.r.Context()
	}
	if verdict := s.guard.Check(ctx, drive); !verdict.Allowed() {
		return "", driveUnavailableErr(drive, verdict.Message)
	}
	return grant.Root, nil
}

// jobConcurrencyGuard enforces limits.concurrent_jobs (429 with a hint).
func (s *Server) jobConcurrencyGuard(c *call) bool {
	limit := c.key.Limits.ConcurrentJobs
	if limit <= 0 {
		return true
	}
	n, err := s.st.ActiveJobCount(c.key.ID)
	if err != nil {
		c.fail(errUpstream("job count query failed"))
		return false
	}
	if n >= limit {
		e := errRateLimited(time.Second * 5)
		e.Message = fmt.Sprintf("concurrent job limit reached (%d running/queued jobs; limit %d)", n, limit)
		e.Hint = "wait for running jobs with GET /v1/jobs/{id}?wait=60; the concurrent_jobs limit is set by an administrator when editing the key"
		c.fail(e)
		return false
	}
	return true
}

func newJobID() string {
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return "job-" + hex.EncodeToString(buf)
}

// ---- POST /v1/transfers ----

func (s *Server) handleTransferCreate(c *call) {
	var body transferRequest
	if !decodeBody(c, &body) {
		return
	}
	if body.From.Path == "" || body.To.Path == "" {
		c.fail(errInvalidRequest("fields `from` and `to` (each {drive,path}) are required"))
		return
	}
	if body.Mode == "" {
		body.Mode = "copy"
	}
	if body.Mode != "copy" && body.Mode != "move" {
		c.fail(errInvalidRequest("mode must be copy or move"))
		return
	}
	var ok bool
	if body.OnConflict, ok = normalizeOnConflict(c, body.OnConflict); !ok {
		return
	}
	fromDrive := body.From.driveOf(c.key.EffectiveDefaultDrive())
	toDrive := body.To.driveOf(c.key.EffectiveDefaultDrive())
	if fromDrive == "" || toDrive == "" {
		c.fail(errInvalidRequest("field `drive` is required for from/to (this key has no default drive)"))
		return
	}

	// Perms (§scope item 3): transfer + read on src, transfer + write on dst.
	if _, e := s.checkJobDrive(c, c.key, fromDrive, auth.PermTransfer, auth.PermRead); e != nil {
		c.fail(e)
		return
	}
	if _, e := s.checkJobDrive(c, c.key, toDrive, auth.PermTransfer, auth.PermWrite); e != nil {
		c.fail(e)
		return
	}

	// Resolve both sides to compute visible paths for dry-run and conflicts.
	srcRoot, _ := c.key.DriveGrant(fromDrive)
	srcCall := s.subCall(c, fromDrive, srcRoot.Root)
	_, fromInternal, fromVisible, e := s.resolve(srcCall, body.From.Path)
	if e != nil {
		c.fail(e)
		return
	}
	dstRoot, _ := c.key.DriveGrant(toDrive)
	dstCall := s.subCall(c, toDrive, dstRoot.Root)
	_, toInternal, toVisible, e := s.resolve(dstCall, body.To.Path)
	if e != nil {
		c.fail(e)
		return
	}

	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	// Source must exist.
	srcInfo, err := s.fsGet(ctx, fromInternal)
	if err != nil {
		if isNotFoundError(err) {
			ae := errPathNotFound(fromVisible)
			if fromDrive == toDrive {
				ae.Suggestions = s.suggestionsFor(ctx, srcCall, fromVisible)
			}
			c.fail(ae)
			return
		}
		c.fail(fsError(err))
		return
	}

	// Determine the effective target: `to` may be an existing directory
	// (copy into it) or a full target path.
	targetInternal, targetVisible := toInternal, toVisible
	toInfo, toErr := s.fsGet(ctx, toInternal)
	if toErr == nil && toInfo.IsDir {
		targetInternal = path.Join(toInternal, path.Base(fromInternal))
		targetVisible = path.Join(toVisible, path.Base(fromVisible))
	} else if toErr != nil && !isNotFoundError(toErr) {
		c.fail(fsError(toErr))
		return
	}

	// Conflict handling happens up front so agents get immediate feedback;
	// destructive steps (overwrite) run inside the job.
	effectiveTo := targetVisible
	if _, existsErr := s.fsGet(ctx, targetInternal); existsErr == nil {
		switch body.OnConflict {
		case "fail":
			c.fail(errAlreadyExists(targetVisible))
			return
		case "rename":
			for n := 1; n <= 999; n++ {
				cand := renameCandidate(targetInternal, n)
				if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
					targetInternal = cand
					effectiveTo = renameCandidate(targetVisible, n)
					break
				} else if err != nil {
					c.fail(fsError(err))
					return
				}
			}
		}
	}

	if body.DryRun {
		s.respondDryRun(c, srcCall, fromVisible)
		return
	}

	if !s.jobConcurrencyGuard(c) {
		return
	}

	eff := body
	eff.From.Drive, eff.To.Drive = fromDrive, toDrive
	reqJSON, _ := json.Marshal(map[string]any{
		"from":         eff.From,
		"to":           eff.To,
		"mode":         eff.Mode,
		"on_conflict":  eff.OnConflict,
		"effective_to": effectiveTo,
		"source":       map[string]any{"is_dir": srcInfo.IsDir, "size": srcInfo.Size},
	})
	job := store.Job{
		ID: newJobID(), KeyID: c.key.ID, Kind: store.JobKindTransfer,
		State: store.JobQueued, Request: string(reqJSON), Result: "{}",
	}
	if err := s.st.InsertJob(job); err != nil {
		c.fail(errUpstream("job create failed"))
		return
	}
	go s.runJob(context.Background(), job, false)
	writeJSON(c.w, http.StatusAccepted, map[string]any{"job": s.jobWithProgress(job)})
}

// jobWithProgress reloads a just-inserted job for rendering.
func (s *Server) jobWithProgress(j store.Job) map[string]any {
	if fresh, err := s.st.GetJob(j.ID); err == nil {
		return jobView(fresh)
	}
	return jobView(j)
}

// subCall clones the request call onto a specific drive+root (path resolution
// and audit bookkeeping without a second HTTP request).
func (s *Server) subCall(c *call, drive, root string) *call {
	return &call{w: c.w, r: c.r, key: c.key, base: c.base, drive: drive, root: root, start: c.start}
}

// respondDryRun answers a dry_run request with the affected inventory
// (§4.10.3): counts, total bytes and the first 200 items.
func (s *Server) respondDryRun(c *call, src *call, visible string) {
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	stats, e := s.walkPath(ctx, src, visible, walkDefaultMaxEntries, time.Now().Add(25*time.Second))
	if e != nil {
		c.fail(e)
		return
	}
	items := stats.Items
	if len(items) > 200 {
		items = items[:200]
	}
	c.ok(map[string]any{
		"dry_run": true,
		"would_affect": map[string]any{
			"files": stats.Files,
			"dirs":  stats.Dirs,
			"bytes": stats.Bytes,
		},
		"items":     items,
		"truncated": stats.Truncated || len(stats.Items) > 200,
	})
}

// ---- transfer runner ----

// runTransferJob executes a queued transfer. It returns nil only when it set
// the job's terminal state itself.
func (s *Server) runTransferJob(ctx context.Context, job store.Job, resumed bool) error {
	var req transferRequest
	var extra struct {
		EffectiveTo string `json:"effective_to"`
	}
	raw := map[string]any{}
	_ = json.Unmarshal([]byte(job.Request), &raw)
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("corrupt job request: %w", err)
	}
	_ = json.Unmarshal(b, &extra)

	key, ok := s.jobKey(job)
	if !ok {
		return fmt.Errorf("key %q is revoked or gone; the transfer cannot run", job.KeyID)
	}
	fromDrive := req.From.driveOf(key.EffectiveDefaultDrive())
	toDrive := req.To.driveOf(key.EffectiveDefaultDrive())
	fromRoot, e := s.checkJobDrive(nil, key, fromDrive, auth.PermTransfer, auth.PermRead)
	if e != nil {
		return fmt.Errorf("%s", e.Message)
	}
	toRoot, e := s.checkJobDrive(nil, key, toDrive, auth.PermTransfer, auth.PermWrite)
	if e != nil {
		return fmt.Errorf("%s", e.Message)
	}
	srcCall := &call{r: &http.Request{}, key: key, drive: fromDrive, root: fromRoot}
	dstCall := &call{r: &http.Request{}, key: key, drive: toDrive, root: toRoot}
	_, fromInternal, fromVisible, e := s.resolve(srcCall, req.From.Path)
	if e != nil {
		return fmt.Errorf("%s", e.Message)
	}
	_, toInternal, toVisible, e := s.resolve(dstCall, req.To.Path)
	if e != nil {
		return fmt.Errorf("%s", e.Message)
	}

	// A restarted job may already carry submitted OpenList tasks; the source
	// can legitimately be gone by then (completed move).
	resumeHasTasks := resumed && len(taskIDsOf(job)) > 0 && taskTypeOf(job) != ""
	if !resumeHasTasks {
		if _, err := s.fsGet(ctx, fromInternal); err != nil {
			if isNotFoundError(err) {
				return fmt.Errorf("source %s no longer exists", fromVisible)
			}
			return err
		}
	}

	// Effective target (same normalization as the POST handler).
	dstDir, dstBase := parentOf(toInternal)
	targetInternal, targetVisible := toInternal, toVisible
	if extra.EffectiveTo != "" {
		// Rename was resolved at POST time; rebuild from the visible form.
		effFull := joinRoot(toRoot, extra.EffectiveTo)
		targetInternal = s.toInternal(toDrive, effFull)
		targetVisible = extra.EffectiveTo
		dstDir, _ = parentOf(targetInternal)
		dstBase = path.Base(targetInternal)
	} else if info, err := s.fsGet(ctx, toInternal); err == nil && info.IsDir {
		dstDir = toInternal
		dstBase = path.Base(fromInternal)
		targetInternal = path.Join(toInternal, path.Base(fromInternal))
		targetVisible = path.Join(toVisible, path.Base(fromVisible))
	}

	// Overwrite: remove the existing target first (through the key trash when
	// enabled, so the destructive step stays reversible). Skipped on a task
	// resume (the copy already finished; see below).
	if !resumeHasTasks {
		if _, err := s.fsGet(ctx, targetInternal); err == nil && req.OnConflict == "overwrite" {
			if res := s.trashOrRemove(ctx, key, toDrive, toRoot, targetInternal, targetVisible); res != "" {
				return fmt.Errorf("overwrite failed: %s", res)
			}
		}
	}

	srcDir, srcBase := parentOf(fromInternal)
	if srcDir == dstDir && fromDrive == toDrive && srcBase == dstBase && req.Mode == "move" {
		s.jobStateIfNotCanceled(job.ID, store.JobSucceeded, 100, map[string]any{"from": fromVisible, "to": targetVisible}, "")
		return nil
	}

	apiPath, taskType := "/api/fs/copy", taskCopy
	if req.Mode == "move" {
		apiPath, taskType = "/api/fs/move", taskMove
	}

	// Restart resume: the OpenList task was already submitted before the
	// restart; poll it instead of submitting again (and skip the destructive
	// overwrite step, which would trash the finished copy).
	resumedTasks := false
	if resumeHasTasks {
		{
			ids, typ := taskIDsOf(job), taskTypeOf(job)
			if terr := s.trackOpenListTasks(ctx, job, typ, ids); terr != nil {
				if errors.Is(terr, errJobCanceled) {
					s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, nil, "canceled")
					return nil
				}
				return terr
			}
			if s.jobCanceled(job.ID) {
				return nil
			}
			resumedTasks = true
		}
	}
	if !resumedTasks {
		tids, err := s.fsSubmitCopyMove(ctx, apiPath, srcDir, dstDir, []string{srcBase})
		if err != nil {
			return err
		}
		res := s.rememberTasks(job.ID, taskType, tids)
		if terr := s.trackOpenListTasks(ctx, job, taskType, tids); terr != nil {
			if errors.Is(terr, errJobCanceled) {
				s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, res, "canceled")
				return nil
			}
			return terr
		}
		if s.jobCanceled(job.ID) {
			return nil
		}
	}

	// The copy traveled under the source basename; rename at the destination
	// when the effective target differs. A missing landed path is fine when
	// the target already exists (the rename ran before, e.g. by a runner from
	// before a restart).
	landed := path.Join(dstDir, srcBase)
	if landed != targetInternal {
		_, landedErr := s.fsGet(ctx, landed)
		if landedErr == nil {
			if _, err := s.fsGet(ctx, targetInternal); err == nil {
				// Someone created the final name meanwhile; fall back to a suffix.
				for n := 1; n <= 999; n++ {
					cand := renameCandidate(targetInternal, n)
					if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
						targetInternal = cand
						targetVisible = renameCandidate(targetVisible, n)
						break
					}
				}
			}
			if err := s.fsRename(ctx, landed, path.Base(targetInternal)); err != nil {
				return fmt.Errorf("copied but final rename failed: %s", upstreamMessage(err))
			}
		} else if !isNotFoundError(landedErr) {
			return landedErr
		} else if _, err := s.fsGet(ctx, targetInternal); err != nil {
			return fmt.Errorf("transfer reported success but neither %s nor %s exists", landed, targetInternal)
		}
	}

	info, err := s.fsGet(ctx, targetInternal)
	if err != nil {
		return fmt.Errorf("transfer reported success but %s is missing", targetVisible)
	}
	s.jobStateIfNotCanceled(job.ID, store.JobSucceeded, 100, map[string]any{
		"from": fromVisible, "to": targetVisible, "bytes": info.Size, "is_dir": info.IsDir,
	}, "")
	return nil
}

// jobKey reloads the runtime key for a job runner; revoked/expired keys stop
// the job.
func (s *Server) jobKey(job store.Job) (*auth.Key, bool) {
	row, err := s.keys.Get(job.KeyID)
	if err != nil || row.Revoked {
		return nil, false
	}
	if row.ExpiresAt != nil && time.Now().After(*row.ExpiresAt) {
		return nil, false
	}
	rk, ok := s.keys.Authenticator().ByID(row.ID)
	if rk == nil || !ok {
		return nil, false
	}
	return rk, true
}

// trashOrRemove deletes internalPermanently or through the key trash,
// returning "" on success or an error message.
func (s *Server) trashOrRemove(ctx context.Context, key *auth.Key, drive, root, internal, visible string) string {
	dir, base := parentOf(internal)
	if !key.TrashOn {
		if err := s.fsRemove(ctx, dir, []string{base}); err != nil {
			return upstreamMessage(err)
		}
		return ""
	}
	// Trash move (same logic as deleteOne, without a live request call).
	trashFull := s.trashPathFor(root, visible)
	trashInternal := s.toInternal(drive, trashFull)
	tDir, _ := parentOf(trashInternal)
	if err := s.mkdirAll(ctx, tDir); err != nil {
		return upstreamMessage(err)
	}
	actualTrash := trashFull
	if _, err := s.fsGet(ctx, trashInternal); err == nil {
		for n := 1; n <= 999; n++ {
			cand := renameCandidate(trashInternal, n)
			if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
				actualTrash = renameCandidate(trashFull, n)
				trashInternal = cand
				break
			}
		}
	}
	_, tBase := parentOf(trashInternal)
	if err := s.moveAcross(ctx, internal, parentOf0(trashInternal), tBase); err != nil {
		return upstreamMessage(err)
	}
	days := key.TrashDays
	if days <= 0 {
		days = 7
	}
	info, _ := s.fsGet(ctx, trashInternal)
	isDir, size := false, int64(0)
	if info.Name != "" {
		isDir, size = info.IsDir, info.Size
	}
	_, _ = s.st.InsertTrashItem(store.TrashItem{
		KeyID: key.ID, Drive: drive, OrigPath: visible, TrashPath: actualTrash,
		IsDir: isDir, Size: size, PurgeAfter: time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour),
	})
	return ""
}

func upstreamMessage(err error) string {
	if ue, ok := err.(*upstreamError); ok {
		return ue.Message
	}
	return err.Error()
}

// ---- POST /v1/fetch ----

func (s *Server) handleFetchCreate(c *call) {
	var body fetchRequest
	if !decodeBody(c, &body) {
		return
	}
	if body.URL == "" || body.Path == "" {
		c.fail(errInvalidRequest("fields `url` and `path` are required"))
		return
	}
	u, err := url.Parse(body.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.fail(errInvalidRequest("`url` must be an absolute http(s) URL"))
		return
	}
	var ok bool
	if body.OnConflict, ok = normalizeOnConflict(c, body.OnConflict); !ok {
		return
	}
	drive := body.Drive
	if drive == "" {
		drive = c.key.EffectiveDefaultDrive()
	}
	if drive == "" {
		c.fail(errInvalidRequest("field `drive` is required (this key has no default drive)"))
		return
	}
	if _, e := s.checkJobDrive(c, c.key, drive, auth.PermFetch, auth.PermWrite); e != nil {
		c.fail(e)
		return
	}
	grant, _ := c.key.DriveGrant(drive)
	sub := s.subCall(c, drive, grant.Root)
	_, internal, visible, e := s.resolve(sub, body.Path)
	if e != nil {
		c.fail(e)
		return
	}

	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	// SimpleHttp stores the URL's basename into the parent directory; a
	// different target name is applied by a rename after the download.
	urlBase := path.Base(u.Path)
	if strings.HasPrefix(urlBase, ".") || urlBase == "/" || urlBase == "" {
		c.fail(errInvalidRequest("the URL has no usable file name; pass an explicit `path` ending in a file name (a rename is applied automatically)"))
		return
	}
	parent, wantBase := parentOf(internal)
	landed := path.Join(parent, urlBase)
	targetInternal, targetVisible := internal, visible
	effectiveTo := visible
	if wantBase != urlBase {
		targetInternal, targetVisible = internal, visible // rename to the wanted name afterwards
	}
	// Conflict on the name that will actually land.
	conflictAt := landed
	if wantBase != urlBase {
		conflictAt = targetInternal
	}
	if _, err := s.fsGet(ctx, conflictAt); err == nil {
		switch body.OnConflict {
		case "fail":
			c.fail(errAlreadyExists(targetVisible))
			return
		case "rename":
			for n := 1; n <= 999; n++ {
				cand := renameCandidate(conflictAt, n)
				if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
					if wantBase != urlBase {
						targetInternal = cand
					}
					effectiveTo = renameCandidate(targetVisible, n)
					break
				} else if err != nil {
					c.fail(fsError(err))
					return
				}
			}
		}
	}

	if body.DryRun {
		c.ok(map[string]any{
			"dry_run": true,
			"would_affect": map[string]any{
				"files": 1,
				"dirs":  0,
				"bytes": 0, // unknown before the download
			},
			"items":     []string{effectiveTo},
			"truncated": false,
		})
		return
	}

	if !s.jobConcurrencyGuard(c) {
		return
	}

	reqJSON, _ := json.Marshal(map[string]any{
		"url": body.URL, "drive": drive, "path": body.Path,
		"on_conflict": body.OnConflict, "effective_to": effectiveTo,
	})
	job := store.Job{
		ID: newJobID(), KeyID: c.key.ID, Kind: store.JobKindFetch,
		State: store.JobQueued, Request: string(reqJSON), Result: "{}",
	}
	if err := s.st.InsertJob(job); err != nil {
		c.fail(errUpstream("job create failed"))
		return
	}
	go s.runJob(context.Background(), job, false)
	writeJSON(c.w, http.StatusAccepted, map[string]any{"job": s.jobWithProgress(job)})
}

// runFetchJob drives the offline download to completion.
func (s *Server) runFetchJob(ctx context.Context, job store.Job, resumed bool) error {
	var req fetchRequest
	var extra struct {
		EffectiveTo string `json:"effective_to"`
	}
	raw := map[string]any{}
	_ = json.Unmarshal([]byte(job.Request), &raw)
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("corrupt job request: %w", err)
	}
	_ = json.Unmarshal(b, &extra)

	key, ok := s.jobKey(job)
	if !ok {
		return fmt.Errorf("key %q is revoked or gone; the fetch cannot run", job.KeyID)
	}
	if _, e := s.checkJobDrive(nil, key, req.Drive, auth.PermFetch, auth.PermWrite); e != nil {
		return fmt.Errorf("%s", e.Message)
	}
	grant, _ := key.DriveGrant(req.Drive)
	sub := &call{r: &http.Request{}, key: key, drive: req.Drive, root: grant.Root}
	_, internal, visible, e := s.resolve(sub, req.Path)
	if e != nil {
		return fmt.Errorf("%s", e.Message)
	}
	parent, _ := parentOf(internal)

	u, err := url.Parse(req.URL)
	if err != nil {
		return fmt.Errorf("corrupt fetch url")
	}
	urlBase := path.Base(u.Path)

	res := jobResult(job)
	var tids []string
	if ids, ok := res["task_ids"].([]any); ok {
		for _, v := range ids {
			if str, ok := v.(string); ok {
				tids = append(tids, str)
			}
		}
	}
	if len(tids) == 0 {
		tids, err = s.fsOfflineDownload(ctx, parent, req.URL)
		if err != nil {
			return err
		}
		res = s.rememberTasks(job.ID, taskOffline, tids)
	}
	if terr := s.trackOpenListTasks(ctx, job, taskOffline, tids); terr != nil {
		if errors.Is(terr, errJobCanceled) {
			s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, res, "canceled")
			return nil
		}
		return terr
	}
	if s.jobCanceled(job.ID) {
		return nil
	}

	// Rename the landed URL-basename to the wanted/effective target. The
	// live OpenList SimpleHttp tool may already have stored the file under
	// the final name (it honors Content-Disposition), so a missing
	// URL-basename file is fine whenever the target exists.
	targetInternal, targetVisible := internal, visible
	if extra.EffectiveTo != "" {
		targetInternal = s.toInternal(req.Drive, joinRoot(grant.Root, extra.EffectiveTo))
		targetVisible = extra.EffectiveTo
	}
	landed := path.Join(parent, urlBase)
	if landed != targetInternal {
		_, landedErr := s.fsGet(ctx, landed)
		if landedErr == nil {
			if _, err := s.fsGet(ctx, targetInternal); err == nil {
				for n := 1; n <= 999; n++ {
					cand := renameCandidate(targetInternal, n)
					if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
						targetInternal = cand
						targetVisible = renameCandidate(targetVisible, n)
						break
					}
				}
			}
			if err := s.fsRename(ctx, landed, path.Base(targetInternal)); err != nil {
				return fmt.Errorf("downloaded but final rename failed: %s", upstreamMessage(err))
			}
		} else if !isNotFoundError(landedErr) {
			return landedErr
		} else if _, err := s.fsGet(ctx, targetInternal); err != nil {
			return fmt.Errorf("fetch reported success but neither %s nor %s exists", landed, targetInternal)
		}
	}
	info, err := s.fsGet(ctx, targetInternal)
	if err != nil {
		return fmt.Errorf("fetch reported success but %s is missing", targetVisible)
	}
	_ = s.st.RecordUpload(store.Upload{
		Drive: req.Drive, Path: joinRoot(grant.Root, targetVisible),
		Size: info.Size, KeyID: key.ID, Mtime: info.Modified,
		// checksums unknown: gateway did not see the bytes
	})
	s.jobStateIfNotCanceled(job.ID, store.JobSucceeded, 100, map[string]any{
		"url": req.URL, "path": targetVisible, "bytes": info.Size,
	}, "")
	return nil
}

// ---- POST /v1/batch ----

func (s *Server) handleBatch(c *call) {
	var body batchRequest
	if !decodeBody(c, &body) {
		return
	}
	if len(body.Ops) == 0 {
		c.fail(errInvalidRequest("field `ops` (array) is required"))
		return
	}
	if len(body.Ops) > 100 {
		c.fail(errInvalidRequest("at most 100 ops per batch"))
		return
	}
	def := c.key.EffectiveDefaultDrive()
	needsJob := false
	driveSet := map[string]bool{}
	for i := range body.Ops {
		op := &body.Ops[i]
		switch op.Op {
		case "mkdir", "delete", "move", "copy":
		default:
			c.fail(errInvalidRequest("ops[" + strconv.Itoa(i) + "].op must be mkdir, delete, move or copy"))
			return
		}
		var conflictOK bool
		op.OnConflict, conflictOK = normalizeOnConflict(c, op.OnConflict)
		if !conflictOK {
			return
		}
		switch op.Op {
		case "mkdir", "delete":
			d := op.Drive
			if d == "" {
				d = def
			}
			if d != "" {
				driveSet[d] = true
			}
		case "move", "copy":
			if d := op.From.driveOf(def); d != "" {
				driveSet[d] = true
			}
			if d := op.To.driveOf(def); d != "" {
				driveSet[d] = true
			}
			if op.Op == "copy" && op.From.driveOf(def) != op.To.driveOf(def) {
				needsJob = true // cross-drive copies are long-running
			}
		}
	}

	// Submit-time drive gate (the runner re-validates every op): identity,
	// grant and storage guard for every drive any op touches, so an
	// unavailable drive (e.g. an unconfirmed replacement storage) fails the
	// whole batch up front instead of half-executing.
	for d := range driveSet {
		if _, e := s.checkJobDrive(c, c.key, d); e != nil {
			c.fail(e)
			return
		}
	}

	if body.DryRun {
		s.batchDryRun(c, body, def)
		return
	}

	if !needsJob {
		results := s.batchRunSync(c, body, def)
		c.ok(map[string]any{"results": results, "job": nil})
		return
	}

	if !s.jobConcurrencyGuard(c) {
		return
	}
	reqJSON, _ := json.Marshal(body)
	job := store.Job{
		ID: newJobID(), KeyID: c.key.ID, Kind: store.JobKindBatch,
		State: store.JobQueued, Request: string(reqJSON), Result: `{"results":[]}`,
	}
	if err := s.st.InsertJob(job); err != nil {
		c.fail(errUpstream("job create failed"))
		return
	}
	go s.runJob(context.Background(), job, false)
	writeJSON(c.w, http.StatusAccepted, map[string]any{"job": s.jobWithProgress(job)})
}

// batchDryRun summarizes what each op would affect without touching anything.
func (s *Server) batchDryRun(c *call, body batchRequest, def string) {
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	results := make([]map[string]any, 0, len(body.Ops))
	for _, op := range body.Ops {
		entry := map[string]any{"op": op.Op, "status": "dry_run"}
		switch op.Op {
		case "mkdir":
			entry["path"] = op.Path
			entry["would_affect"] = map[string]any{"files": 0, "dirs": 1, "bytes": 0}
			entry["items"] = []string{op.Path}
		case "delete":
			total := map[string]int64{"files": 0, "dirs": 0, "bytes": 0}
			var items []string
			for _, p := range op.Paths {
				drive := op.Drive
				if drive == "" {
					drive = def
				}
				if root, grantOK := c.key.DriveGrant(drive); grantOK {
					sub := s.subCall(c, drive, root.Root)
					if stats, e := s.walkPath(ctx, sub, p, walkDefaultMaxEntries, time.Now().Add(20*time.Second)); e == nil {
						total["files"] += stats.Files
						total["dirs"] += stats.Dirs
						total["bytes"] += stats.Bytes
						items = append(items, stats.Items...)
					}
				}
			}
			if len(items) > 200 {
				items = items[:200]
			}
			entry["would_affect"] = map[string]any{"files": total["files"], "dirs": total["dirs"], "bytes": total["bytes"]}
			entry["items"] = items
		case "move", "copy":
			drive := op.From.driveOf(def)
			entry["from"] = op.From
			entry["to"] = op.To
			if root, grantOK := c.key.DriveGrant(drive); grantOK {
				sub := s.subCall(c, drive, root.Root)
				if stats, e := s.walkPath(ctx, sub, op.From.Path, walkDefaultMaxEntries, time.Now().Add(20*time.Second)); e == nil {
					items := stats.Items
					if len(items) > 200 {
						items = items[:200]
					}
					entry["would_affect"] = map[string]any{"files": stats.Files, "dirs": stats.Dirs, "bytes": stats.Bytes}
					entry["items"] = items
				} else {
					entry["error"] = e.Message
				}
			}
		}
		results = append(results, entry)
	}
	c.ok(map[string]any{"dry_run": true, "results": results})
}

// runBatchJob executes batch ops sequentially, appending per-op results.
func (s *Server) runBatchJob(ctx context.Context, job store.Job, resumed bool) error {
	var body batchRequest
	if err := json.Unmarshal([]byte(job.Request), &body); err != nil {
		return fmt.Errorf("corrupt job request: %w", err)
	}
	key, ok := s.jobKey(job)
	if !ok {
		return fmt.Errorf("key %q is revoked or gone; the batch cannot run", job.KeyID)
	}
	def := key.EffectiveDefaultDrive()

	results := []map[string]any{}
	if resumed {
		res := jobResult(job)
		if existing, ok := res["results"].([]any); ok {
			for _, v := range existing {
				if m, ok := v.(map[string]any); ok {
					results = append(results, m)
				}
			}
		}
	}
	persist := func() {
		j, err := s.st.GetJob(job.ID)
		if err != nil {
			return
		}
		res := jobResult(j)
		res["results"] = results
		_ = s.st.PatchJobResult(job.ID, marshalOr(res, "{}"))
	}

	for i := len(results); i < len(body.Ops); i++ {
		if ctx.Err() != nil || s.jobCanceled(job.ID) {
			persist()
			s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, map[string]any{"results": results}, "canceled")
			return nil
		}
		op := body.Ops[i]
		res := s.batchExecOp(ctx, key, job.ID, op, def)
		results = append(results, res)
		persist()
		if st, _ := res["status"].(string); st == "error" && res["error"] == "canceled" {
			s.jobStateIfNotCanceled(job.ID, store.JobCanceled, 0, map[string]any{"results": results}, "canceled")
			return nil
		}
	}
	s.jobStateIfNotCanceled(job.ID, store.JobSucceeded, 100, map[string]any{"results": results}, "")
	return nil
}

// batchExecOp runs one op with a fresh pseudo-call. jobID is empty for
// synchronous batches (no cancellation source); otherwise cancellation is
// checked against the job row between steps.
func (s *Server) batchExecOp(ctx context.Context, key *auth.Key, jobID string, op batchOp, def string) map[string]any {
	canceled := func() map[string]any {
		return map[string]any{"op": op.Op, "status": "error", "error": "canceled"}
	}
	isCanceled := func() bool { return jobID != "" && s.jobCanceled(jobID) }
	c := &call{r: &http.Request{}, key: key}
	switch op.Op {
	case "mkdir":
		drive := op.Drive
		if drive == "" {
			drive = def
		}
		root, e := s.checkJobDrive(nil, key, drive, auth.PermWrite)
		if e != nil {
			return map[string]any{"op": "mkdir", "status": "error", "error": e.Message}
		}
		c.drive, c.root = drive, root
		_, internal, visible, e := s.resolve(c, op.Path)
		if e != nil {
			return map[string]any{"op": "mkdir", "status": "error", "error": e.Message}
		}
		if _, err := s.fsGet(ctx, internal); err == nil {
			return map[string]any{"op": "mkdir", "path": visible, "status": "ok", "created": false}
		} else if !isNotFoundError(err) {
			return map[string]any{"op": "mkdir", "status": "error", "error": upstreamMessage(err)}
		}
		if err := s.mkdirAll(ctx, internal); err != nil {
			return map[string]any{"op": "mkdir", "status": "error", "error": upstreamMessage(err)}
		}
		return map[string]any{"op": "mkdir", "path": visible, "status": "ok", "created": true}

	case "delete":
		drive := op.Drive
		if drive == "" {
			drive = def
		}
		root, e := s.checkJobDrive(nil, key, drive, auth.PermDelete)
		if e != nil {
			return map[string]any{"op": "delete", "status": "error", "error": e.Message}
		}
		c.drive, c.root = drive, root
		out := map[string]any{"op": "delete", "status": "ok", "results": []map[string]any{}}
		perPath := []map[string]any{}
		for _, p := range op.Paths {
			if isCanceled() {
				return canceled()
			}
			_, internal, visible, e := s.resolve(c, p)
			if e != nil {
				perPath = append(perPath, map[string]any{"path": p, "status": "error", "error": e.Message})
				continue
			}
			info, err := s.fsGet(ctx, internal)
			if err != nil {
				if isNotFoundError(err) {
					perPath = append(perPath, map[string]any{"path": visible, "status": "already_absent"})
					continue
				}
				perPath = append(perPath, map[string]any{"path": visible, "status": "error", "error": upstreamMessage(err)})
				continue
			}
			if !key.TrashOn {
				dir, base := parentOf(internal)
				if err := s.fsRemove(ctx, dir, []string{base}); err != nil {
					perPath = append(perPath, map[string]any{"path": visible, "status": "error", "error": upstreamMessage(err)})
					continue
				}
				perPath = append(perPath, map[string]any{"path": visible, "status": "deleted"})
				continue
			}
			trashFull := s.trashPathFor(root, visible)
			trashInternal := s.toInternal(drive, trashFull)
			tDir, _ := parentOf(trashInternal)
			if err := s.mkdirAll(ctx, tDir); err != nil {
				perPath = append(perPath, map[string]any{"path": visible, "status": "error", "error": upstreamMessage(err)})
				continue
			}
			actual := trashFull
			if _, err := s.fsGet(ctx, trashInternal); err == nil {
				for n := 1; n <= 999; n++ {
					cand := renameCandidate(trashInternal, n)
					if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
						actual = renameCandidate(trashFull, n)
						trashInternal = cand
						break
					}
				}
			}
			_, tBase := parentOf(trashInternal)
			if err := s.moveAcross(ctx, internal, parentOf0(trashInternal), tBase); err != nil {
				perPath = append(perPath, map[string]any{"path": visible, "status": "error", "error": upstreamMessage(err)})
				continue
			}
			days := key.TrashDays
			if days <= 0 {
				days = 7
			}
			id, _ := s.st.InsertTrashItem(store.TrashItem{
				KeyID: key.ID, Drive: drive, OrigPath: visible, TrashPath: actual,
				IsDir: info.IsDir, Size: info.Size,
				PurgeAfter: time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour),
			})
			perPath = append(perPath, map[string]any{"path": visible, "status": "trashed", "trash_id": id})
		}
		out["results"] = perPath
		return out

	case "move", "copy":
		fromDrive := op.From.driveOf(def)
		toDrive := op.To.driveOf(def)
		perms := []auth.Permission{auth.PermWrite, auth.PermRead}
		if fromDrive != toDrive {
			perms = append(perms, auth.PermTransfer)
		}
		fromRoot, e := s.checkJobDrive(nil, key, fromDrive, perms...)
		if e != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": e.Message}
		}
		toRoot, e := s.checkJobDrive(nil, key, toDrive, auth.PermWrite)
		if e != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": e.Message}
		}
		src := &call{r: &http.Request{}, key: key, drive: fromDrive, root: fromRoot}
		dst := &call{r: &http.Request{}, key: key, drive: toDrive, root: toRoot}
		_, fromInternal, fromVisible, e := s.resolve(src, op.From.Path)
		if e != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": e.Message}
		}
		_, toInternal, toVisible, e := s.resolve(dst, op.To.Path)
		if e != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": e.Message}
		}
		if _, err := s.fsGet(ctx, fromInternal); err != nil {
			if isNotFoundError(err) {
				return map[string]any{"op": op.Op, "status": "error", "error": "path_not_found: " + fromVisible}
			}
			return map[string]any{"op": op.Op, "status": "error", "error": upstreamMessage(err)}
		}

		if fromDrive == toDrive && op.Op == "move" {
			// Same-drive move is fast; reuse the synchronous helper.
			target := toInternal
			if info, err := s.fsGet(ctx, toInternal); err == nil && info.IsDir {
				target = path.Join(toInternal, path.Base(fromInternal))
			}
			if _, err := s.fsGet(ctx, target); err == nil {
				switch op.OnConflict {
				case "fail":
					return map[string]any{"op": "move", "status": "error", "error": "already_exists"}
				case "overwrite":
					if msg := s.trashOrRemove(ctx, key, fromDrive, fromRoot, target, toVisible); msg != "" {
						return map[string]any{"op": "move", "status": "error", "error": msg}
					}
				}
			}
			toDir, toBase := parentOf(target)
			if toDir != "/" {
				if err := s.mkdirAll(ctx, toDir); err != nil {
					return map[string]any{"op": "move", "status": "error", "error": upstreamMessage(err)}
				}
			}
			if err := s.moveAcross(ctx, fromInternal, toDir, toBase); err != nil {
				return map[string]any{"op": "move", "status": "error", "error": upstreamMessage(err)}
			}
			return map[string]any{"op": "move", "status": "ok", "from": fromVisible, "to": toVisible}
		}

		// copy (same- or cross-drive): submit and wait inline within the job.
		dstDir, _ := parentOf(toInternal)
		targetInternal, targetVisible := toInternal, toVisible
		if info, err := s.fsGet(ctx, toInternal); err == nil && info.IsDir {
			dstDir = toInternal
			targetInternal = path.Join(toInternal, path.Base(fromInternal))
			targetVisible = path.Join(toVisible, path.Base(fromVisible))
		}
		if _, err := s.fsGet(ctx, targetInternal); err == nil {
			switch op.OnConflict {
			case "fail":
				return map[string]any{"op": "copy", "status": "error", "error": "already_exists: " + targetVisible}
			case "overwrite":
				if msg := s.trashOrRemove(ctx, key, toDrive, toRoot, targetInternal, targetVisible); msg != "" {
					return map[string]any{"op": "copy", "status": "error", "error": msg}
				}
			case "rename":
				for n := 1; n <= 999; n++ {
					cand := renameCandidate(targetInternal, n)
					if _, err := s.fsGet(ctx, cand); isNotFoundError(err) {
						targetInternal = cand
						targetVisible = renameCandidate(targetVisible, n)
						break
					}
				}
			}
		}
		srcDir, srcBase := parentOf(fromInternal)
		apiPath, taskType := "/api/fs/copy", taskCopy
		if op.Op == "move" {
			apiPath, taskType = "/api/fs/move", taskMove
		}
		tids, err := s.fsSubmitCopyMove(ctx, apiPath, srcDir, dstDir, []string{srcBase})
		if err != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": upstreamMessage(err)}
		}
		if isCanceled() {
			return canceled()
		}
		if terr := s.trackOpenListTasks(ctx, store.Job{}, taskType, tids); terr != nil {
			return map[string]any{"op": op.Op, "status": "error", "error": terr.Error()}
		}
		landed := path.Join(dstDir, srcBase)
		if landed != targetInternal {
			if err := s.fsRename(ctx, landed, path.Base(targetInternal)); err != nil {
				return map[string]any{"op": op.Op, "status": "error", "error": "copied but rename failed: " + upstreamMessage(err)}
			}
		}
		return map[string]any{"op": op.Op, "status": "ok", "from": fromVisible, "to": targetVisible}
	}
	return map[string]any{"op": op.Op, "status": "error", "error": "unknown op"}
}

// batchRunSync executes ops that never need long-running tasks inline.
func (s *Server) batchRunSync(c *call, body batchRequest, def string) []map[string]any {
	ctx, cancel := context.WithTimeout(c.r.Context(), 5*time.Minute)
	defer cancel()
	results := make([]map[string]any, 0, len(body.Ops))
	for i, op := range body.Ops {
		if ctx.Err() != nil {
			results = append(results, map[string]any{"op": op.Op, "index": i, "status": "error", "error": "batch timed out"})
			continue
		}
		res := s.batchExecOp(ctx, c.key, "", op, def)
		res["index"] = i
		results = append(results, res)
	}
	return results
}

// ---- GET /v1/jobs, GET /v1/jobs/{id}?wait=N, POST /v1/jobs/{id}/cancel ----

func (s *Server) handleJobsList(c *call) {
	q := c.r.URL.Query()
	state := q.Get("state")
	switch state {
	case "":
	case store.JobQueued, store.JobRunning, store.JobSucceeded, store.JobFailed, store.JobCanceled:
	default:
		c.fail(errInvalidRequest("state must be one of queued, running, succeeded, failed, canceled"))
		return
	}
	limit, e := parseInt(q.Get, "limit", 50, 200)
	if e != nil {
		c.fail(e)
		return
	}
	jobs, err := s.st.ListJobs(c.key.ID, state, limit)
	if err != nil {
		c.fail(errUpstream("job listing failed"))
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView(j))
	}
	c.ok(map[string]any{"jobs": out})
}

// runJobItem routes GET /v1/jobs/{id} and POST /v1/jobs/{id}/cancel.
func (s *Server) runJobItem(c *call, rest string) {
	id, action := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	job, err := s.st.GetJob(id)
	if err != nil || job.KeyID != c.key.ID {
		c.fail(errNotFoundRoute().hint("list your jobs with GET /v1/jobs"))
		return
	}
	switch {
	case action == "" && c.r.Method == http.MethodGet:
		c.op = "job_get"
		s.handleJobGet(c, job)
	case action == "cancel" && c.r.Method == http.MethodPost:
		c.op = "job_cancel"
		s.handleJobCancel(c, job)
	default:
		c.fail(errNotFoundRoute())
	}
}

// handleJobGet implements the long poll: it returns immediately when the job
// is terminal, wakes on a state change, or answers with the current state
// after wait seconds (§4.10.4).
func (s *Server) handleJobGet(c *call, job store.Job) {
	wait := time.Duration(0)
	if raw := c.r.URL.Query().Get("wait"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			c.fail(errInvalidRequest("wait must be a non-negative number of seconds"))
			return
		}
		wait = time.Duration(n) * time.Second
		if wait > jobWaitCap {
			wait = jobWaitCap
		}
	}
	current := job
	if wait > 0 && !store.JobTerminal(current.State) {
		deadline := time.Now().Add(wait)
		for {
			ch := s.jobman.watch(current.ID)
			fresh, err := s.st.GetJob(current.ID)
			if err != nil {
				s.jobman.unwatch(current.ID, ch)
				c.fail(errUpstream("job query failed"))
				return
			}
			if store.JobTerminal(fresh.State) || fresh.State != current.State {
				s.jobman.unwatch(current.ID, ch)
				current = fresh
				break
			}
			select {
			case <-ch:
			case <-c.r.Context().Done():
				s.jobman.unwatch(current.ID, ch)
				current = fresh
			case <-time.After(time.Until(deadline)):
				s.jobman.unwatch(current.ID, ch)
				current = fresh
			}
			if store.JobTerminal(current.State) || current.State != job.State || c.r.Context().Err() != nil || !time.Now().Before(deadline) {
				if !time.Now().Before(deadline) {
					fresh2, err := s.st.GetJob(current.ID)
					if err == nil {
						current = fresh2
					}
				}
				break
			}
		}
	}
	c.ok(map[string]any{"job": jobView(current)})
}

// handleJobCancel cancels a queued/running job where possible: OpenList tasks
// are canceled through the task API, batch jobs stop between ops.
func (s *Server) handleJobCancel(c *call, job store.Job) {
	if store.JobTerminal(job.State) {
		c.fail(errInvalidRequest("job is already " + job.State))
		return
	}
	// Ask OpenList to cancel the tracked tasks (best effort).
	res := jobResult(job)
	taskType, _ := res["task_type"].(string)
	if ids, ok := res["task_ids"].([]any); ok && taskType != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, v := range ids {
			if tid, ok := v.(string); ok {
				_ = s.taskCancel(ctx, taskType, tid)
			}
		}
		cancel()
	}
	s.jobState(job.ID, store.JobCanceled, job.Progress, res, "canceled by "+c.key.ID)
	c.ok(map[string]any{"job": jobView(mustJob(s, job.ID)), "canceled": true})
}

func mustJob(s *Server, id string) store.Job {
	j, err := s.st.GetJob(id)
	if err != nil {
		return store.Job{ID: id}
	}
	return j
}

// sortJobsByCreated is a helper for deterministic rendering.
func sortJobsByCreated(jobs []store.Job) {
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
}

// taskIDsOf extracts the persisted OpenList task ids of a job.
func taskIDsOf(job store.Job) []string {
	res := jobResult(job)
	ids, _ := res["task_ids"].([]any)
	out := make([]string, 0, len(ids))
	for _, v := range ids {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// taskTypeOf extracts the persisted OpenList task type of a job.
func taskTypeOf(job store.Job) string {
	res := jobResult(job)
	t, _ := res["task_type"].(string)
	return t
}
