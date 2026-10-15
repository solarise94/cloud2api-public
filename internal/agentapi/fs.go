package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/gateway"
	"cloud2api/internal/openlist"
)

// fsTimeout bounds metadata calls; uploads/downloads use the relaxed one.
const (
	fsTimeout     = openlist.DefaultTimeout
	streamTimeout = openlist.RelaxedTimeout
)

// fsFileInfo is the typed view of one OpenList fs entry the agent API exposes
// (no OpenList-specific fields ever leave this package, §2).
type fsFileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
	Sign     string `json:"sign"`
	HashInfo string `json:"hashinfo"`
}

// upstreamError carries an OpenList business error.
type upstreamError struct {
	Code    int
	Message string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("code=%d message=%s", e.Code, e.Message)
}

// fsCall performs one OpenList fs JSON API call.
func (s *Server) fsCall(ctx context.Context, method, apiPath string, payload any) ([]byte, error) {
	var body io.Reader
	var length int64
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
		length = int64(len(data))
	}
	u, err := url.Parse(apiPath)
	if err != nil {
		return nil, err
	}
	req, err := s.client.NewRequest(ctx, method, *u, body, length,
		http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return nil, err
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("openlist returned non-JSON (HTTP %d)", resp.StatusCode)
	}
	if envelope.Code != 0 && envelope.Code != http.StatusOK {
		return nil, &upstreamError{Code: envelope.Code, Message: envelope.Message}
	}
	return envelope.Data, nil
}

// fsGet returns the metadata of one path ("object not found" included in the
// error text so callers can map it).
func (s *Server) fsGet(ctx context.Context, internal string) (fsFileInfo, error) {
	data, err := s.fsCall(ctx, http.MethodPost, "/api/fs/get", map[string]string{"path": internal})
	if err != nil {
		return fsFileInfo{}, err
	}
	var info fsFileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fsFileInfo{}, err
	}
	return info, nil
}

// fsList lists one directory page.
func (s *Server) fsList(ctx context.Context, internal string, page, perPage int) ([]fsFileInfo, int, error) {
	payload := map[string]any{"path": internal, "page": page, "per_page": perPage}
	data, err := s.fsCall(ctx, http.MethodPost, "/api/fs/list", payload)
	if err != nil {
		return nil, 0, err
	}
	var doc struct {
		Content []fsFileInfo `json:"content"`
		Total   int          `json:"total"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, err
	}
	return doc.Content, doc.Total, nil
}

// mkdirAll creates internal and its parents; "already exists" is success.
func (s *Server) mkdirAll(ctx context.Context, internal string) error {
	_, err := s.fsCall(ctx, http.MethodPost, "/api/fs/mkdir", map[string]string{"path": internal})
	if err != nil {
		if isExistsError(err) {
			return nil
		}
		return err
	}
	return nil
}

func isExistsError(err error) bool {
	if err == nil {
		return false
	}
	if ue, ok := err.(*upstreamError); ok {
		m := strings.ToLower(ue.Message)
		return strings.Contains(m, "exist") || strings.Contains(m, "already")
	}
	return false
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "object not found") || strings.Contains(m, "no such file")
}

// fsRemove removes names inside dir (permanent — no trash on the OpenList side).
func (s *Server) fsRemove(ctx context.Context, dir string, names []string) error {
	_, err := s.fsCall(ctx, http.MethodPost, "/api/fs/remove",
		map[string]any{"dir": dir, "names": names})
	return err
}

// fsMove moves names from srcDir to dstDir (names preserved).
func (s *Server) fsMove(ctx context.Context, srcDir, dstDir string, names []string) error {
	_, err := s.fsCall(ctx, http.MethodPost, "/api/fs/move",
		map[string]any{"src_dir": srcDir, "dst_dir": dstDir, "names": names})
	return err
}

// fsRename renames the basename of path.
func (s *Server) fsRename(ctx context.Context, p, newName string) error {
	_, err := s.fsCall(ctx, http.MethodPost, "/api/fs/rename",
		map[string]string{"path": p, "name": newName})
	return err
}

// fsSubmitCopyMove submits fs/copy or fs/move and returns the OpenList task
// ids it reported (same-storage copies may complete synchronously but still
// answer with tasks; the engine tracks them regardless).
func (s *Server) fsSubmitCopyMove(ctx context.Context, apiPath, srcDir, dstDir string, names []string) ([]string, error) {
	data, err := s.fsCall(ctx, http.MethodPost, apiPath,
		map[string]any{"src_dir": srcDir, "dst_dir": dstDir, "names": names})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(data, &doc)
	ids := make([]string, 0, len(doc.Tasks))
	for _, t := range doc.Tasks {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// fsOfflineDownload submits an offline download (SimpleHttp tool) into dir
// and returns the OpenList task ids. The file lands under dir with the URL's
// basename.
func (s *Server) fsOfflineDownload(ctx context.Context, dir, url string) ([]string, error) {
	data, err := s.fsCall(ctx, http.MethodPost, "/api/fs/add_offline_download",
		map[string]any{"path": dir, "urls": []string{url}, "tool": toolSimpleHT, "delete_policy": "delete_on_success"})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(data, &doc)
	ids := make([]string, 0, len(doc.Tasks))
	for _, t := range doc.Tasks {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// fetchSign resolves the /p download sign for an internal path.
func (s *Server) fetchSign(ctx context.Context, internal string) (string, error) {
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		return "", err
	}
	return info.Sign, nil
}

// ---- chroot path resolution (§4.2: root layer before MountJoin) ----

// resolve maps an agent-visible path on the key's drive to the internal
// OpenList path, enforcing the per-drive chroot root on top of the existing
// path-safety rules (gateway.CleanLogicalPath + gateway.MountJoin).
func (s *Server) resolve(c *call, logical string) (full string, internal string, visible string, apiErr *APIError) {
	root := c.root
	if root == "" {
		root = "/"
	}
	cleaned, err := gateway.CleanLogicalPath(logical)
	if err != nil {
		return "", "", "", errPathInvalid(logical)
	}
	// The trash directory is managed through /v1/trash only. The relay
	// directory (Gokapi compat, §4.7) is managed through /api/files/* only;
	// both are invisible to direct /v1 addressing.
	if cleaned == "/.c2a-trash" || strings.HasPrefix(cleaned, "/.c2a-trash/") {
		e := errPathInvalid(cleaned)
		e.Message = "/.c2a-trash is managed by the trash API; use GET /v1/trash and POST /v1/trash/restore"
		e.Hint = "deleting a path moves it to the trash automatically when the key has trash enabled"
		return "", "", "", e
	}
	if cleaned == "/"+relayDirName || strings.HasPrefix(cleaned, "/"+relayDirName+"/") {
		e := errPathInvalid(cleaned)
		e.Message = "/" + relayDirName + " is managed by the Gokapi compatibility API (/api/files/*)"
		e.Hint = "relay files are uploaded and listed through the /api/files endpoints"
		return "", "", "", e
	}
	root = path.Clean("/" + root)
	full = path.Clean(path.Join(root, cleaned))
	// Defense in depth: the joined path must stay inside the chroot (a clean
	// input can never escape, but verify anyway).
	if root != "/" && full != root && !strings.HasPrefix(full, root+"/") {
		return "", "", "", errPathInvalid(logical)
	}
	acct, _ := s.registry.Get(c.drive)
	mounted, err2 := gateway.MountJoin(acct.MountPath, full)
	if err2 != nil {
		return "", "", "", errPathInvalid(logical)
	}
	// visible: the agent-view path of full (root stripped).
	visible = full
	if root != "/" {
		visible = strings.TrimPrefix(full, root)
		if visible == "" {
			visible = "/"
		}
	}
	return full, mounted, visible, nil
}

// trashPathFor computes the trash destination of origPath (agent-visible)
// for today's date bucket, inside the key root.
func (s *Server) trashPathFor(root, visibleOrig string) string {
	date := time.Now().UTC().Format("2006-01-02")
	base := path.Clean("/" + root)
	if base == "/" {
		base = ""
	}
	return base + "/.c2a-trash/" + date + "/" + strings.TrimPrefix(path.Clean(visibleOrig), "/")
}

// escapeUploadPath percent-encodes each path segment for the File-Path
// header. OpenList PathUnescape()s the header value, where '+' is a literal
// character — url.QueryEscape would corrupt filenames containing spaces (or
// '+') by turning spaces into '+'.
func escapeUploadPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// parentOf returns dir and base ("/a/b.txt" -> "/a", "b.txt").
func parentOf(p string) (string, string) {
	p = path.Clean(p)
	dir, base := path.Split(p)
	return path.Clean("/" + dir), base
}

// renameCandidate produces "name (n).ext" variants.
func renameCandidate(p string, n int) string {
	dir, base := parentOf(p)
	if n <= 0 {
		return p
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return path.Join(dir, stem+" ("+strconv.Itoa(n)+")"+ext)
}

// suffixName produces "name (n).ext" from a bare name.
func suffixName(base string, n int) string {
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return stem + " (" + strconv.Itoa(n) + ")" + ext
}

// moveAcross moves srcInternal into dstDirInternal under the final basename
// wantBase, which the caller has verified to be free at the destination.
// OpenList's fs/move cannot rename, so when the current basename differs the
// item travels under a transit name that is free in BOTH directories: the
// original basename when the destination does not use it, otherwise wantBase
// via a rename at the source.
func (s *Server) moveAcross(ctx context.Context, srcInternal, dstDirInternal, wantBase string) error {
	srcDir, srcBase := parentOf(srcInternal)
	if srcDir == dstDirInternal {
		if srcBase != wantBase {
			return s.fsRename(ctx, srcInternal, wantBase)
		}
		return nil
	}
	transit := srcBase
	if srcBase != wantBase {
		if _, err := s.fsGet(ctx, path.Join(dstDirInternal, srcBase)); err == nil {
			// The destination already holds the current name; rename at the
			// source to the final name instead (the caller verified the
			// destination does not hold wantBase).
			if err := s.fsRename(ctx, srcInternal, wantBase); err != nil {
				return err
			}
			transit = wantBase
		}
	}
	if err := s.fsMove(ctx, srcDir, dstDirInternal, []string{transit}); err != nil {
		return err
	}
	if transit != wantBase {
		return s.fsRename(ctx, path.Join(dstDirInternal, transit), wantBase)
	}
	return nil
}

// parseInt parses a positive query int with a default.
func parseInt(get func(string) string, key string, def, max int) (int, *APIError) {
	raw := strings.TrimSpace(get(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errInvalidRequest(key + " must be a positive integer")
	}
	if n > max {
		n = max
	}
	return n, nil
}

// u64decimal formats a uint64 as a decimal string (CRC64 convention).
func u64decimal(v uint64) string { return strconv.FormatUint(v, 10) }
