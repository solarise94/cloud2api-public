// Gokapi compatibility layer (UPGRADE §4.7, Phase 1d).
//
// An agent that already follows the Gokapi relay instructions works unchanged
// after swapping the Base URL and API key: same paths (/api/info/*, /api/files/*,
// /api/chunk/*, /downloadFile), same `apikey` header, same UploadResult/FileInfo
// JSON shapes (CRC64 as an unsigned 64-bit decimal string).
//
// Semantics implemented here, not inherited from /v1:
//   - files live on the key's relay drive under <key root>/.relay/<Id>/<Name>,
//     hidden from /v1 listings and blocked from direct /v1 addressing;
//   - expiry is capped at the key's relay max age (default 7 days; never
//     unlimited), allowedDownloads=0 means unlimited downloads;
//   - quota: on upload, oldest files of the key are evicted first when the
//     relay usage would exceed quota_bytes (default 200 GiB); non-relay files
//     are never touched;
//   - deletion is permanent (no trash); `delay` arms a cancellable pending
//     deletion (POST /api/files/restore cancels it);
//   - the background sweep (every 5 minutes) purges expired / due files and
//     abandoned chunk staging directories (older than one day).
package agentapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/store"
)

const (
	// gokapiCompatVersion is the version string reported by /api/info/version.
	// The reference relay's OpenAPI documents 2.1.0/20100, so agents that
	// probe the version keep working (documented deviation: this is a
	// compatibility claim, not the upstream Gokapi release).
	gokapiCompatVersion    = "2.1.0"
	gokapiCompatVersionInt = 20100

	relayDirName = ".relay" // inside the key root on the relay drive

	relayChunkMinBytes  = 5 << 20  // Gokapi: chunks must be >= 5 MiB except the last
	relayChunkMiB       = 45       // recommended chunk size (reported by /api/info/config)
	relayStagedBytesCap = 50 << 30 // total staged chunk bytes across all uploads
	relayChunkPurgeAge  = 24 * time.Hour
	relayFileIDLen      = 15 // Gokapi-style random ids ("oNleRD3pUZgaDKn")
	relayHotlinkIDLen   = 43
	relayUploadTimeout  = 30 * time.Minute
)

const relayIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newCRC64 / newSHA256 / shaHex are thin wrappers so the relay layer reads the
// same way as files.go (CRC-64/XZ via the ECMA table, §4.4).
func newCRC64() hash.Hash64 { return crc64.New(crc64Table) }
func newSHA256() hash.Hash  { return sha256.New() }
func shaHex(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

// relayID mints a random URL-safe id of n characters (Gokapi-style).
func relayID(n int) string {
	buf := make([]byte, n)
	max := byte(len(relayIDAlphabet))
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is fatal-worthy; never fall back to a
		// predictable id for a bearer-of-id download surface.
		panic("relay id generation failed: " + err.Error())
	}
	for i, b := range buf {
		buf[i] = relayIDAlphabet[b%max]
	}
	return string(buf)
}

// ---- Gokapi JSON shapes (field names match the reference OpenAPI exactly) ----

type gokapiVersionInfo struct {
	Version                   string `json:"Version"`
	VersionInt                int    `json:"VersionInt"`
	EndToEndEncryptionEnabled bool   `json:"EndToEndEncryptionEnabled"`
}

type gokapiConfigInfo struct {
	MaxFilesize  int `json:"MaxFilesize"`  // MiB
	MaxChunksize int `json:"MaxChunksize"` // MiB
}

// gokapiFileInfo mirrors the Gokapi "File" schema.
type gokapiFileInfo struct {
	Id                           string `json:"Id"`
	Name                         string `json:"Name"`
	Size                         string `json:"Size"` // human readable
	SizeBytes                    int64  `json:"SizeBytes"`
	CRC64                        string `json:"CRC64"` // unsigned 64-bit decimal string
	SHA256                       string `json:"SHA256,omitempty"`
	HotlinkId                    string `json:"HotlinkId"`
	ContentType                  string `json:"ContentType"`
	ExpireAtString               string `json:"ExpireAtString"`
	UrlDownload                  string `json:"UrlDownload"` // HTML share page
	UrlHotlink                   string `json:"UrlHotlink"`  // direct file URL
	FileRequestId                string `json:"FileRequestId"`
	UploadDate                   int64  `json:"UploadDate"`
	ExpireAt                     int64  `json:"ExpireAt"`
	DownloadsRemaining           int64  `json:"DownloadsRemaining"`
	DownloadCount                int64  `json:"DownloadCount"`
	UnlimitedDownloads           bool   `json:"UnlimitedDownloads"`
	UnlimitedTime                bool   `json:"UnlimitedTime"` // always false: retention is capped
	RequiresClientSideDecryption bool   `json:"RequiresClientSideDecryption"`
	IsEncrypted                  bool   `json:"IsEncrypted"`
	IsEndToEndEncrypted          bool   `json:"IsEndToEndEncrypted"`
	IsPasswordProtected          bool   `json:"IsPasswordProtected"` // always false: passwords rejected at upload
	IsSavedOnLocalStorage        bool   `json:"IsSavedOnLocalStorage"`
	IsPendingDeletion            bool   `json:"IsPendingDeletion"`
	IsFileRequest                bool   `json:"IsFileRequest"`
	UploaderId                   int64  `json:"UploaderId"`
}

type gokapiUploadResult struct {
	Result          string          `json:"Result"`
	FileInfo        *gokapiFileInfo `json:"FileInfo,omitempty"`
	IncludeFilename bool            `json:"IncludeFilename"`
	// Additive receipt fields (docs/API-KEYS-AND-RELAY.md): every upload
	// response names its durable receipt so the true outcome — including a
	// later background failure or a disconnect — stays queryable.
	UploadId string `json:"UploadId,omitempty"`
	Status   string `json:"Status,omitempty"`
	Message  string `json:"Message,omitempty"`
}

// gokapiMessage writes a Gokapi-style error body {"Message": "..."}.
func gokapiMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"Message": msg})
}

// ---- routing ----

// isGokapiAPIPath reports whether the legacy /api/* namespace belongs to the
// Gokapi compatibility surface (never true for /api/storage/* or /api/transfer,
// which keep flowing to the legacy gateway).
func isGokapiAPIPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/")
	if !ok || rest == "" {
		return false
	}
	seg, _, _ := strings.Cut(rest, "/")
	switch seg {
	case "info", "files", "chunk", "auth", "user", "logs", "uploadrequest":
		return true
	}
	return false
}

// routeGokapi authenticates (apikey header or Bearer), rate-limits and audits
// one Gokapi-compat API request, mirroring routeV1.
func (s *Server) routeGokapi(w http.ResponseWriter, r *http.Request) {
	rec := &respRecorder{ResponseWriter: w}
	// Relay hop guard, answered in the Gokapi error shape (a cheap,
	// key-independent rejection before authentication).
	r, hopErr := hopGuard(r)
	if hopErr != nil {
		gokapiMessage(rec, hopErr.Status(), "invalid relay hop header")
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_hop", start: time.Now()}
		c.errMsg = hopErr.Code
		s.audit(c)
		return
	}
	presented := extractKey(r)
	if presented == "" {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_auth", start: time.Now()}
		gokapiMessage(rec, http.StatusUnauthorized, "invalid or missing API key")
		c.errMsg = "unauthorized"
		s.audit(c)
		return
	}
	key, outcome := s.keys.Authenticate(presented)
	if outcome != auth.AuthOK || key == nil {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_auth", start: time.Now()}
		msg := "invalid or missing API key"
		switch outcome {
		case auth.AuthRevoked:
			msg = "API key has been revoked"
		case auth.AuthExpired:
			msg = "API key has expired"
		}
		gokapiMessage(rec, http.StatusUnauthorized, msg)
		c.errMsg = msg
		s.audit(c)
		return
	}
	if ok, wait := s.limiter.allow(key.ID, key.Limits.RequestsPerMinute); !ok {
		c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_ratelimit", key: key, start: time.Now()}
		rec.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		gokapiMessage(rec, http.StatusTooManyRequests, "rate limit exceeded; retry later")
		c.errMsg = "rate_limited"
		s.audit(c)
		return
	}
	s.dispatchGokapi(rec, r, key)
}

// unsupportedGokapiEndpoints are the Gokapi admin surfaces the compatibility
// layer deliberately does not implement (UPGRADE §4.7): user management, API
// key management, file requests, logs and the file mutation endpoints.
func unsupportedGokapiEndpoint(segs []string) bool {
	if len(segs) < 2 {
		return false
	}
	switch segs[1] {
	case "auth", "user", "logs", "uploadrequest":
		return true
	case "files":
		if len(segs) >= 3 {
			switch segs[2] {
			case "changeOwner", "duplicate", "modify", "replace", "downloadzip":
				return true
			}
		}
	}
	return false
}

func (s *Server) dispatchGokapi(rec *respRecorder, r *http.Request, key *auth.Key) {
	c := &call{w: rec, r: r, key: key, base: s.baseURL(r), start: time.Now()}
	defer func() { s.audit(c) }()
	segs := splitPath(r.URL.Path) // ["api", ...]

	gk := func(status int, msg string) {
		c.errMsg = fmt.Sprintf("%d: %s", status, msg)
		gokapiMessage(rec, status, msg)
	}

	switch {
	case len(segs) == 3 && segs[1] == "info" && segs[2] == "version" && r.Method == http.MethodGet:
		c.op = "gokapi_version"
		writeJSON(rec, http.StatusOK, gokapiVersionInfo{
			Version: gokapiCompatVersion, VersionInt: gokapiCompatVersionInt,
			EndToEndEncryptionEnabled: false,
		})

	case len(segs) == 3 && segs[1] == "info" && segs[2] == "config" && r.Method == http.MethodGet:
		c.op = "gokapi_config"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		writeJSON(rec, http.StatusOK, s.gokapiConfigInfo(c))

	case len(segs) == 3 && segs[1] == "files" && segs[2] == "add" && r.Method == http.MethodPost:
		c.op = "gokapi_upload"
		s.handleGokapiAdd(c, gk)

	case len(segs) == 3 && segs[1] == "files" && segs[2] == "list" && r.Method == http.MethodGet:
		c.op = "gokapi_list"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		s.handleGokapiList(c)

	case len(segs) == 4 && segs[1] == "files" && segs[2] == "list" && r.Method == http.MethodGet:
		c.op = "gokapi_list"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		s.handleGokapiListOne(c, segs[3], gk)

	case len(segs) == 3 && segs[1] == "files" && segs[2] == "delete" && r.Method == http.MethodDelete:
		c.op = "gokapi_delete"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		s.handleGokapiDelete(c, gk)

	case len(segs) == 3 && segs[1] == "files" && segs[2] == "restore" && r.Method == http.MethodPost:
		c.op = "gokapi_restore"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		s.handleGokapiRestore(c, gk)

	case len(segs) == 4 && segs[1] == "files" && segs[2] == "download" && r.Method == http.MethodGet:
		c.op = "gokapi_download"
		if !s.gokapiRequireRelay(c, gk) {
			return
		}
		s.handleGokapiAPIDownload(c, segs[3], gk)

	case len(segs) == 4 && segs[1] == "files" && segs[2] == "status" && r.Method == http.MethodGet:
		c.op = "gokapi_upload_status"
		s.handleGokapiUploadStatus(c, segs[3], gk)

	case len(segs) == 3 && segs[1] == "chunk" && segs[2] == "add" && r.Method == http.MethodPost:
		c.op = "gokapi_chunk_add"
		s.handleGokapiChunkAdd(c, gk)

	case len(segs) == 3 && segs[1] == "chunk" && segs[2] == "complete" && r.Method == http.MethodPost:
		c.op = "gokapi_chunk_complete"
		s.handleGokapiChunkComplete(c, gk)

	case unsupportedGokapiEndpoint(segs):
		c.op = "gokapi_unsupported"
		c.errMsg = "not supported"
		writeJSON(rec, http.StatusForbidden, map[string]string{
			"Message": "this Gokapi endpoint is not supported by the cloud2api compatibility layer; " +
				"supported: /api/info/*, /api/files/{add,list,delete,restore,download}, /api/chunk/*",
		})

	default:
		c.op = "gokapi_not_found"
		gk(http.StatusNotFound, "endpoint not found")
	}
}

// gokapiRequireRelay resolves the effective relay drive, checks the `relay`
// permission and the drive availability (storage guard). It fills c.drive and
// c.root on success.
func (s *Server) gokapiRequireRelay(c *call, gk func(int, string)) bool {
	drive := c.key.EffectiveRelayDrive()
	if drive == "" {
		gk(http.StatusBadRequest, "this key has no relay drive; ask the administrator to configure one")
		return false
	}
	grant, granted := c.key.DriveGrant(drive)
	if !granted {
		gk(http.StatusForbidden, "the relay drive of this key is not granted to it")
		return false
	}
	if !c.key.CanOn(drive, auth.PermRelay) {
		gk(http.StatusForbidden, "API key does not have the relay permission")
		return false
	}
	acct, known := s.registry.Get(drive)
	if !known {
		gk(http.StatusServiceUnavailable, "relay drive not found")
		return false
	}
	if !acct.Enabled {
		gk(http.StatusServiceUnavailable, "relay drive is disabled")
		return false
	}
	if verdict := s.guard.Check(c.r.Context(), drive); !verdict.Allowed() {
		gk(http.StatusServiceUnavailable, "relay drive is unavailable: "+verdict.Message)
		return false
	}
	c.drive = drive
	c.root = grant.Root
	return true
}

func (s *Server) gokapiConfigInfo(c *call) gokapiConfigInfo {
	maxMiB := int(relayMaxUploadBytes(c.key) >> 20)
	if maxMiB < 1 {
		maxMiB = 1
	}
	return gokapiConfigInfo{MaxFilesize: maxMiB, MaxChunksize: relayChunkMiB}
}

// ---- upload receipts (docs/API-KEYS-AND-RELAY.md) ----

// validUploadID accepts client-chosen X-Upload-ID values: 8-128 url-safe
// characters, no leading dot.
func validUploadID(id string) bool {
	if len(id) < 8 || len(id) > 128 || strings.HasPrefix(id, ".") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// credQueryRe strips credential-looking query values from upload messages.
var credQueryRe = regexp.MustCompile(`(?i)(sign|token|password|passwd|secret|key|credential)=[^&\s"']+`)

// authHeaderRe strips Authorization-style credential values, and keyShapeRe
// redacts a pasted API key of either format wherever it leaks into a message.
var authHeaderRe = regexp.MustCompile(`(?i)(bearer|apikey|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
var keyShapeRe = regexp.MustCompile(`sk-(c2a-[a-z0-9]{8}-)?[A-Za-z0-9]{16,}`)

// scrubUploadMessage bounds and sanitizes upload outcome text everywhere it
// travels: the persisted receipt, the JSON response and the audit row.
func scrubUploadMessage(msg string) string {
	msg = credQueryRe.ReplaceAllString(msg, "$1=<redacted>")
	msg = authHeaderRe.ReplaceAllString(msg, "$1 <redacted>")
	msg = keyShapeRe.ReplaceAllString(msg, "<redacted-api-key>")
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return msg
}

// reserveUploadReceipt creates the immutable identity of a NEW upload attempt
// at authenticated entry — BEFORE any multipart body parsing, so a disconnect
// while receiving the body is still recorded. A client-supplied X-Upload-ID
// is required to be fresh: any duplicate (own or foreign) is a 409, because
// reusing an id could overwrite the truthful outcome of the earlier attempt.
// Returns "" when the response was already written.
func (s *Server) reserveUploadReceipt(c *call, gk func(int, string), uploadUUID, filename string, filesize int64) string {
	id := strings.TrimSpace(c.r.Header.Get("X-Upload-ID"))
	if id != "" && !validUploadID(id) {
		gk(http.StatusBadRequest, "X-Upload-ID must be 8-128 characters (letters, digits, '-', '_', '.')")
		return ""
	}
	if id == "" {
		id = relayID(relayFileIDLen)
	}
	if err := s.st.ReserveRelayUpload(id, c.key.ID, uploadUUID, filename, filesize); err != nil {
		switch {
		case errors.Is(err, store.ErrUploadIDTaken):
			gk(http.StatusConflict, "this upload id already belongs to another API key; send a fresh X-Upload-ID")
		case errors.Is(err, store.ErrUploadIDExists):
			gk(http.StatusConflict, "this upload id was already used by this key; retry with a NEW X-Upload-ID (the previous attempt stays queryable and unchanged)")
		default:
			gk(http.StatusInternalServerError, "upload receipt could not be created")
		}
		return ""
	}
	c.w.Header().Set("X-Upload-ID", id)
	return id
}

// receiptGK wraps the Gokapi error writer of an upload handler: the error
// body gains the additive UploadId/Status fields and the receipt records the
// outcome — interrupted when ctx is done (client disconnect, cancel or
// timeout: the remote may hold a partial or complete object), failed
// otherwise. The message is scrubbed ONCE for the response, the audit row and
// the receipt. Bytes read or progress never decide the state; a receipt that
// already reached a terminal state is left untouched (monotone outcomes).
func (s *Server) receiptGK(c *call, id string, ctx context.Context, gk func(int, string)) func(int, string) {
	return func(status int, raw string) {
		state := store.RelayUploadFailed
		if ctx.Err() != nil {
			state = store.RelayUploadInterrupted
			raw = "connection interrupted; the remote may hold a partial or complete object (" + raw + ")"
		}
		msg := scrubUploadMessage(raw)
		_ = s.st.FinishRelayUpload(id, state, msg, nil)
		c.errMsg = fmt.Sprintf("%d: %s", status, msg)
		writeJSON(c.w, status, map[string]any{
			"Message":  msg,
			"UploadId": id,
			"Status":   state,
		})
	}
}

// finishUploadReceipt records the terminal success of an upload: upstream
// fs/put answered HTTP success with a valid success envelope AND the relay
// metadata row was persisted. It fails when the receipt could not be moved to
// completed (store error or a terminal state already recorded) — the caller
// must then answer honestly unconfirmed instead of claiming completion.
func (s *Server) finishUploadReceipt(id string, row *store.RelayFile) error {
	msg := "upload confirmed by the storage backend and recorded"
	return s.st.FinishRelayUpload(id, store.RelayUploadCompleted, msg, row)
}

// unconfirmedUpload writes the honest non-completion answer after an upload
// stored upstream but its receipt could not be finalized: the reported state
// is the receipt's actual (read-back) state, never a made-up "completed".
func (s *Server) unconfirmedUpload(c *call, id string) {
	state, msg := store.RelayUploadInterrupted,
		"upload stored but its receipt could not be finalized; outcome unconfirmed — query /api/files/status/"+id+" before reusing the file"
	if fresh, err := s.st.GetRelayUpload(id); err == nil {
		state = fresh.State
		if fresh.State == store.RelayUploadCompleted {
			msg = "upload recorded by a concurrent attempt; verify via /api/files/status/" + id
		}
	}
	c.errMsg = "502: " + msg
	writeJSON(c.w, http.StatusBadGateway, map[string]any{
		"Message":  msg,
		"UploadId": id,
		"Status":   state,
	})
}

// handleGokapiUploadStatus implements GET /api/files/status/{uploadId}: the
// durable receipt of one upload of THIS key. Unknown ids and ids owned by
// another key are indistinguishable 404s.
func (s *Server) handleGokapiUploadStatus(c *call, id string, gk func(int, string)) {
	if !validUploadID(id) {
		gk(http.StatusNotFound, "unknown upload id")
		return
	}
	r, err := s.st.GetRelayUpload(id)
	if err != nil || r.KeyID != c.key.ID {
		gk(http.StatusNotFound, "unknown upload id")
		return
	}
	c.setPath("/" + relayDirName + "/receipt/" + r.ID)
	out := map[string]any{
		"UploadId":  r.ID,
		"Status":    r.State,
		"Message":   r.Message,
		"Filename":  r.Filename,
		"Filesize":  r.Filesize,
		"CreatedAt": r.CreatedAt.UTC().Format(time.RFC3339),
		"UpdatedAt": r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.State == store.RelayUploadCompleted && r.FileID != "" {
		if f, ferr := s.st.GetRelayFile(r.FileID); ferr == nil {
			out["FileInfo"] = s.gokapiFileInfoOf(c.base, f)
		}
	}
	writeJSON(c.w, http.StatusOK, out)
}

// ---- multipart helpers ----

// gokapiForm is one parsed /api/files/add or /api/chunk/add multipart body.
// The `file` part is spooled to a temporary file because scalar fields may
// follow it (multipart readers discard a part once NextPart advances) and
// because uploads must not be buffered in memory.
type gokapiForm struct {
	filePath    string
	filename    string
	contentType string
	size        int64
	values      map[string]string
}

// open returns a fresh reader over the spooled file part.
func (f *gokapiForm) open() (io.ReadCloser, error) {
	return os.Open(f.filePath)
}

// relayMaxUploadBytes is the effective per-file relay cap: the key's
// max_upload_bytes when set, else the Gokapi-relay default of 8 GiB (the same
// number /api/info/config reports).
func relayMaxUploadBytes(key *auth.Key) int64 {
	if key.Limits.MaxUploadBytes > 0 {
		return key.Limits.MaxUploadBytes
	}
	return 8 << 30
}

// parseGokapiMultipart streams the multipart body, spooling the `file` part
// to disk and capturing the scalar form values. Content-Length is required
// (Gokapi rejects unknown-length bodies). The caller removes form.filePath
// when done.
func (s *Server) parseGokapiMultipart(c *call, gk func(int, string)) (*gokapiForm, bool) {
	if c.r.ContentLength <= 0 {
		gk(http.StatusLengthRequired, "Content-Length is required")
		return nil, false
	}
	ct := c.r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
		gk(http.StatusBadRequest, "request must be multipart/form-data")
		return nil, false
	}
	mr, err := c.r.MultipartReader()
	if err != nil {
		gk(http.StatusBadRequest, "invalid multipart body: "+err.Error())
		return nil, false
	}
	spoolDir := filepath.Join(s.relayChunkRoot(), "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		gk(http.StatusInternalServerError, "upload staging failed")
		return nil, false
	}
	form := &gokapiForm{values: map[string]string{}}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.Remove(form.filePath)
			gk(http.StatusBadRequest, "invalid multipart body: "+err.Error())
			return nil, false
		}
		if part.FormName() == "file" {
			if form.filePath != "" {
				os.Remove(form.filePath)
				gk(http.StatusBadRequest, "multiple `file` parts")
				return nil, false
			}
			tmp, err := os.CreateTemp(spoolDir, ".spool-*")
			if err != nil {
				gk(http.StatusInternalServerError, "upload staging failed")
				return nil, false
			}
			max := relayMaxUploadBytes(c.key)
			written, copyErr := io.Copy(tmp, io.LimitReader(part, max+1))
			closeErr := tmp.Close()
			if copyErr != nil || closeErr != nil {
				os.Remove(tmp.Name())
				gk(http.StatusInternalServerError, "upload staging failed")
				return nil, false
			}
			if written > max {
				os.Remove(tmp.Name())
				gk(http.StatusRequestEntityTooLarge,
					"file exceeds the configured maximum size of "+u64decimal(uint64(max))+" bytes")
				return nil, false
			}
			form.filePath = tmp.Name()
			form.filename = part.FileName()
			form.contentType = part.Header.Get("Content-Type")
			form.size = written
			continue
		}
		// Scalar fields are tiny; bound them defensively.
		data, err := io.ReadAll(io.LimitReader(part, 4<<10))
		if err != nil {
			os.Remove(form.filePath)
			gk(http.StatusBadRequest, "invalid multipart body: "+err.Error())
			return nil, false
		}
		form.values[part.FormName()] = strings.TrimSpace(string(data))
	}
	if form.filePath == "" {
		gk(http.StatusBadRequest, "multipart body has no `file` part")
		return nil, false
	}
	return form, true
}

// gokapiSanitizeName reduces an uploaded filename to a safe single component.
func gokapiSanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "\x00", "")
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	if len(name) > 255 {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:255-len(ext)] + ext
	}
	return name
}

// gokapiContentType fills an empty content type from the filename extension.
func gokapiContentType(given, name string) string {
	given = strings.TrimSpace(given)
	if given != "" && given != "application/octet-stream" {
		return given
	}
	if ext := path.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}

// ---- upload core (shared by /api/files/add and /api/chunk/complete) ----

// relayUploadOptions carries the Gokapi upload parameters.
type relayUploadOptions struct {
	name             string
	contentType      string
	allowedDownloads int64 // 0 = unlimited
	expiryDays       int64 // <=0 or >cap -> cap
	knownSize        int64 // -1 when streaming with unknown length
}

// relayPutEnvelope is the fs/put response the receipt contract validates. The
// patched OpenList (server/handles/fsup.go) answers a queued upload with
// common.SuccessResp nesting the task info under data.task (data.task.id);
// a top-level task_id is kept as a legacy alias. Code is a pointer so a
// malformed/empty body or a missing code field can never read as success.
type relayPutEnvelope struct {
	Code    *int   `json:"code"`
	Message string `json:"message"`
	TaskID  string `json:"task_id"`
	Data    *struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
		ID string `json:"id"`
	} `json:"data"`
}

// taskID returns the id of a queued upload task, if the response carries one.
func (e *relayPutEnvelope) taskID() string {
	if e.Data != nil {
		if e.Data.Task != nil && e.Data.Task.ID != "" {
			return e.Data.Task.ID
		}
		if e.Data.ID != "" {
			return e.Data.ID
		}
	}
	return e.TaskID
}

// relayUpload streams reader onto the relay drive under /.relay/<Id>/<Name>,
// computing CRC-64/XZ + SHA-256, enforcing the per-key max upload size and the
// relay quota (evicting the key's oldest files first), and records the row.
func (s *Server) relayUpload(ctx context.Context, c *call, reader io.Reader, opts relayUploadOptions) (*store.RelayFile, string, *gokapiError) {
	name := gokapiSanitizeName(opts.name)
	contentType := gokapiContentType(opts.contentType, name)

	id := relayID(relayFileIDLen)
	ext := ""
	if e := path.Ext(name); len(e) <= 11 && strings.Trim(e, ".") != "" {
		ext = e
	}
	hotlink := relayID(relayHotlinkIDLen) + ext

	maxAge := c.key.EffectiveRelayMaxAge()
	days := opts.expiryDays
	if days <= 0 || days > int64(maxAge) {
		days = int64(maxAge)
	}
	now := time.Now().UTC()
	expire := now.AddDate(0, 0, int(days))
	if expire.Sub(now) > time.Duration(maxAge)*24*time.Hour {
		expire = now.Add(time.Duration(maxAge) * 24 * time.Hour)
	}
	if opts.allowedDownloads < 0 {
		return nil, "", &gokapiError{status: http.StatusBadRequest, msg: "allowedDownloads must be >= 0"}
	}
	remaining := opts.allowedDownloads
	if remaining == 0 {
		remaining = -1 // unlimited
	}

	// Destination: <root>/.relay/<Id>/<Name> on the relay drive.
	full := joinRoot(c.root, "/"+relayDirName+"/"+id+"/"+name)
	internal := s.toInternal(c.drive, full)
	dir, _ := parentOf(internal)
	if err := s.mkdirAll(ctx, dir); err != nil {
		return nil, "", gokapiUpstreamError(err)
	}

	crc := newCRC64()
	sha := newSHA256()
	counted := &countingReader{r: io.TeeReader(io.TeeReader(reader, crc), sha)}
	stream := io.Reader(counted)
	limit := relayMaxUploadBytes(c.key)
	stream = io.LimitReader(counted, limit+1)
	hdr := http.Header{}
	hdr.Set("File-Path", escapeUploadPath(internal))
	hdr.Set("Content-Type", contentType)
	// The fork's fs/put defaults uploads to As-Task:true (queued work); a
	// synchronous put is what the receipt contract treats as authoritative.
	hdr.Set("As-Task", "false")
	req, err := s.client.NewRequest(ctx, http.MethodPut, url.URL{Path: "/api/fs/put"}, stream, opts.knownSize, hdr)
	if err != nil {
		return nil, "", &gokapiError{status: http.StatusBadGateway, msg: "failed to build upload request"}
	}
	resp, err := s.client.DoAutoHeal(req)
	if err != nil {
		return nil, "", &gokapiError{status: http.StatusBadGateway, msg: "upload failed: " + err.Error()}
	}
	upData, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	// Authoritative completion requires a VALID success envelope: valid JSON,
	// a present "code" and a success value. A malformed or empty body — even
	// with HTTP 200 — proves nothing and never completes a receipt (the
	// remote may still hold a partial or complete object).
	var envelope relayPutEnvelope
	if jerr := json.Unmarshal(upData, &envelope); jerr != nil || envelope.Code == nil {
		return nil, "", &gokapiError{status: http.StatusBadGateway,
			msg: fmt.Sprintf("upload response was not a valid success envelope (HTTP %s); the remote may hold a partial or complete object", resp.Status)}
	}
	if resp.StatusCode != http.StatusOK || (*envelope.Code != 0 && *envelope.Code != http.StatusOK) {
		msg := envelope.Message
		if msg == "" {
			msg = resp.Status
		}
		return nil, "", &gokapiError{status: http.StatusBadGateway, msg: "upload rejected: " + msg}
	}
	// A put that OpenList still queued as an async task is NOT a completed
	// upload. The fork nests the task under data.task (handles/fsup.go);
	// top-level task_id is kept as a legacy alias. Track any detected task to
	// a terminal state — an unresolved task never completes a receipt.
	if taskID := envelope.taskID(); taskID != "" {
		if terr := s.awaitRelayUploadTask(ctx, taskID); terr != nil {
			return nil, "", terr
		}
	}
	c.bytes = counted.n

	if limit > 0 && counted.n > limit {
		_, b := parentOf(internal)
		_ = s.fsRemove(ctx, dir, []string{b})
		return nil, "", &gokapiError{status: http.StatusRequestEntityTooLarge,
			msg: "file exceeds the configured maximum size of " + u64decimal(uint64(limit)) + " bytes"}
	}

	size := counted.n
	if opts.knownSize > 0 && opts.knownSize != size {
		_, b := parentOf(internal)
		_ = s.fsRemove(ctx, dir, []string{b})
		return nil, "", &gokapiError{status: http.StatusBadRequest,
			msg: fmt.Sprintf("uploaded %d bytes but filesize header said %d", size, opts.knownSize)}
	}

	row := store.RelayFile{
		ID:                 id,
		HotlinkID:          hotlink,
		KeyID:              c.key.ID,
		Drive:              c.drive,
		Name:               name,
		Size:               size,
		CRC64:              u64decimal(crc.Sum64()),
		SHA256:             shaHex(sha),
		ContentType:        contentType,
		UploadedAt:         now,
		ExpireAt:           expire,
		DownloadsRemaining: remaining,
	}
	if err := s.st.InsertRelayFile(row); err != nil {
		_, b := parentOf(internal)
		_ = s.fsRemove(ctx, dir, []string{b})
		return nil, "", &gokapiError{status: http.StatusInternalServerError,
			msg: "relay metadata write failed; the upload reached the storage backend but could not be recorded — treat the outcome as unconfirmed and verify before reuse"}
	}
	if err := s.enforceRelayQuota(ctx, c, id); err != nil {
		// The new file alone exceeded the quota (nothing older left to evict).
		return nil, "", err
	}
	return &row, internal, nil
}

type gokapiError struct {
	status int
	msg    string
}

func gokapiUpstreamError(err error) *gokapiError {
	return &gokapiError{status: http.StatusBadGateway, msg: "storage backend error: " + err.Error()}
}

// awaitRelayUploadTask polls an OpenList upload task that fs/put returned to a
// terminal state. Task states mirror jobman.go's mapping. A failed or canceled
// task is a failed upload; a task that is still unresolved when the request
// context ends is an interrupted upload (the remote may hold a partial or
// complete object) — never a completed one.
func (s *Server) awaitRelayUploadTask(ctx context.Context, taskID string) *gokapiError {
	unresolved := func(why string) *gokapiError {
		return &gokapiError{status: http.StatusGatewayTimeout,
			msg: why + " (OpenList task " + taskID + " still processing; the remote may hold a partial or complete object — verify before reuse)"}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		found := false
		for _, done := range []bool{false, true} {
			tasks, err := s.taskList(ctx, taskUpload, done)
			if err != nil {
				if ctx.Err() != nil {
					return unresolved("upload tracking aborted")
				}
				continue
			}
			for _, t := range tasks {
				if t.ID != taskID {
					continue
				}
				found = true
				switch olTaskTerminalState(t) {
				case store.JobSucceeded:
					return nil
				case store.JobCanceled:
					return &gokapiError{status: http.StatusBadGateway, msg: "upload task canceled: " + t.Error}
				case store.JobFailed:
					msg := t.Error
					if msg == "" {
						msg = t.Status
					}
					return &gokapiError{status: http.StatusBadGateway, msg: "upload task failed: " + msg}
				}
			}
		}
		if !found && ctx.Err() != nil {
			return unresolved("upload tracking lost")
		}
		select {
		case <-ctx.Done():
			return unresolved("upload tracking timed out")
		case <-ticker.C:
		}
	}
}

// enforceRelayQuota evicts the key's oldest relay files (by upload time) until
// the usage fits the quota. keepID (the file that triggered the check) is only
// removed when it alone exceeds the quota, in which case the upload fails
// before any older file is evicted.
func (s *Server) enforceRelayQuota(ctx context.Context, c *call, keepID string) *gokapiError {
	quota := c.key.EffectiveRelayQuota()
	quotaErr := func() *gokapiError {
		return &gokapiError{status: http.StatusBadRequest,
			msg: "upload exceeds the relay quota of " + u64decimal(uint64(quota)) + " bytes"}
	}
	// A file larger than the whole quota can never fit: fail (and remove it)
	// before evicting anything, so a doomed upload destroys no older files.
	newFile, err := s.st.GetRelayFile(keepID)
	if err != nil {
		return &gokapiError{status: http.StatusInternalServerError, msg: "relay metadata read failed"}
	}
	if newFile.Size > quota {
		s.relayPurgeFile(ctx, &newFile)
		return quotaErr()
	}
	for {
		usage, err := s.st.RelayUsage(c.key.ID)
		if err != nil {
			return &gokapiError{status: http.StatusInternalServerError, msg: "relay usage query failed"}
		}
		if usage <= quota {
			return nil
		}
		files, err := s.st.ListRelayFiles(c.key.ID) // oldest first
		if err != nil {
			return &gokapiError{status: http.StatusInternalServerError, msg: "relay listing failed"}
		}
		var victim *store.RelayFile
		for i := range files {
			if files[i].ID != keepID {
				victim = &files[i]
				break
			}
		}
		if victim == nil {
			// Should not happen (the new file itself fits the quota), but
			// never loop forever: drop the new file and fail.
			s.relayPurgeFile(ctx, &newFile)
			return quotaErr()
		}
		s.relayPurgeFile(ctx, victim)
		s.log.Info("relay quota eviction", "key", c.key.ID, "file", victim.ID, "size", victim.Size)
	}
}

// relayPurgeFile permanently removes one relay file's bytes and row (never the
// trash; Gokapi semantics). When the storage guard denies the drive (e.g. an
// unconfirmed replacement sits at the mount), the fs removal is skipped —
// deleting under the mount could destroy files that belong to the NEW storage
// — but the row is still dropped so retention and quota eviction make
// progress.
func (s *Server) relayPurgeFile(ctx context.Context, f *store.RelayFile) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	allowed := s.guard.Check(cctx, f.Drive).Allowed()
	cancel()
	internal := s.relayInternalPath(f)
	if allowed && internal != "" {
		dir, base := parentOf(internal)
		if _, err := s.fsGet(ctx, internal); err == nil {
			if err := s.fsRemove(ctx, dir, []string{base}); err != nil {
				s.log.Warn("relay purge: file remove failed", "id", f.ID, "err", err)
			}
		}
		// Drop the (now empty) id directory too.
		idDir, idBase := parentOf(dir)
		_ = s.fsRemove(ctx, idDir, []string{idBase})
	} else if !allowed {
		s.log.Warn("relay purge skipped the storage (drive unavailable)", "id", f.ID, "drive", f.Drive)
	}
	_ = s.st.DeleteRelayFile(f.ID)
}

// relayInternalPath maps a relay row back to its OpenList path using the
// owning key's current grant root. Empty when the row is undressable.
func (s *Server) relayInternalPath(f *store.RelayFile) string {
	root := "/"
	if k, err := s.keys.Get(f.KeyID); err == nil {
		for _, g := range k.Drives {
			if g.Drive == f.Drive {
				root = g.Root
				if root == "" {
					root = "/"
				}
				break
			}
		}
	}
	full := joinRoot(root, "/"+relayDirName+"/"+f.ID+"/"+f.Name)
	return s.toInternal(f.Drive, full)
}

// gokapiFileInfoOf renders one row in Gokapi's File shape.
func (s *Server) gokapiFileInfoOf(base string, f store.RelayFile) gokapiFileInfo {
	remaining := f.DownloadsRemaining
	unlimited := f.DownloadsRemaining < 0
	if unlimited {
		remaining = 0
	}
	info := gokapiFileInfo{
		Id:                 f.ID,
		Name:               f.Name,
		Size:               gokapiHumanSize(f.Size),
		SizeBytes:          f.Size,
		CRC64:              f.CRC64,
		SHA256:             f.SHA256,
		HotlinkId:          f.HotlinkID,
		ContentType:        f.ContentType,
		ExpireAtString:     f.ExpireAt.UTC().Format("2006-01-02 15:04"),
		UrlDownload:        base + "/d?id=" + url.QueryEscape(f.ID),
		UrlHotlink:         base + "/h/" + f.HotlinkID,
		UploadDate:         f.UploadedAt.UTC().Unix(),
		ExpireAt:           f.ExpireAt.UTC().Unix(),
		DownloadsRemaining: remaining,
		DownloadCount:      f.DownloadCount,
		UnlimitedDownloads: unlimited,
		IsPendingDeletion:  !f.PendingDeletion.IsZero(),
		UploaderId:         1,
	}
	return info
}

// gokapiHumanSize renders Gokapi-style sizes ("395.4 kB").
func gokapiHumanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n) / 1024
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		if v < 1024 {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1f PB", v)
}

// ---- /api/files/* handlers ----

func (s *Server) handleGokapiAdd(c *call, gk func(int, string)) {
	// Reserve the receipt at authenticated entry, BEFORE the relay checks and
	// body parsing alike: a disconnect anywhere after this point — including
	// while the multipart body streams in — leaves a queryable record.
	// The attempt id is immutable: a duplicate X-Upload-ID is rejected here.
	id := s.reserveUploadReceipt(c, gk, "", "", 0)
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), relayUploadTimeout)
	defer cancel()
	gk = s.receiptGK(c, id, ctx, gk)

	if !s.gokapiRequireRelay(c, gk) {
		return
	}
	form, ok := s.parseGokapiMultipart(c, gk)
	if !ok {
		return
	}
	defer os.Remove(form.filePath)
	// Fill the receipt with what is actually being uploaded now that the
	// body is known (the entry reservation could only bind the id).
	_ = s.st.UpdateRelayUploadMeta(id, c.key.ID, "", form.filename, form.size)
	if pw := form.values["password"]; pw != "" {
		gk(http.StatusBadRequest, "password-protected uploads are not supported; omit the password field")
		return
	}
	allowed, err := gokapiInt(form.values["allowedDownloads"], 1)
	if err != nil {
		gk(http.StatusBadRequest, "allowedDownloads must be an integer")
		return
	}
	days, err := gokapiInt(form.values["expiryDays"], 0)
	if err != nil {
		gk(http.StatusBadRequest, "expiryDays must be an integer")
		return
	}
	reader, err := form.open()
	if err != nil {
		gk(http.StatusInternalServerError, "upload staging failed")
		return
	}
	defer reader.Close()
	row, _, gerr := s.relayUpload(ctx, c, reader, relayUploadOptions{
		name:             form.filename,
		contentType:      form.contentType,
		allowedDownloads: allowed,
		expiryDays:       days,
		knownSize:        form.size,
	})
	if gerr != nil {
		gk(gerr.status, gerr.msg)
		return
	}
	if ferr := s.finishUploadReceipt(id, row); ferr != nil {
		s.unconfirmedUpload(c, id)
		return
	}
	writeJSON(c.w, http.StatusOK, gokapiUploadResult{
		Result:          "OK",
		FileInfo:        ptr(s.gokapiFileInfoOf(c.base, *row)),
		IncludeFilename: false,
		UploadId:        id,
		Status:          store.RelayUploadCompleted,
		Message:         "upload confirmed by the storage backend and recorded",
	})
}

func (s *Server) handleGokapiList(c *call) {
	files, err := s.st.ListRelayFiles(c.key.ID)
	if err != nil {
		gokapiMessage(c.w, http.StatusInternalServerError, "relay listing failed")
		c.errMsg = "relay listing failed"
		return
	}
	// Gokapi answers null (not []) when nothing is stored.
	var out []gokapiFileInfo
	for _, f := range files {
		out = append(out, s.gokapiFileInfoOf(c.base, f))
	}
	writeJSON(c.w, http.StatusOK, out)
}

func (s *Server) handleGokapiListOne(c *call, id string, gk func(int, string)) {
	f, err := s.st.GetRelayFile(id)
	if err != nil || f.KeyID != c.key.ID {
		gk(http.StatusNotFound, "invalid ID provided or file has expired")
		return
	}
	writeJSON(c.w, http.StatusOK, s.gokapiFileInfoOf(c.base, f))
}

func (s *Server) handleGokapiDelete(c *call, gk func(int, string)) {
	id := strings.TrimSpace(c.r.Header.Get("id"))
	if id == "" {
		gk(http.StatusBadRequest, "the `id` header is required")
		return
	}
	f, err := s.st.GetRelayFile(id)
	if err != nil || f.KeyID != c.key.ID {
		gk(http.StatusNotFound, "invalid ID supplied")
		return
	}
	// Deleting relay files (now or delayed) is a destructive capability: it
	// requires the explicit delete permission on the relay drive, so the
	// standard no-delete permission groups cannot remove files through the
	// relay either.
	if !c.key.CanOn(c.drive, auth.PermDelete) {
		gk(http.StatusForbidden, "deleting relay files requires the delete permission on the relay drive")
		return
	}
	c.setPath("/" + relayDirName + "/" + f.ID + "/" + f.Name)
	delay := int64(0)
	if raw := strings.TrimSpace(c.r.Header.Get("delay")); raw != "" {
		d, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || d < 0 {
			gk(http.StatusBadRequest, "the `delay` header must be a non-negative integer (seconds)")
			return
		}
		delay = d
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	if delay > 0 {
		if err := s.st.SetRelayPendingDeletion(id, time.Now().UTC().Add(time.Duration(delay)*time.Second)); err != nil {
			gk(http.StatusInternalServerError, "failed to arm the delayed deletion")
			return
		}
		c.w.WriteHeader(http.StatusOK)
		return
	}
	s.relayPurgeFile(ctx, &f)
	c.w.WriteHeader(http.StatusOK)
}

func (s *Server) handleGokapiRestore(c *call, gk func(int, string)) {
	id := strings.TrimSpace(c.r.Header.Get("id"))
	if id == "" {
		gk(http.StatusBadRequest, "the `id` header is required")
		return
	}
	f, err := s.st.GetRelayFile(id)
	if err != nil || f.KeyID != c.key.ID {
		gk(http.StatusNotFound, "invalid ID supplied or the file has already been deleted")
		return
	}
	if !c.key.CanOn(c.drive, auth.PermDelete) {
		gk(http.StatusForbidden, "restoring relay files requires the delete permission on the relay drive")
		return
	}
	if f.PendingDeletion.IsZero() {
		gk(http.StatusNotFound, "this file is not pending deletion")
		return
	}
	if err := s.st.SetRelayPendingDeletion(id, time.Time{}); err != nil {
		gk(http.StatusInternalServerError, "failed to cancel the pending deletion")
		return
	}
	f, _ = s.st.GetRelayFile(id)
	writeJSON(c.w, http.StatusOK, gokapiUploadResult{
		Result:          "OK",
		FileInfo:        ptr(s.gokapiFileInfoOf(c.base, f)),
		IncludeFilename: false,
	})
}

func (s *Server) handleGokapiAPIDownload(c *call, id string, gk func(int, string)) {
	f, err := s.st.GetRelayFile(id)
	if err != nil || f.KeyID != c.key.ID {
		gk(http.StatusNotFound, "invalid ID provided or file has expired")
		return
	}
	if strings.EqualFold(c.r.Header.Get("presignUrl"), "true") {
		gk(http.StatusBadRequest, "presignUrl is not supported; the file is streamed directly")
		return
	}
	consume := false
	if v := c.r.Header.Get("increaseCounter"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			gk(http.StatusBadRequest, "the `increaseCounter` header must be a boolean")
			return
		}
		consume = b
	}
	c.setPath("/" + relayDirName + "/" + f.ID + "/" + f.Name)
	s.gokapiServeFile(c, f, consume)
}

// gokapiInt parses a scalar form value with a Gokapi default.
func gokapiInt(raw string, def int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

// gokapiServeFile streams one relay file's bytes to the client (shared by the
// authenticated download and the public hotlink/downloadFile endpoints).
// consume=true decrements downloads_remaining.
func (s *Server) gokapiServeFile(c *call, f store.RelayFile, consume bool) {
	gkFail := func(status int, msg string) {
		c.errMsg = fmt.Sprintf("%d: %s", status, msg)
		gokapiMessage(c.w, status, msg)
	}
	if !f.ExpireAt.IsZero() && time.Now().After(f.ExpireAt) {
		gkFail(http.StatusGone, "this file has expired")
		return
	}
	if f.DownloadsRemaining == 0 {
		gkFail(http.StatusGone, "the download limit of this file has been reached")
		return
	}
	if consume && !s.st.ConsumeRelayDownload(f.ID) {
		gkFail(http.StatusGone, "the download limit of this file has been reached")
		return
	}
	// Storage guard on the file's own drive: the public /downloadFile and
	// /h/{id} endpoints carry no API key, so this is their only drive check —
	// a replaced or unverified storage must never serve relay bytes.
	if verdict := s.guard.Check(c.r.Context(), f.Drive); !verdict.Allowed() {
		gkFail(http.StatusServiceUnavailable, "the storage of this file is unavailable: "+verdict.Message)
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), streamTimeout)
	defer cancel()
	internal := s.relayInternalPath(&f)
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			gkFail(http.StatusGone, "the file data of this link no longer exists")
			return
		}
		gkFail(http.StatusBadGateway, "storage backend error")
		return
	}
	sign := info.Sign
	if sign == "" {
		sign, err = s.fetchSign(ctx, internal)
		if err != nil {
			gkFail(http.StatusBadGateway, "storage backend error")
			return
		}
	}
	q := url.Values{}
	q.Set("sign", sign)
	u := url.URL{Path: "/d" + internal, RawQuery: q.Encode()}
	hdr := http.Header{}
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range"} {
		if v := c.r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	// DoDownload follows OpenList's /d direct-link 302 server-side so the
	// upstream URL never reaches the Gokapi client; failures stay generic.
	resp, err := s.client.DoDownload(ctx, http.MethodGet, u, hdr)
	if err != nil {
		gkFail(http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	if downloadRedirect(resp.StatusCode) || resp.StatusCode >= 400 {
		gkFail(http.StatusBadGateway, "upstream returned "+resp.Status)
		return
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			c.w.Header().Set(h, v)
		}
	}
	c.w.Header().Set("Content-Type", f.ContentType)
	c.w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(f.Name)+`"`)
	c.w.Header().Set("Cache-Control", "private, max-age=0")
	c.w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(c.w, resp.Body)
	c.bytes = n
}

// ---- chunk staging (disk) ----

// relayChunkRoot is the chunk staging directory: $C2A_RELAY_DATA_DIR or
// <db dir>/relay-chunks.
func (s *Server) relayChunkRoot() string {
	if v := strings.TrimSpace(os.Getenv("C2A_RELAY_DATA_DIR")); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(s.st.Path()), "relay-chunks")
}

func validRelayUUID(uuid string) bool {
	if len(uuid) < 10 || len(uuid) > 128 {
		return false
	}
	for _, r := range uuid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(uuid, ".")
}

// relayChunkMeta is the tiny consistency record inside a staging directory.
type relayChunkMeta struct {
	FileSize  int64  `json:"filesize"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) readChunkMeta(dir string) (relayChunkMeta, error) {
	var m relayChunkMeta
	data, err := os.ReadFile(filepath.Join(dir, ".meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func writeChunkMeta(dir string, m relayChunkMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".meta.json"), data, 0o600)
}

// stagedBytes sums every staged chunk across all uploads (size guard).
func stagedBytes(root string) int64 {
	var total int64
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if info, err := f.Info(); err == nil && !f.IsDir() {
				total += info.Size()
			}
		}
	}
	return total
}

func (s *Server) handleGokapiChunkAdd(c *call, gk func(int, string)) {
	// Receipts are optional at staging: one is bound only when the client
	// sends X-Upload-ID, still at authenticated entry — before the relay
	// checks and the chunk body are touched. A fresh id is reserved; an id
	// this key already uses is reused WITHOUT resetting it (repeated staging);
	// a foreign id is refused. Staging errors never finalize the receipt —
	// the attempt stays alive for the next chunk — and the staged
	// acknowledgement is NOT an upload completion.
	var receiptID string
	if raw := strings.TrimSpace(c.r.Header.Get("X-Upload-ID")); raw != "" {
		if !validUploadID(raw) {
			gk(http.StatusBadRequest, "X-Upload-ID must be 8-128 characters (letters, digits, '-', '_', '.')")
			return
		}
		switch err := s.st.ReserveRelayUpload(raw, c.key.ID, "", "", 0); {
		case err == nil, errors.Is(err, store.ErrUploadIDExists): // fresh, or own id reused
		case errors.Is(err, store.ErrUploadIDTaken):
			gk(http.StatusConflict, "this upload id already belongs to another API key; send a fresh X-Upload-ID")
			return
		default:
			gk(http.StatusInternalServerError, "upload receipt could not be created")
			return
		}
		receiptID = raw
		c.w.Header().Set("X-Upload-ID", receiptID)
	}
	if !s.gokapiRequireRelay(c, gk) {
		return
	}
	form, ok := s.parseGokapiMultipart(c, gk)
	if !ok {
		return
	}
	if receiptID != "" {
		// Metadata only — never a state reset, never a revival.
		_ = s.st.UpdateRelayUploadMeta(receiptID, c.key.ID, form.values["uuid"], form.filename, form.size)
	}
	spoolPath := form.filePath
	// On success the spool file is renamed into the chunk directory; every
	// other exit removes it.
	committed := false
	defer func() {
		if !committed {
			os.Remove(spoolPath)
		}
	}()
	uuid := form.values["uuid"]
	if !validRelayUUID(uuid) {
		gk(http.StatusBadRequest, "uuid must be at least 10 characters (alphanumeric, '-', '_')")
		return
	}
	filesize, err := strconv.ParseInt(form.values["filesize"], 10, 64)
	if err != nil || filesize <= 0 {
		gk(http.StatusBadRequest, "filesize must be a positive integer (total file size in bytes)")
		return
	}
	offset, err := strconv.ParseInt(form.values["offset"], 10, 64)
	if err != nil || offset < 0 {
		gk(http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	written := form.size
	if offset+written > filesize {
		gk(http.StatusBadRequest, fmt.Sprintf("chunk (offset %d, %d bytes) exceeds the declared file size %d", offset, written, filesize))
		return
	}
	isLast := offset+written == filesize
	if written < relayChunkMinBytes && !isLast {
		gk(http.StatusBadRequest, fmt.Sprintf("chunks must be at least %d bytes unless they are the last chunk (got %d)", relayChunkMinBytes, written))
		return
	}

	root := s.relayChunkRoot()
	dir := filepath.Join(root, uuid)
	if m, err := s.readChunkMeta(dir); err == nil && m.FileSize != filesize {
		gk(http.StatusBadRequest, fmt.Sprintf("filesize changed mid-upload: meta says %d, request says %d", m.FileSize, filesize))
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		gk(http.StatusInternalServerError, "chunk staging failed")
		return
	}
	if err := writeChunkMeta(dir, relayChunkMeta{FileSize: filesize, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		gk(http.StatusInternalServerError, "chunk staging failed")
		return
	}

	target := filepath.Join(dir, strconv.FormatInt(offset, 10))
	existing := int64(0)
	if st, err := os.Stat(target); err == nil {
		existing = st.Size()
	}
	// Global staging-area guard: after the rename the area holds
	// staged(root) - old chunk + this chunk bytes.
	if stagedBytes(root)-existing+written > relayStagedBytesCap {
		gk(http.StatusBadRequest, "the upload staging area is full; retry later")
		return
	}
	if err := os.Rename(spoolPath, target); err != nil {
		gk(http.StatusInternalServerError, "chunk staging failed")
		return
	}
	committed = true
	c.bytes = written
	resp := map[string]any{"Result": "OK"}
	if receiptID != "" {
		resp["UploadId"] = receiptID
		resp["Status"] = store.RelayUploadUploading
		resp["Message"] = "chunk staged; staging is not upload completion"
	}
	writeJSON(c.w, http.StatusOK, resp)
}

// relayChunkPart is one staged chunk.
type relayChunkPart struct {
	offset int64
	size   int64
	path   string
}

// collectRelayChunks reads a staging directory and validates contiguity.
func collectRelayChunks(dir string, filesize int64) ([]relayChunkPart, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("unknown uuid: %w", err)
	}
	var parts []relayChunkPart
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		off, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || off < 0 {
			return nil, fmt.Errorf("unexpected file %q in the staging area", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		parts = append(parts, relayChunkPart{offset: off, size: info.Size(), path: filepath.Join(dir, e.Name())})
	}
	if len(parts) == 0 {
		return nil, errors.New("no chunks uploaded for this uuid")
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].offset < parts[j].offset })
	expect := int64(0)
	for _, p := range parts {
		if p.offset != expect {
			return nil, fmt.Errorf("chunks are not contiguous: expected a chunk at offset %d", expect)
		}
		expect += p.size
	}
	if expect != filesize {
		return nil, fmt.Errorf("assembled size is %d bytes but filesize says %d", expect, filesize)
	}
	return parts, nil
}

func (s *Server) handleGokapiChunkComplete(c *call, gk func(int, string)) {
	h := c.r.Header
	uuid := strings.TrimSpace(h.Get("uuid"))
	if !validRelayUUID(uuid) {
		gk(http.StatusBadRequest, "the `uuid` header must be at least 10 characters")
		return
	}
	if pw := strings.TrimSpace(h.Get("password")); pw != "" {
		gk(http.StatusBadRequest, "password-protected uploads are not supported; omit the password header")
		return
	}
	filename := strings.TrimSpace(h.Get("filename"))
	if filename == "" {
		gk(http.StatusBadRequest, "the `filename` header is required")
		return
	}
	if rest, ok := strings.CutPrefix(filename, "base64:"); ok {
		dec, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			gk(http.StatusBadRequest, "the `filename` header carries an invalid base64: prefix")
			return
		}
		filename = string(dec)
	}
	filesize, err := strconv.ParseInt(strings.TrimSpace(h.Get("filesize")), 10, 64)
	if err != nil || filesize <= 0 {
		gk(http.StatusBadRequest, "the `filesize` header must be a positive integer")
		return
	}
	allowed, err := gokapiInt(strings.TrimSpace(h.Get("allowedDownloads")), 1)
	if err != nil || allowed < 0 {
		gk(http.StatusBadRequest, "the `allowedDownloads` header must be a non-negative integer")
		return
	}
	days, err := gokapiInt(strings.TrimSpace(h.Get("expiryDays")), 0)
	if err != nil {
		gk(http.StatusBadRequest, "the `expiryDays` header must be an integer")
		return
	}
	nonblocking := strings.EqualFold(strings.TrimSpace(h.Get("nonblocking")), "true")

	// Bind the receipt at authenticated entry — headers are metadata only, no
	// body follows — so the relay checks and every later failure (and the
	// background nonblocking attempt) are recorded against it. The chunk flow
	// may reuse the receipt its staging created (header id or uuid lookup);
	// a duplicate completion of an attempt that already finished is refused
	// instead of silently re-running or overwriting the truthful outcome.
	id, ok := s.bindChunkReceipt(c, gk, uuid, filename, filesize)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), relayUploadTimeout)
	defer cancel()
	gk = s.receiptGK(c, id, ctx, gk)

	if !s.gokapiRequireRelay(c, gk) {
		return
	}

	dir := filepath.Join(s.relayChunkRoot(), uuid)
	if m, err := s.readChunkMeta(dir); err == nil && m.FileSize != filesize {
		gk(http.StatusBadRequest, fmt.Sprintf("filesize changed mid-upload: chunks were staged for %d bytes", m.FileSize))
		return
	}
	parts, err := collectRelayChunks(dir, filesize)
	if err != nil {
		gk(http.StatusBadRequest, err.Error())
		return
	}

	contentType := strings.TrimSpace(h.Get("contenttype"))
	opts := relayUploadOptions{
		name:             filename,
		contentType:      contentType,
		allowedDownloads: allowed,
		expiryDays:       days,
		knownSize:        filesize,
	}

	if nonblocking {
		// Finish in the background; the acknowledgement is not proof of a
		// completed upload — Status stays uploading and the receipt records
		// the background outcome for polling. The call is copied so the
		// in-flight request's audit row is untouched.
		bc := *c
		bc.w = nil
		go func() {
			bgCtx, bgCancel := context.WithTimeout(context.Background(), relayUploadTimeout)
			defer bgCancel()
			row, _, gerr := s.relayUpload(bgCtx, &bc, chunkAssemblyReader(parts), opts)
			if gerr != nil {
				state, msg := store.RelayUploadFailed, gerr.msg
				if bgCtx.Err() != nil {
					state = store.RelayUploadInterrupted
					msg = "background assembly aborted; the remote may hold a partial or complete object (" + msg + ")"
				}
				if ferr := s.st.FinishRelayUpload(id, state, scrubUploadMessage(msg), nil); ferr != nil {
					s.log.Warn("relay chunk completion outcome could not be recorded", "uuid", uuid, "upload", id, "err", ferr)
				}
				s.log.Warn("relay chunk completion failed", "uuid", uuid, "upload", id, "err", gerr.msg)
				return
			}
			_ = os.RemoveAll(dir)
			if ferr := s.finishUploadReceipt(id, row); ferr != nil {
				s.log.Warn("relay chunk completion could not be recorded as completed", "uuid", uuid, "upload", id, "err", ferr)
			}
			s.log.Info("relay chunk upload completed", "uuid", uuid, "upload", id, "id", row.ID, "size", row.Size)
		}()
		writeJSON(c.w, http.StatusOK, gokapiUploadResult{
			Result:          "OK",
			IncludeFilename: false,
			UploadId:        id,
			Status:          store.RelayUploadUploading,
			Message:         "assembly accepted and running in the background; poll GET /api/files/status/" + id + " for the terminal state",
		})
		return
	}

	row, _, gerr := s.relayUpload(ctx, c, chunkAssemblyReader(parts), opts)
	if gerr != nil {
		gk(gerr.status, gerr.msg)
		return
	}
	_ = os.RemoveAll(dir)
	if ferr := s.finishUploadReceipt(id, row); ferr != nil {
		s.unconfirmedUpload(c, id)
		return
	}
	writeJSON(c.w, http.StatusOK, gokapiUploadResult{
		Result:          "OK",
		FileInfo:        ptr(s.gokapiFileInfoOf(c.base, *row)),
		IncludeFilename: false,
		UploadId:        id,
		Status:          store.RelayUploadCompleted,
		Message:         "upload confirmed by the storage backend and recorded",
	})
}

// receiptClaimConflict answers a duplicate completion that lost the atomic
// claim: 409 with the receipt's actual state, WITHOUT persisting anything —
// the loser must neither run an upstream upload nor overwrite the winner's
// eventual outcome with failed/interrupted.
func (s *Server) receiptClaimConflict(c *call, id string) {
	state := store.RelayUploadUploading
	if r, err := s.st.GetRelayUpload(id); err == nil {
		state = r.State
	}
	msg := "this upload is already being completed by another request; poll GET /api/files/status/" + id + " for its outcome"
	c.errMsg = "409: " + msg
	writeJSON(c.w, http.StatusConflict, map[string]any{
		"Message":  msg,
		"UploadId": id,
		"Status":   state,
	})
}

// bindChunkReceipt resolves the immutable receipt of a chunked upload attempt
// for /api/chunk/complete: a client X-Upload-ID wins (an existing own receipt
// of the SAME chunk uuid is adopted without reset, a foreign one is a 409, an
// unknown one is reserved fresh); without a header the receipt bound to the
// uuid is adopted, else a fresh id is minted. After binding, the completion
// claim is taken atomically: exactly one duplicate may proceed — the loser
// answers 409 before any upstream work. A receipt that already reached a
// terminal state, belongs to another attempt, or is mid-completion is never
// adopted or re-run.
func (s *Server) bindChunkReceipt(c *call, gk func(int, string), uploadUUID, filename string, filesize int64) (string, bool) {
	adoptOr409 := func(id string) (string, bool) {
		err := s.st.AdoptRelayUpload(id, c.key.ID, uploadUUID, filename, filesize)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrReceiptTerminal):
			gk(http.StatusConflict, "this upload already finished (terminal receipt); start a new upload with a fresh X-Upload-ID")
			return "", false
		case errors.Is(err, store.ErrUploadIDTaken):
			gk(http.StatusConflict, "this upload id already belongs to another API key; send a fresh X-Upload-ID")
			return "", false
		case errors.Is(err, store.ErrUploadAttemptMismatch):
			gk(http.StatusConflict, "this upload id belongs to a different upload attempt (uuid mismatch); send a fresh X-Upload-ID")
			return "", false
		case errors.Is(err, store.ErrReceiptClaimed):
			s.receiptClaimConflict(c, id)
			return "", false
		case errors.Is(err, store.ErrNotFound):
			// Unknown id: treat as a fresh attempt under the client's id.
			id = s.reserveUploadReceipt(c, gk, uploadUUID, filename, filesize)
			if id == "" {
				return "", false
			}
		default:
			gk(http.StatusInternalServerError, "upload receipt could not be updated")
			return "", false
		}
		// Atomic completion claim: the single guard between duplicate
		// completions and the upstream upload.
		if cerr := s.st.ClaimRelayUpload(id, c.key.ID); cerr != nil {
			switch {
			case errors.Is(cerr, store.ErrReceiptTerminal):
				gk(http.StatusConflict, "this upload already finished (terminal receipt); start a new upload with a fresh X-Upload-ID")
			case errors.Is(cerr, store.ErrReceiptClaimed):
				s.receiptClaimConflict(c, id)
			default:
				gk(http.StatusInternalServerError, "upload receipt could not be claimed")
			}
			return "", false
		}
		c.w.Header().Set("X-Upload-ID", id)
		return id, true
	}
	if raw := strings.TrimSpace(c.r.Header.Get("X-Upload-ID")); raw != "" {
		if !validUploadID(raw) {
			gk(http.StatusBadRequest, "X-Upload-ID must be 8-128 characters (letters, digits, '-', '_', '.')")
			return "", false
		}
		if id, ok := adoptOr409(raw); id != "" {
			return id, ok
		}
		return "", false
	}
	if existing, err := s.st.RelayUploadByUUID(c.key.ID, uploadUUID); err == nil {
		return adoptOr409(existing.ID)
	}
	id := relayID(relayFileIDLen)
	if err := s.st.ReserveRelayUpload(id, c.key.ID, uploadUUID, filename, filesize); err != nil {
		gk(http.StatusInternalServerError, "upload receipt could not be created")
		return "", false
	}
	if cerr := s.st.ClaimRelayUpload(id, c.key.ID); cerr != nil {
		s.receiptClaimConflict(c, id)
		return "", false
	}
	c.w.Header().Set("X-Upload-ID", id)
	return id, true
}

// chunkAssemblyReader concatenates the staged chunks in offset order.
func chunkAssemblyReader(parts []relayChunkPart) io.Reader {
	readers := make([]io.Reader, 0, len(parts))
	for _, p := range parts {
		readers = append(readers, &fileOpener{path: p.path})
	}
	return io.MultiReader(readers...)
}

// fileOpener opens its file lazily on first Read and streams it through.
type fileOpener struct {
	path string
	f    *os.File
}

func (o *fileOpener) Read(p []byte) (int, error) {
	if o.f == nil {
		f, err := os.Open(o.path)
		if err != nil {
			return 0, err
		}
		o.f = f
	}
	return o.f.Read(p)
}

// ---- public endpoints (no API key) ----

// handleGokapiDownloadFile streams GET /downloadFile?id=<Id> without a key,
// consuming one download.
func (s *Server) handleGokapiDownloadFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		gokapiMessage(w, http.StatusBadRequest, "the `id` query parameter is required")
		return
	}
	rec := &respRecorder{ResponseWriter: w}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_public_download", start: time.Now()}
	defer func() { s.audit(c) }()
	f, err := s.st.GetRelayFile(id)
	if err != nil {
		c.errMsg = "file not found"
		gokapiMessage(rec, http.StatusNotFound, "invalid ID provided or file has expired")
		return
	}
	c.auditKeyID = f.KeyID
	c.drive = f.Drive
	c.setPath("/" + relayDirName + "/" + f.ID + "/" + f.Name)
	s.gokapiServeFile(c, f, true)
}

// handleGokapiHotlink streams GET /h/<HotlinkId> without a key, consuming one
// download.
func (s *Server) handleGokapiHotlink(w http.ResponseWriter, r *http.Request, hotlinkID string) {
	hotlinkID = strings.Trim(strings.TrimSpace(hotlinkID), "/")
	if hotlinkID == "" {
		gokapiMessage(w, http.StatusNotFound, "unknown hotlink")
		return
	}
	rec := &respRecorder{ResponseWriter: w}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_public_download", start: time.Now()}
	defer func() { s.audit(c) }()
	f, err := s.st.GetRelayFileByHotlink(hotlinkID)
	if err != nil {
		c.errMsg = "hotlink not found"
		gokapiMessage(rec, http.StatusNotFound, "unknown hotlink")
		return
	}
	c.auditKeyID = f.KeyID
	c.drive = f.Drive
	c.setPath("/" + relayDirName + "/" + f.ID + "/" + f.Name)
	s.gokapiServeFile(c, f, true)
}

// handleGokapiDownloadPage serves GET /d?id=<Id>: the minimal HTML share page
// (Gokapi's UrlDownload). The button points at /downloadFile?id=.
func (s *Server) handleGokapiDownloadPage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	rec := &respRecorder{ResponseWriter: w}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "gokapi_download_page", start: time.Now()}
	defer func() { s.audit(c) }()
	pageErr := func(status int, msg string) {
		c.errMsg = msg
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>cloud2api relay</title></head>"+
			"<body style=\"font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;line-height:1.5\">"+
			"<h1>File unavailable</h1><p>%s</p></body></html>", msg)
	}
	if id == "" {
		pageErr(http.StatusBadRequest, "the `id` query parameter is required")
		return
	}
	f, err := s.st.GetRelayFile(id)
	if err != nil {
		pageErr(http.StatusNotFound, "invalid ID provided or file has expired")
		return
	}
	c.auditKeyID = f.KeyID
	c.drive = f.Drive
	if time.Now().After(f.ExpireAt) {
		pageErr(http.StatusGone, "this file has expired")
		return
	}
	if f.DownloadsRemaining == 0 {
		pageErr(http.StatusGone, "the download limit of this file has been reached")
		return
	}
	dl := s.baseURL(r) + "/downloadFile?id=" + url.QueryEscape(f.ID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	name := strings.ReplaceAll(sanitizeFilename(f.Name), "<", "&lt;")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;line-height:1.5">
<h1>%s</h1>
<p>Size: %s (%d bytes) &middot; Type: %s<br>
Expires: %s UTC</p>
<p><a href="%s" style="display:inline-block;padding:0.6rem 1.2rem;background:#2563eb;color:#fff;
text-decoration:none;border-radius:6px;font-weight:600">Download</a></p>
<p style="color:#666;font-size:0.9rem">Served by the cloud2api Gokapi-compatible file relay.</p>
</body></html>`, name, name, gokapiHumanSize(f.Size), f.Size,
		strings.ReplaceAll(f.ContentType, "<", "&lt;"),
		f.ExpireAt.UTC().Format("2006-01-02 15:04"), dl)
}

// ---- retention sweep ----

// StartRelayMaintenance launches the 5-minute relay loop: purge expired and
// due relay files permanently and remove abandoned chunk staging directories.
// The boot pass first marks every receipt still in state uploading as
// interrupted — an in-flight upload cannot have survived the restart, and the
// remote may hold a partial or complete object.
func (s *Server) StartRelayMaintenance(ctx context.Context) {
	if n, err := s.st.InterruptRelayUploadsBefore(time.Now(),
		"gateway restarted; the remote may hold a partial or complete object; verify before reuse"); err != nil {
		s.log.Warn("upload receipt boot interrupt failed", "err", err)
	} else if n > 0 {
		s.log.Info("interrupted stale upload receipts after restart", "count", n)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		s.runRelayMaintenance(ctx) // run once at startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runRelayMaintenance(ctx)
			}
		}
	}()
}

// relayUploadReceiptStale marks receipts uploading for more than a day as
// interrupted (a handler that died without finalizing); terminal receipts are
// pruned after a week — they are diagnostics, not billing records.
const (
	relayUploadReceiptStale  = 24 * time.Hour
	relayUploadReceiptRetain = 7 * 24 * time.Hour
)

func (s *Server) runRelayMaintenance(ctx context.Context) map[string]int {
	report := map[string]int{}
	if n, err := s.st.InterruptRelayUploadsBefore(time.Now().Add(-relayUploadReceiptStale),
		"no final outcome was recorded within 24h; the remote may hold a partial or complete object; verify before reuse"); err == nil && n > 0 {
		report["upload_receipts_interrupted"] = n
	}
	if n, err := s.st.PruneRelayUploads(time.Now().Add(-relayUploadReceiptRetain)); err == nil && n > 0 {
		report["upload_receipts_pruned"] = n
	}
	due, err := s.st.DueRelayFiles(time.Now())
	if err != nil {
		s.log.Warn("relay expiry query failed", "err", err)
	}
	for i := range due {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		s.relayPurgeFile(cctx, &due[i])
		cancel()
		report["relay_files_purged"]++
		s.log.Info("purged relay file", "id", due[i].ID, "key", due[i].KeyID, "size", due[i].Size)
	}
	report["chunk_dirs_purged"] = s.purgeAbandonedChunks()
	return report
}

// purgeAbandonedChunks removes staging directories whose newest activity is
// older than one day (Gokapi: abandoned chunks are cleaned after one day).
func (s *Server) purgeAbandonedChunks() int {
	root := s.relayChunkRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if e.Name() == "spool" {
			// Leaked spool files (crashed requests) expire too.
			files, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range files {
				info, err := f.Info()
				if err == nil && !f.IsDir() && time.Since(info.ModTime()) > relayChunkPurgeAge {
					if os.Remove(filepath.Join(dir, f.Name())) == nil {
						n++
					}
				}
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		stale := time.Since(info.ModTime()) > relayChunkPurgeAge
		if !stale {
			// Fall back to the recorded creation time (renames can refresh
			// the directory mtime).
			if m, err := s.readChunkMeta(dir); err == nil {
				if created, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
					stale = time.Since(created) > relayChunkPurgeAge
				}
			}
		}
		if stale {
			if err := os.RemoveAll(dir); err == nil {
				n++
				s.log.Info("purged abandoned chunk staging", "uuid", e.Name())
			}
		}
	}
	return n
}

// RunMaintenanceNow runs every background maintenance pass synchronously and
// returns the per-loop counts (admin "run maintenance now" hook, §8.2).
func (s *Server) RunMaintenanceNow(ctx context.Context) map[string]int {
	report := map[string]int{}
	trashPurged := s.runMaintenance(ctx)
	report["trash_purged"] = trashPurged
	for k, v := range s.runRelayMaintenance(ctx) {
		report[k] = v
	}
	return report
}

// ptr is a tiny generic helper.
func ptr[T any](v T) *T { return &v }
