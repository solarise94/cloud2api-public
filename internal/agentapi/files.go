package agentapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"cloud2api/internal/store"
)

// crc64Table is the ECMA table; Go's crc64 with this table yields the
// CRC-64/XZ check value (verified: "123456789" -> 11051210869376104954),
// matching Gokapi and Tencent COS.
var crc64Table = crc64.MakeTable(crc64.ECMA)

// listEntry is one typed directory entry (no OpenList fields).
type listEntry struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	IsDir    bool           `json:"is_dir"`
	Size     int64          `json:"size"`
	Modified string         `json:"modified"`
	Hashes   map[string]any `json:"hashes"`
}

func (s *Server) handleList(c *call) {
	q := c.r.URL.Query()
	logical := q.Get("path")
	if logical == "" {
		logical = "/"
	}
	page, e := parseInt(q.Get, "page", 1, 1_000_000)
	if e != nil {
		c.fail(e)
		return
	}
	perPage, e := parseInt(q.Get, "per_page", 200, 1000)
	if e != nil {
		c.fail(e)
		return
	}
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	entries, total, err := s.fsList(ctx, internal, page, perPage)
	if err != nil {
		c.fail(fsError(err))
		return
	}
	items := make([]listEntry, 0, len(entries))
	for _, en := range entries {
		// The trash directory never shows up in normal listings (§4.10.3);
		// the same goes for the Gokapi relay directory (§4.7).
		if path.Clean(visible) == "/" && (en.Name == ".c2a-trash" || en.Name == relayDirName) {
			continue
		}
		items = append(items, listEntry{
			Name:     en.Name,
			Path:     path.Join(path.Clean(visible), en.Name),
			IsDir:    en.IsDir,
			Size:     en.Size,
			Modified: en.Modified,
			Hashes:   map[string]any{},
		})
	}
	c.ok(map[string]any{
		"items":    items,
		"total":    total,
		"page":     page,
		"per_page": perPage,
	})
}

func (s *Server) handleStat(c *call) {
	logical := c.r.URL.Query().Get("path")
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			ae := errPathNotFound(visible)
			ae.Suggestions = s.suggestionsFor(ctx, c, visible)
			c.fail(ae)
			return
		}
		c.fail(fsError(err))
		return
	}
	resp := map[string]any{
		"path":     visible,
		"name":     info.Name,
		"is_dir":   info.IsDir,
		"size":     info.Size,
		"modified": info.Modified,
		"hashes":   map[string]any{},
	}
	// Attach the recorded upload checksums when size and mtime still match
	// the record (§4.4), so agents can verify without re-downloading.
	drive := c.drive
	full := joinRoot(c.root, visible)
	if rec, err := s.st.LatestUploadFor(drive, full); err == nil {
		if rec.Size == info.Size && (rec.Mtime == "" || rec.Mtime == info.Modified) {
			resp["hashes"] = map[string]any{
				"crc64":  rec.CRC64,
				"sha256": rec.SHA256,
			}
			resp["hashes_source"] = "upload_record"
		}
	}
	c.ok(resp)
}

// joinRoot reattaches the chroot root to a visible path (storage key form).
func joinRoot(root, visible string) string {
	base := path.Clean("/" + root)
	if base == "/" {
		return path.Clean(visible)
	}
	return path.Clean(base + "/" + strings.TrimPrefix(path.Clean(visible), "/"))
}

// ---- upload ----

func (s *Server) handleUpload(c *call) {
	q := c.r.URL.Query()
	logical := q.Get("path")
	if logical == "" {
		c.fail(errInvalidRequest("query parameter `path` is required (e.g. ?path=/reports/report.pdf)"))
		return
	}
	onConflict := q.Get("on_conflict")
	switch onConflict {
	case "", "fail", "overwrite", "rename":
		if onConflict == "" {
			onConflict = "fail"
		}
	default:
		c.fail(errInvalidRequest("on_conflict must be one of fail, overwrite, rename"))
		return
	}
	full, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)

	var reader io.Reader
	var size int64 = -1
	ct := c.r.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		mr, err := c.r.MultipartReader()
		if err != nil {
			c.fail(errInvalidRequest("invalid multipart body: " + err.Error()))
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				c.fail(errInvalidRequest("multipart body has no `file` part"))
				return
			}
			if err != nil {
				c.fail(errInvalidRequest("invalid multipart body: " + err.Error()))
				return
			}
			if part.FormName() == "file" {
				reader = part
				break
			}
		}
	} else {
		if c.r.ContentLength <= 0 {
			e := errInvalidRequest("raw-body uploads must carry a positive Content-Length (or use multipart with a `file` part)")
			e.Code = CodeLengthRequired
			e.status = http.StatusLengthRequired
			c.fail(e)
			return
		}
		size = c.r.ContentLength
		reader = c.r.Body
	}

	// Size limit (§4.5).
	limit := c.key.Limits.MaxUploadBytes
	if limit > 0 && size > limit {
		c.fail(tooLarge(limit))
		return
	}

	ctx, cancel := context.WithTimeout(c.r.Context(), streamTimeout)
	defer cancel()

	// Conflict handling (§4.10.2).
	conflictMode := "created"
	targetInternal, targetFull, targetVisible := internal, full, visible
	_, statErr := s.fsGet(ctx, internal)
	if statErr == nil {
		switch onConflict {
		case "fail":
			c.fail(errAlreadyExists(visible))
			return
		case "overwrite":
			conflictMode = "overwritten"
		case "rename":
			conflictMode = "renamed"
			var e2 *APIError
			targetInternal, targetFull, targetVisible, e2 = s.nextFreePath(c, internal, full, visible)
			if e2 != nil {
				c.fail(e2)
				return
			}
			c.setPath(targetVisible)
		}
	} else if !isNotFoundError(statErr) {
		c.fail(fsError(statErr))
		return
	}

	// Auto-create parents (§4.10.2).
	parent, _ := parentOf(targetInternal)
	if parent != "/" && parent != "" {
		if err := s.mkdirAll(ctx, parent); err != nil {
			c.fail(fsError(err))
			return
		}
	}

	// Stream to OpenList while computing CRC64 (ECMA == CRC-64/XZ) and
	// SHA-256 through a TeeReader (§4.4).
	crc := crc64.New(crc64Table)
	sha := sha256.New()
	counted := &countingReader{r: io.TeeReader(io.TeeReader(reader, crc), sha)}
	tee := io.Reader(counted)
	if limit > 0 && size < 0 {
		// Unknown length: enforce the limit locally.
		tee = io.LimitReader(counted, limit+1)
	}

	hdr := http.Header{}
	hdr.Set("File-Path", escapeUploadPath(targetInternal))
	hdr.Set("Content-Type", c.r.Header.Get("Content-Type"))
	hdr.Set("As-Task", "false") // synchronous put; a queued task is not authoritative completion
	req, err := s.client.NewRequest(ctx, http.MethodPut, url.URL{Path: "/api/fs/put"}, tee, size, hdr)
	if err != nil {
		c.fail(errUpstream("failed to build upload request"))
		return
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		c.fail(errUpstream(err.Error()))
		return
	}
	upData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	// A completed upload requires a valid success envelope (valid JSON with a
	// present success code); garbage or a missing code — even with HTTP 200 —
	// proves nothing.
	var envelope relayPutEnvelope
	if jerr := json.Unmarshal(upData, &envelope); jerr != nil || envelope.Code == nil {
		c.fail(errUpstream(fmt.Sprintf("upload response was not a valid success envelope (HTTP %s); the remote may hold a partial or complete object", resp.Status)))
		return
	}
	if resp.StatusCode != http.StatusOK || (*envelope.Code != 0 && *envelope.Code != http.StatusOK) {
		msg := envelope.Message
		if msg == "" {
			msg = resp.Status
		}
		c.fail(errUpstream("upload rejected: " + msg))
		return
	}
	// A queued async task is not a completion: track it to a terminal state
	// before the upload is reported as done.
	if taskID := envelope.taskID(); taskID != "" {
		if terr := s.awaitRelayUploadTask(ctx, taskID); terr != nil {
			e := errUpstream(terr.msg)
			e.Hint = "the upload was queued upstream and did not confirm in time; stat the target path to verify before retrying"
			c.fail(e)
			return
		}
	}

	crcStr := crc.Sum64()
	crcDec := u64decimal(crcStr)
	shaHex := hex.EncodeToString(sha.Sum(nil))

	// Verify the size limit when the length was unknown (multipart).
	if limit > 0 && size < 0 && counted.n > limit {
		dir, base := parentOf(targetInternal)
		_ = s.fsRemove(ctx, dir, []string{base})
		c.fail(tooLarge(limit))
		return
	}

	// Stat the uploaded object to record authoritative size/mtime.
	info, statErr := s.fsGet(ctx, targetInternal)
	finalSize := int64(0)
	mtime := ""
	if statErr == nil {
		finalSize = info.Size
		mtime = info.Modified
	} else if size > 0 {
		finalSize = size
	}

	// Expected checksums (§4.4): mismatch deletes the upload and returns 422.
	if exp := strings.TrimSpace(c.r.Header.Get("X-Expected-CRC64")); exp != "" && exp != crcDec {
		_ = s.fsRemove(ctx, parentOf0(targetInternal), []string{path.Base(targetInternal)})
		e := newAPIError(http.StatusUnprocessableEntity, CodeChecksumMismatch,
			"CRC64 mismatch: expected "+exp+", stored "+crcDec)
		e.Hint = "the upload was deleted; verify the local file and retry"
		c.fail(e)
		return
	}
	if exp := strings.ToLower(strings.TrimSpace(c.r.Header.Get("X-Expected-SHA256"))); exp != "" && exp != shaHex {
		_ = s.fsRemove(ctx, parentOf0(targetInternal), []string{path.Base(targetInternal)})
		e := newAPIError(http.StatusUnprocessableEntity, CodeChecksumMismatch,
			"SHA-256 mismatch: expected "+exp+", stored "+shaHex)
		e.Hint = "the upload was deleted; verify the local file and retry"
		c.fail(e)
		return
	}

	_ = s.st.RecordUpload(store.Upload{
		Drive:  c.drive,
		Path:   targetFull,
		Size:   finalSize,
		CRC64:  crcDec,
		SHA256: shaHex,
		Mtime:  mtime,
		KeyID:  c.key.ID,
	})
	if finalSize == 0 {
		finalSize = counted.n
	}
	c.bytes = finalSize
	c.ok(map[string]any{
		"path":     targetVisible,
		"size":     finalSize,
		"crc64":    crcDec,
		"sha256":   shaHex,
		"conflict": conflictMode,
	})
}

// countingReader tracks how many body bytes were consumed (multipart size
// limit enforcement).
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// parentOf0 is the dir part of parentOf.
func parentOf0(p string) string {
	d, _ := parentOf(p)
	return d
}

func tooLarge(limit int64) *APIError {
	e := newAPIError(http.StatusRequestEntityTooLarge, CodeTooLarge,
		"upload exceeds the key limit of "+u64decimal(uint64(limit))+" bytes")
	e.Hint = "ask the administrator to raise limits.max_upload_bytes if you need larger files"
	return e
}

// nextFreePath finds "name (n).ext" for rename conflicts.
func (s *Server) nextFreePath(c *call, internal, full, visible string) (string, string, string, *APIError) {
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	for n := 1; n <= 999; n++ {
		candInternal := renameCandidate(internal, n)
		candFull := renameCandidate(full, n)
		candVisible := renameCandidate(visible, n)
		if _, err := s.fsGet(ctx, candInternal); isNotFoundError(err) {
			return candInternal, candFull, candVisible, nil
		} else if err != nil {
			return "", "", "", fsError(err)
		}
	}
	return "", "", "", errAlreadyExists(visible)
}

// ---- download ----

func (s *Server) handleDownload(c *call) {
	logical := c.r.URL.Query().Get("path")
	full, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), streamTimeout)
	defer cancel()

	// Resolve metadata first: existence, sign and recorded checksums.
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			ae := errPathNotFound(visible)
			ae.Suggestions = s.suggestionsFor(ctx, c, visible)
			c.fail(ae)
			return
		}
		c.fail(fsError(err))
		return
	}
	if rec, err := s.st.LatestUploadFor(c.drive, full); err == nil &&
		rec.Size == info.Size && (rec.Mtime == "" || rec.Mtime == info.Modified) {
		c.w.Header().Set("X-C2A-SHA256", rec.SHA256)
		c.w.Header().Set("X-C2A-CRC64", rec.CRC64)
	}

	sign := info.Sign
	if sign == "" {
		sign, err = s.fetchSign(ctx, internal)
		if err != nil {
			c.fail(fsError(err))
			return
		}
	}

	q := c.r.URL.Query()
	q.Set("sign", sign)
	u := url.URL{Path: "/p" + internal, RawQuery: q.Encode()}
	hdr := http.Header{}
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range", "Accept"} {
		if v := c.r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	req, err := s.client.NewRequest(ctx, http.MethodGet, u, nil, 0, hdr)
	if err != nil {
		c.fail(errUpstream("failed to build download request"))
		return
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		c.fail(errUpstream(err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		c.fail(errUpstream("download upstream returned " + resp.Status))
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			c.w.Header().Set(h, v)
		}
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		c.w.Header().Set("Content-Disposition", cd)
	} else {
		c.w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(path.Base(visible))+`"`)
	}
	c.w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(c.w, resp.Body)
	c.bytes = n
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `"`, "'")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\x00", "")
	if name == "" {
		name = "download"
	}
	return name
}

// ---- mkdir / delete / move ----

func (s *Server) handleMkdir(c *call) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if body.Path == "" {
		c.fail(errInvalidRequest("field `path` is required"))
		return
	}
	_, internal, visible, e := s.resolve(c, body.Path)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	_, statErr := s.fsGet(ctx, internal)
	if statErr == nil {
		// Idempotent: an existing directory is a success (§4.10.2).
		c.ok(map[string]any{"path": visible, "created": false})
		return
	}
	if !isNotFoundError(statErr) {
		c.fail(fsError(statErr))
		return
	}
	if err := s.mkdirAll(ctx, internal); err != nil {
		c.fail(fsError(err))
		return
	}
	c.ok(map[string]any{"path": visible, "created": true})
}

func (s *Server) handleDelete(c *call) {
	var body struct {
		Paths  []string `json:"paths"`
		Path   string   `json:"path"`
		DryRun bool     `json:"dry_run"`
	}
	if !decodeBody(c, &body) {
		return
	}
	paths := body.Paths
	if len(paths) == 0 && body.Path != "" {
		paths = []string{body.Path}
	}
	if len(paths) == 0 {
		c.fail(errInvalidRequest("field `paths` (array) is required"))
		return
	}
	if len(paths) > 100 {
		c.fail(errInvalidRequest("at most 100 paths per delete call"))
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	if body.DryRun {
		// §4.10.3: report what would be affected without touching anything.
		totalFiles, totalDirs, totalBytes := int64(0), int64(0), int64(0)
		var items []string
		truncated := false
		for _, p := range paths {
			stats, e := s.walkPath(ctx, c, p, walkDefaultMaxEntries, time.Now().Add(25*time.Second))
			if e != nil {
				c.fail(e)
				return
			}
			totalFiles += stats.Files
			totalDirs += stats.Dirs
			totalBytes += stats.Bytes
			items = append(items, stats.Items...)
			truncated = truncated || stats.Truncated
		}
		if len(items) > 200 {
			items = items[:200]
		}
		c.ok(map[string]any{
			"dry_run": true,
			"would_affect": map[string]any{
				"files": totalFiles,
				"dirs":  totalDirs,
				"bytes": totalBytes,
			},
			"items":     items,
			"truncated": truncated || len(items) > 200,
		})
		return
	}

	useTrash := c.key.TrashOn
	results := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		results = append(results, s.deleteOne(c, ctx, p, useTrash))
	}
	c.ok(map[string]any{"results": results})
}

// deleteOne removes one path, routing through the trash when enabled.
func (s *Server) deleteOne(c *call, ctx context.Context, logical string, useTrash bool) map[string]any {
	_, internal, visible, e := s.resolve(c, logical)
	if e != nil {
		return map[string]any{"path": logical, "status": "error", "error": e.Message}
	}
	c.setPath(visible)
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			// Idempotent delete (§4.10.2).
			return map[string]any{"path": visible, "status": "already_absent"}
		}
		ae := fsError(err)
		return map[string]any{"path": visible, "status": "error", "error": ae.Message}
	}
	if !useTrash {
		dir, base := parentOf(internal)
		if err := s.fsRemove(ctx, dir, []string{base}); err != nil {
			ae := fsError(err)
			return map[string]any{"path": visible, "status": "error", "error": ae.Message}
		}
		return map[string]any{"path": visible, "status": "deleted"}
	}
	// Trash: move into /.c2a-trash/<date>/<orig> inside the key root
	// (§4.10.3) and record the item. trashFull is the logical path inside
	// the drive; trashInternal adds the mount prefix for upstream calls.
	trashFull := s.trashPathFor(c.root, visible)
	trashInternal := s.toInternal(c.drive, trashFull)
	tDir, tBase := parentOf(trashInternal)
	if err := s.mkdirAll(ctx, tDir); err != nil {
		ae := fsError(err)
		return map[string]any{"path": visible, "status": "error", "error": ae.Message}
	}
	// If the trash destination is taken, pick a suffixed name.
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
	_, tBase = parentOf(trashInternal)
	if err := s.moveAcross(ctx, internal, parentOf0(trashInternal), tBase); err != nil {
		ae := fsError(err)
		return map[string]any{"path": visible, "status": "error", "error": ae.Message}
	}
	days := c.key.TrashDays
	if days <= 0 {
		days = 7
	}
	id, err := s.st.InsertTrashItem(store.TrashItem{
		KeyID:      c.key.ID,
		Drive:      c.drive,
		OrigPath:   visible,
		TrashPath:  actualTrash,
		IsDir:      info.IsDir,
		Size:       info.Size,
		PurgeAfter: time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour),
	})
	if err != nil {
		return map[string]any{"path": visible, "status": "error", "error": "trash record failed"}
	}
	return map[string]any{"path": visible, "status": "trashed", "trash_id": id}
}

func (s *Server) handleMove(c *call) {
	var body struct {
		From       string `json:"from"`
		To         string `json:"to"`
		OnConflict string `json:"on_conflict"`
		DryRun     bool   `json:"dry_run"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if body.From == "" || body.To == "" {
		c.fail(errInvalidRequest("fields `from` and `to` are required"))
		return
	}
	switch body.OnConflict {
	case "", "fail", "overwrite", "rename":
		if body.OnConflict == "" {
			body.OnConflict = "fail"
		}
	default:
		c.fail(errInvalidRequest("on_conflict must be one of fail, overwrite, rename"))
		return
	}
	_, fromInternal, fromVisible, e := s.resolve(c, body.From)
	if e != nil {
		c.fail(e)
		return
	}
	toFull, toInternal, toVisible, e := s.resolve(c, body.To)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(fromVisible)
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()

	if _, err := s.fsGet(ctx, fromInternal); err != nil {
		if isNotFoundError(err) {
			ae := errPathNotFound(fromVisible)
			ae.Suggestions = s.suggestionsFor(ctx, c, fromVisible)
			c.fail(ae)
			return
		}
		c.fail(fsError(err))
		return
	}

	if body.DryRun {
		// §4.10.3: report the affected inventory of the source side.
		stats, e := s.walkPath(ctx, c, fromVisible, walkDefaultMaxEntries, time.Now().Add(25*time.Second))
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
			"from":    fromVisible,
			"to":      toVisible,
			"would_affect": map[string]any{
				"files": stats.Files,
				"dirs":  stats.Dirs,
				"bytes": stats.Bytes,
			},
			"items":     items,
			"truncated": stats.Truncated || len(stats.Items) > 200,
		})
		return
	}

	// Target conflict handling.
	if _, err := s.fsGet(ctx, toInternal); err == nil {
		switch body.OnConflict {
		case "fail":
			c.fail(errAlreadyExists(toVisible))
			return
		case "overwrite":
			// Remove the previous target through the trash when enabled so
			// the destructive step stays reversible.
			if res := s.deleteOne(c, ctx, body.To, c.key.TrashOn); res["status"] == "error" {
				c.fail(errUpstream("overwrite failed: " + res["error"].(string)))
				return
			}
		case "rename":
			var e2 *APIError
			_, toInternal, toVisible, e2 = s.nextFreePath(c, toInternal, toFull, toVisible)
			if e2 != nil {
				c.fail(e2)
				return
			}
		}
	} else if !isNotFoundError(err) {
		c.fail(fsError(err))
		return
	}

	toDir, toBase := parentOf(toInternal)
	if toDir != "/" {
		if err := s.mkdirAll(ctx, toDir); err != nil {
			c.fail(fsError(err))
			return
		}
	}
	fromDir, fromBase := parentOf(fromInternal)
	if fromDir == toDir && fromBase == toBase {
		c.fail(errInvalidRequest("`from` and `to` are identical"))
		return
	}
	if err := s.moveAcross(ctx, fromInternal, toDir, toBase); err != nil {
		c.fail(fsError(err))
		return
	}
	c.ok(map[string]any{"from": fromVisible, "to": toVisible})
}
