package agentapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/gateway"
	"cloud2api/internal/store"
)

// linkMaxTTL is the 7-day link cap (§4.3).
const linkMaxTTL = 7 * 24 * time.Hour

// resolveLinkSecret returns the link HMAC secret: the LINK_SECRET environment
// variable when set, else a random secret generated once and persisted in
// the settings table (§5.3).
func resolveLinkSecret(st *store.Store) []byte {
	if v := strings.TrimSpace(os.Getenv("LINK_SECRET")); v != "" {
		return []byte(v)
	}
	if v, ok := st.GetSetting("link_secret"); ok && v != "" {
		return []byte(v)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is fatal-worthy; a fixed fallback must not be
		// silently used, so panic loudly at startup instead.
		panic("link secret generation failed: " + err.Error())
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	if err := st.SetSetting("link_secret", secret); err == nil {
		return []byte(secret)
	}
	return []byte(secret)
}

// linkPayload is the HMAC-protected content of a link token.
type linkPayload struct {
	Drive string `json:"d"`
	Path  string `json:"p"` // full logical path inside the drive (post-chroot)
	KeyID string `json:"k"`
	Exp   int64  `json:"e"` // unix seconds
}

// mintLinkToken produces "<payload>.<sig>".
func (s *Server) mintLinkToken(p linkPayload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.linkSecret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	return payload + "." + sig, nil
}

// verifyLinkToken checks signature and expiry.
func (s *Server) verifyLinkToken(token string) (linkPayload, error) {
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 {
		return linkPayload{}, errors.New("malformed token")
	}
	payload, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, s.linkSecret)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return linkPayload{}, errors.New("invalid token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return linkPayload{}, errors.New("malformed token payload")
	}
	var p linkPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return linkPayload{}, errors.New("malformed token payload")
	}
	return p, nil
}

// handleLinkCreate implements POST /v1/links (§4.3).
func (s *Server) handleLinkCreate(c *call) {
	var body struct {
		Drive     string `json:"drive"`
		Path      string `json:"path"`
		ExpiresIn int64  `json:"expires_in"` // seconds
	}
	if !decodeBody(c, &body) {
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
	if body.Path == "" {
		c.fail(errInvalidRequest("field `path` is required"))
		return
	}
	if !c.key.CanOn(drive, auth.PermLink) {
		c.fail(errForbidden("missing \"link\" permission on drive \"" + drive + "\""))
		return
	}
	grant, granted := c.key.DriveGrant(drive)
	if !granted {
		c.fail(errDriveNotFound(drive, nil))
		return
	}
	c.drive = drive
	c.root = grant.Root
	// The storage guard runs before the existence fs/get: a drive whose
	// storage was replaced or is unverifiable must not even reveal whether a
	// path exists.
	if verdict := s.guard.Check(c.r.Context(), drive); !verdict.Allowed() {
		c.fail(driveUnavailableErr(drive, verdict.Message))
		return
	}
	full, internal, visible, e := s.resolve(c, body.Path)
	if e != nil {
		c.fail(e)
		return
	}
	c.setPath(visible)

	ttl := time.Duration(0)
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	} else {
		ttl = time.Hour
	}
	if ttl > linkMaxTTL {
		c.fail(errInvalidRequest("expires_in must not exceed 7 days (604800 seconds)"))
		return
	}

	ctx, cancel := context.WithTimeout(c.r.Context(), fsTimeout)
	defer cancel()
	if _, err := s.fsGet(ctx, internal); err != nil {
		if isNotFoundError(err) {
			c.fail(errPathNotFound(visible))
			return
		}
		c.fail(fsError(err))
		return
	}
	expires := time.Now().UTC().Add(ttl)
	token, err := s.mintLinkToken(linkPayload{Drive: drive, Path: full, KeyID: c.key.ID, Exp: expires.Unix()})
	if err != nil {
		c.fail(errUpstream("token mint failed"))
		return
	}
	_ = s.st.InsertLink(store.Link{
		Token: token, KeyID: c.key.ID, Drive: drive, Path: full,
		ExpiresAt: expires, CreatedAt: time.Now().UTC(),
	})
	c.ok(map[string]any{
		"url":        c.base + "/l/" + token,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// handleLinkDownload streams GET /l/{token} without an API key. Expired or
// revoked tokens answer 410 (§4.3).
func (s *Server) handleLinkDownload(w http.ResponseWriter, r *http.Request, token string) {
	rec := &respRecorder{ResponseWriter: w}
	c := &call{w: rec, r: r, base: s.baseURL(r), op: "link_download", start: time.Now()}
	defer func() { s.audit(c) }()

	failLink := func(status int, msg string) {
		e := newAPIError(status, CodeLinkExpired, msg)
		c.errMsg = e.Message
		e.withDocs(c.base)
		writeJSON(rec, status, map[string]any{"error": e})
	}
	failUpstream := func(rec *respRecorder, c *call, msg string) {
		e := errUpstream(msg)
		c.errMsg = e.Message
		e.withDocs(c.base)
		writeJSON(rec, e.status, map[string]any{"error": e})
	}

	p, err := s.verifyLinkToken(token)
	if err != nil {
		failLink(http.StatusGone, "this link is invalid")
		return
	}
	c.key_id_audit(p.KeyID)
	c.drive = p.Drive
	c.setPath(p.Path)
	if time.Now().Unix() >= p.Exp {
		failLink(http.StatusGone, "this link has expired")
		return
	}
	// Revoking or expiring the key kills its links too (§4.3).
	keyRow, err := s.keys.Get(p.KeyID)
	if err != nil {
		failLink(http.StatusGone, "this link is no longer valid")
		return
	}
	if keyRow.Revoked || (keyRow.ExpiresAt != nil && time.Now().After(*keyRow.ExpiresAt)) {
		failLink(http.StatusGone, "the key that created this link was revoked or expired")
		return
	}
	acct, known := s.registry.Get(p.Drive)
	if !known {
		failLink(http.StatusGone, "the drive of this link no longer exists")
		return
	}
	if !acct.Enabled {
		failLink(http.StatusGone, "the drive of this link is disabled")
		return
	}
	// Storage guard: never serve link bytes from a storage that replaced the
	// one the link was minted against without an administrator confirming it.
	if verdict := s.guard.Check(r.Context(), p.Drive); !verdict.Allowed() {
		e := newAPIError(http.StatusServiceUnavailable, CodeDriveUnavailable, verdict.Message)
		c.errMsg = e.Message
		e.withDocs(c.base)
		writeJSON(rec, e.status, map[string]any{"error": e})
		return
	}
	// Re-read the mount: the guard's reconcile hook may have followed a
	// rename during the check.
	acct, _ = s.registry.Get(p.Drive)
	internal, err := gateway.MountJoin(acct.MountPath, p.Path)
	if err != nil {
		failLink(http.StatusGone, "this link points outside its drive")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), streamTimeout)
	defer cancel()
	info, err := s.fsGet(ctx, internal)
	if err != nil {
		if isNotFoundError(err) {
			failLink(http.StatusGone, "the file of this link no longer exists")
		} else {
			e := fsError(err)
			c.errMsg = e.Message
			e.withDocs(c.base)
			writeJSON(rec, e.status, map[string]any{"error": e})
		}
		return
	}
	sign := info.Sign
	if sign == "" {
		sign, err = s.fetchSign(ctx, internal)
		if err != nil {
			e := fsError(err)
			c.errMsg = e.Message
			writeJSON(rec, e.status, map[string]any{"error": e})
			return
		}
	}
	q := url.Values{}
	q.Set("sign", sign)
	u := url.URL{Path: "/d" + internal, RawQuery: q.Encode()}
	hdr := http.Header{}
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range"} {
		if v := r.Header.Get(h); v != "" {
			hdr.Set(h, v)
		}
	}
	// DoDownload follows OpenList's /d direct-link 302 server-side so the
	// upstream URL stays behind the same-origin /l/{token} endpoint; failures
	// keep the generic message (a transport error would embed the redirect
	// target).
	resp, err := s.client.DoDownload(ctx, http.MethodGet, u, hdr)
	if err != nil {
		failUpstream(rec, c, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	if downloadRedirect(resp.StatusCode) || resp.StatusCode >= 400 {
		failUpstream(rec, c, "upstream returned "+resp.Status)
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			rec.Header().Set(h, v)
		}
	}
	rec.Header().Set("Cache-Control", "private, max-age=0")
	rec.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(rec, resp.Body)
	c.bytes = n
}

// key_id_audit records the creating key for the audit row.
func (c *call) key_id_audit(id string) { c.auditKeyID = id }
