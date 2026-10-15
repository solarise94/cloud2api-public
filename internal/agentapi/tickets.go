package agentapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud2api/internal/gateway"
	"cloud2api/internal/store"
)

// ticketTTL is the fixed lifetime of a large-download ticket (~120s). It is
// short on purpose: the ticket is a UI-only shortcut for a browser to stream a
// large file without buffering it in page memory, not a share. It is checked
// at request start; an already-open valid stream is not cut when the TTL
// passes mid-transfer.
const ticketTTL = 120 * time.Second

// ticketDomain separates ticket MACs from link MACs: both are HMAC-SHA256 over
// a base64url payload with the same secret, so without a distinct domain a
// link token would verify as a ticket (and vice versa). The domain prefix makes
// the two token families cryptographically non-interchangeable.
const ticketDomain = "c2a-download-ticket\x00"

// ticketPayload is the HMAC-protected content of a download ticket. Unlike a
// link, it binds the issuing key's credential generation (Cred) and the drive's
// storage identity (Stor) so that rotating the key material under the same key
// id, or swapping the backing storage, invalidates every outstanding ticket.
type ticketPayload struct {
	Kind  string `json:"t"` // always "tkt"; redundant with ticketDomain
	Drive string `json:"d"`
	Path  string `json:"p"` // full logical path inside the drive (post-chroot)
	Root  string `json:"r"` // chroot root bound at mint time
	KeyID string `json:"k"`
	Cred  string `json:"c"` // key credential tag (rotation detector)
	Stor  int64  `json:"s"` // recorded OpenList storage id of the drive (0 = unadopted)
	Exp   int64  `json:"e"` // unix seconds
}

// keyCredTag derives an opaque, secret-keyed tag of a key's stored hash. It is
// embedded in the (readable) token payload, so it must not reveal the hash: an
// HMAC under the instance secret is not reversible and is useless without the
// secret. When a key is rotated (RotateKeyHash changes the stored hash) the tag
// changes, so all tickets minted against the previous credential fail closed.
func (s *Server) keyCredTag(keyHash string) string {
	mac := hmac.New(sha256.New, s.linkSecret)
	mac.Write([]byte("c2a-key-cred\x00" + keyHash))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// mintTicketToken produces "<b64url(payload)>.<b64url(truncated HMAC)>".
func (s *Server) mintTicketToken(p ticketPayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.linkSecret)
	mac.Write([]byte(ticketDomain + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	return payload + "." + sig, nil
}

// verifyTicketToken checks the signature (ticket domain) and decodes the
// payload. Expiry and identity are checked by the caller against current state.
func (s *Server) verifyTicketToken(token string) (ticketPayload, error) {
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return ticketPayload{}, errors.New("malformed token")
	}
	payload, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, s.linkSecret)
	mac.Write([]byte(ticketDomain + payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return ticketPayload{}, errors.New("invalid token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ticketPayload{}, errors.New("malformed token payload")
	}
	var p ticketPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ticketPayload{}, errors.New("malformed token payload")
	}
	if p.Kind != "tkt" || p.KeyID == "" || p.Drive == "" || p.Path == "" {
		return ticketPayload{}, errors.New("malformed token payload")
	}
	return p, nil
}

// hasReadOn reports whether the persisted key may read on drive, using the same
// union rule as auth.Key.CanOn (a global read permission or a per-drive read
// grant). The grant is returned so the caller can compare its root.
func hasReadOn(k store.APIKey, drive string) (store.KeyDrive, bool) {
	var grant store.KeyDrive
	found := false
	for _, g := range k.Drives {
		if g.Drive == drive {
			grant, found = g, true
			break
		}
	}
	if !found {
		return store.KeyDrive{}, false
	}
	if contains(grant.Perms, "read") || contains(k.GlobalPerms, "read") {
		return grant, true
	}
	return grant, false
}

func contains(v []string, want string) bool {
	for _, s := range v {
		if s == want {
			return true
		}
	}
	return false
}

// handleDownloadTicket implements POST /v1/drives/{drive}/download-ticket.
// It requires the read permission (never link) and returns a short-lived,
// same-origin URL for the browser to stream natively.
func (s *Server) handleDownloadTicket(c *call) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeBody(c, &body) {
		return
	}
	if strings.TrimSpace(body.Path) == "" {
		c.fail(errInvalidRequest("field `path` is required"))
		return
	}
	full, internal, visible, e := s.resolve(c, body.Path)
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
			c.fail(errPathNotFound(visible))
			return
		}
		c.fail(fsError(err))
		return
	}
	if info.IsDir {
		c.fail(errInvalidRequest("path " + visible + " is a directory; tickets are for files only"))
		return
	}

	// Bind the drive's recorded storage identity (0 when the drive has never
	// been reconciled). Guard.Check at redemption re-verifies it fresh; this
	// recorded id additionally catches a rebind between mint and use.
	var stor int64
	if d, err := s.st.GetDrive(c.drive); err == nil {
		stor = d.StorageID
	}
	keyRow, err := s.keys.Get(c.key.ID)
	if err != nil {
		c.fail(errUnauthorized(""))
		return
	}
	expires := time.Now().UTC().Add(ticketTTL)
	token, err := s.mintTicketToken(ticketPayload{
		Kind: "tkt", Drive: c.drive, Path: full, Root: c.root,
		KeyID: c.key.ID, Cred: s.keyCredTag(keyRow.Hash), Stor: stor,
		Exp: expires.Unix(),
	})
	if err != nil {
		c.fail(errUpstream("ticket mint failed"))
		return
	}
	// The URL is relative same-origin by design: the browser streams natively
	// and the token is never placed in a query string or an access log.
	c.w.Header().Set("Cache-Control", "no-store")
	c.w.Header().Set("Referrer-Policy", "no-referrer")
	c.w.Header().Set("X-Content-Type-Options", "nosniff")
	c.ok(map[string]any{
		"url":        "/download/" + token,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// handleTicketDownload streams GET|HEAD /download/{token} without an API key.
// Every redemption re-checks current state (key revocation, expiry, credential
// generation, read grant, exact drive+root, drive enabled, storage identity and
// a fresh storage-guard verdict) so no change can be bypassed. Expired,
// malformed, tampered or no-longer-authorized tokens answer 410 with a generic
// message; the token is never logged.
func (s *Server) handleTicketDownload(w http.ResponseWriter, r *http.Request, token string) {
	rec := &respRecorder{ResponseWriter: w}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "ticket_download", start: time.Now()}
	defer func() { s.audit(c) }()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	failTicket := func(msg string) {
		e := newAPIError(http.StatusGone, CodeTicketInvalid, msg)
		c.errMsg = e.Code + ": " + e.Message
		e.withDocs(c.base)
		writeJSON(rec, http.StatusGone, map[string]any{"error": e})
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		e := newAPIError(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed; allow: GET, HEAD")
		c.errMsg = e.Code + ": " + e.Message
		e.withDocs(c.base)
		rec.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}

	p, err := s.verifyTicketToken(token)
	if err != nil {
		failTicket("this download ticket is invalid")
		return
	}
	c.auditKeyID = p.KeyID
	c.drive = p.Drive
	c.setPath(ticketVisible(p))
	if time.Now().Unix() >= p.Exp {
		failTicket("this download ticket has expired")
		return
	}

	keyRow, err := s.keys.Get(p.KeyID)
	if err != nil {
		failTicket("this download ticket is no longer valid")
		return
	}
	if keyRow.Revoked || (keyRow.ExpiresAt != nil && time.Now().After(*keyRow.ExpiresAt)) {
		failTicket("this download ticket is no longer valid")
		return
	}
	// Credential generation: rotating the key material under the same id
	// changes the stored hash and therefore the tag, invalidating old tickets.
	if s.keyCredTag(keyRow.Hash) != p.Cred {
		failTicket("this download ticket is no longer valid")
		return
	}
	grant, ok := hasReadOn(keyRow, p.Drive)
	if !ok {
		failTicket("this download ticket is no longer valid")
		return
	}
	if normalizeRoot(grant.Root) != normalizeRoot(p.Root) {
		failTicket("this download ticket is no longer valid")
		return
	}
	// Recorded drive identity must still exist and carry the same storage id
	// (0 = drive never adopted). A drive row removed from cloud2api's own store
	// fails closed, exactly like a changed storage binding.
	d, err := s.st.GetDrive(p.Drive)
	if err != nil || d.StorageID != p.Stor {
		failTicket("this download ticket is no longer valid")
		return
	}

	acct, known := s.registry.Get(p.Drive)
	if !known {
		failTicket("this download ticket is no longer valid")
		return
	}
	if !acct.Enabled {
		failTicket("this download ticket is no longer valid")
		return
	}
	// Rate limit with the issuing key's bucket, exactly like a normal download
	// (nil = unlimited, matching the /v1 path).
	rpm := 0
	if keyRow.Limits.RequestsPerMinute != nil {
		rpm = *keyRow.Limits.RequestsPerMinute
	}
	if ok, wait := s.limiter.allow(p.KeyID, rpm); !ok {
		c.fail(errRateLimited(wait))
		return
	}
	// Fresh storage verification: a replaced or unverifiable storage must not
	// serve ticket bytes (same policy as /l/ links and /v1 downloads).
	if verdict := s.guard.Check(r.Context(), p.Drive); !verdict.Allowed() {
		e := newAPIError(http.StatusServiceUnavailable, CodeDriveUnavailable, verdict.Message)
		c.errMsg = e.Code + ": " + e.Message
		e.withDocs(c.base)
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}
	// Re-read the mount (the guard reconcile hook may have followed a rename).
	acct, _ = s.registry.Get(p.Drive)
	internal, err := gateway.MountJoin(acct.MountPath, p.Path)
	if err != nil {
		failTicket("this download ticket is no longer valid")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), streamTimeout)
	defer cancel()
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			failTicket("the file of this download ticket no longer exists")
		} else {
			e := fsError(err)
			c.errMsg = e.Code + ": " + e.Message
			e.withDocs(c.base)
			writeJSON(rec, e.status, map[string]any{"error": e})
		}
		return
	}
	if info.IsDir {
		failTicket("the file of this download ticket no longer exists")
		return
	}
	sign := info.Sign
	if sign == "" {
		sign, err = s.fetchSign(ctx, internal)
		if err != nil {
			e := fsError(err)
			c.errMsg = e.Code + ": " + e.Message
			e.withDocs(c.base)
			writeJSON(rec, e.status, map[string]any{"error": e})
			return
		}
	}
	q := url.Values{}
	q.Set("sign", sign)
	u := url.URL{Path: "/d" + internal, RawQuery: q.Encode()}
	hdr := http.Header{}
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	// DoDownload fetches /d and follows OpenList's direct-link 302
	// server-side, so the upstream URL never reaches the browser; a transport
	// failure is answered with the generic message below (never err.Error,
	// which would embed the upstream redirect target).
	resp, err := s.client.DoDownload(ctx, http.MethodGet, u, hdr)
	if err != nil {
		e := errUpstream("upstream request failed")
		c.errMsg = e.Code + ": " + e.Message
		e.withDocs(c.base)
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}
	defer resp.Body.Close()

	// A range past EOF is a legitimate client condition: preserve 416 and its
	// `Content-Range: bytes */N` (plus Accept-Ranges) without leaking any raw
	// upstream error body. Every other upstream failure maps to a typed 502.
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		for _, h := range []string{"Content-Range", "Accept-Ranges", "Content-Type"} {
			if v := resp.Header.Get(h); v != "" {
				rec.Header().Set(h, v)
			}
		}
		rec.Header().Set("Cache-Control", "no-store")
		rec.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		c.errMsg = "range_not_satisfiable"
		return
	}
	// A redirect the helper should already have followed server-side is an
	// upstream failure: forwarding it would leak the direct-link URL through
	// Location, so it maps to the generic typed error like any other 4xx/5xx.
	if downloadRedirect(resp.StatusCode) || (resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotModified) {
		e := errUpstream("upstream returned " + resp.Status)
		c.errMsg = e.Code + ": " + e.Message
		e.withDocs(c.base)
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			rec.Header().Set(h, v)
		}
	}
	rec.Header().Set("Content-Disposition", contentDisposition(info.Name, p.Path))
	rec.Header().Set("Cache-Control", "no-store")
	// Conditional 304 carries no body; forward it as-is.
	rec.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead || resp.StatusCode == http.StatusNotModified {
		return
	}

	// Enforce the transfer length so a short upstream (chunked without a
	// Content-Length, or a truncated body) cannot masquerade as a complete
	// download. The trusted fsGet size backs a full 200 when the upstream omits
	// Content-Length; a 206 is bounded by Content-Length or the parsed
	// Content-Range. No whole-file buffering happens here.
	want := int64(-1)
	switch {
	case resp.ContentLength >= 0:
		want = resp.ContentLength
	case resp.StatusCode == http.StatusOK && info.Size >= 0:
		want = info.Size
	case resp.StatusCode == http.StatusPartialContent:
		want = contentRangeLength(resp.Header.Get("Content-Range"))
	}
	n, copyErr := streamExact(rec, resp.Body, want)
	c.bytes = n
	if copyErr != nil {
		// Headers are already committed: a JSON error can no longer be sent.
		// Record the failure for audit, then abort the transport so the client
		// (and the browser's native download) observes a broken transfer rather
		// than a clean EOF that would be saved as a complete file.
		c.errMsg = "stream_interrupted: " + copyErr.Error()
		panic(http.ErrAbortHandler)
	}
}

// downloadRedirect reports whether status is one of the redirect codes the
// openlist DoDownload helper follows server-side. Seeing one in a response
// means the direct-link hop was not taken: the status is surfaced as a
// generic upstream failure instead of being forwarded with its Location.
func downloadRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// streamExact copies src to dst, requiring exactly want bytes when want >= 0
// (detecting both a short read and trailing extra bytes) and otherwise copying
// to EOF. It never buffers the payload.
func streamExact(dst io.Writer, src io.Reader, want int64) (int64, error) {
	if want < 0 {
		return io.Copy(dst, src)
	}
	n, err := io.Copy(dst, io.LimitReader(src, want))
	if err != nil {
		return n, err
	}
	if n != want {
		return n, io.ErrUnexpectedEOF
	}
	var extra [1]byte
	if m, _ := src.Read(extra[:]); m > 0 {
		return n, errors.New("upstream sent more bytes than declared")
	}
	return n, nil
}

// contentRangeLength parses "bytes <start>-<end>/<total>" into the byte count
// (end-start+1); -1 when the header is absent or malformed (the length is then
// simply unenforced).
func contentRangeLength(cr string) int64 {
	cr = strings.TrimSpace(cr)
	if !strings.HasPrefix(cr, "bytes ") {
		return -1
	}
	spec := strings.TrimPrefix(cr, "bytes ")
	spec, _, _ = strings.Cut(spec, "/")
	start, end, ok := strings.Cut(spec, "-")
	if !ok {
		return -1
	}
	a, err1 := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	b, err2 := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
	if err1 != nil || err2 != nil || b < a {
		return -1
	}
	return b - a + 1
}

// ticketVisible recovers the agent-visible path for the audit row from the
// bound full path and root.
func ticketVisible(p ticketPayload) string {
	root := normalizeRoot(p.Root)
	if root == "/" {
		return p.Path
	}
	if v := strings.TrimPrefix(p.Path, root); v != "" {
		return v
	}
	return "/"
}

// normalizeRoot canonicalizes a chroot root ("", "/" and "/x/" all coincide).
func normalizeRoot(root string) string {
	return path.Clean("/" + root)
}

// contentDisposition builds a safe attachment header. mime.FormatMediaType
// handles quoting, CRLF injection and non-ASCII names (RFC 2231 filename*),
// which a hand-built `attachment; filename="..."` cannot; the plain sanitized
// name is only a fallback for the rare value FormatMediaType refuses.
func contentDisposition(name, fullPath string) string {
	if name == "" {
		name = path.Base(fullPath)
	}
	if name == "" || name == "/" || name == "." {
		name = "download"
	}
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": name}); cd != "" {
		return cd
	}
	return `attachment; filename="` + sanitizeFilename(name) + `"`
}
