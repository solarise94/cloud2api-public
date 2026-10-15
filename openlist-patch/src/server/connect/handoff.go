package connect

// The handoff flow lets an authorized backend (cloud2api) send an already
// authenticated administrator into this setup page without a second OpenList
// password prompt — and without ever handing the browser a general OpenList
// credential:
//
//  1. POST /api/admin/connect/handoff/start (upstream bearer admin auth only,
//     never the scoped cookie) mints a random single-use ticket, bound to the
//     minting admin and her current PwdTS, that lives at most 60 seconds in
//     this process's memory.
//  2. The browser opens the setup page with the ticket in the URL fragment
//     (fragments never reach the server or the Referer header) and posts it
//     once to the public POST /api/connect/handoff/exchange from the same
//     origin. The response carries metadata only; the credential itself is an
//     HttpOnly SameSite=Strict cookie with Path basePath()+/api/admin/connect.
//  3. The scoped session is process-memory only, expires absolutely after 30
//     minutes, keeps a per-cookie random owner fingerprint, and is re-checked
//     against the live user row (admin, not disabled, unchanged PwdTS) on
//     every request.
//
// Invariants: the scoped cookie authenticates only the routes of the
// /api/admin/connect group — nothing else, and it can never mint a ticket,
// not even next to a bearer header. Cookie-authenticated requests are
// browser-only: every one is Fetch-Metadata checked, and every mutation
// additionally requires a strict same-origin Origin plus a JSON content type.
// The legacy bearer chain keeps working unchanged; a valid scoped cookie wins
// over an Authorization header so the header can never override a wizard
// session.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

const (
	handoffTicketTTL      = 60 * time.Second
	handoffSessionTTL     = 30 * time.Minute
	handoffSessionSeconds = int(handoffSessionTTL / time.Second)

	// connectCookieBase prefixes the deployed cookie name; see
	// connectCookieNameFor for why the name is site-scoped.
	connectCookieBase = "connect_session"

	maxTickets        = 256
	maxTicketsPerUser = 8
	maxSessions       = 512

	ticketBytes      = 32
	sessionBytes     = 32
	fingerprintBytes = 32
)

// gin context keys for the scoped-cookie branch of the auth chain.
const (
	// cKeyScopedDone marks a request authenticated by the scoped cookie; the
	// bearer middlewares are skipped for it and handoff/start refuses it.
	cKeyScopedDone = "connect_scoped_done"
	// cKeyOwnerFP carries the per-cookie owner fingerprint ownerFrom uses
	// instead of hashing the Authorization header.
	cKeyOwnerFP = "connect_owner_fp"
)

// handoffTicket is a minted, single-use start ticket.
type handoffTicket struct {
	userID  uint
	pwdTS   int64
	expires time.Time
}

// connectSession is one scoped cookie's server-side state.
type connectSession struct {
	userID      uint
	username    string
	pwdTS       int64
	fingerprint string
	expires     time.Time
}

// handoffStore holds the process-memory ticket and session state. now is
// injectable so tests can walk expiry without sleeping.
type handoffStore struct {
	mu       sync.Mutex
	now      func() time.Time
	tickets  map[string]*handoffTicket
	sessions map[string]*connectSession
}

func newHandoffStore() *handoffStore {
	return &handoffStore{
		now:      time.Now,
		tickets:  make(map[string]*handoffTicket),
		sessions: make(map[string]*connectSession),
	}
}

// randomToken returns a fresh base64url (unpadded) string of n random bytes.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pruneLocked drops expired entries. Caller holds h.mu. An entry at exactly
// its expiry instant counts as expired.
func (h *handoffStore) pruneLocked() {
	now := h.now()
	for code, t := range h.tickets {
		if !now.Before(t.expires) {
			delete(h.tickets, code)
		}
	}
	for val, s := range h.sessions {
		if !now.Before(s.expires) {
			delete(h.sessions, val)
		}
	}
}

// startTicket mints one ticket for (userID, pwdTS). Bounded: per-user first,
// then global, both after a pruning pass; over the limit is a 429, never
// growth.
func (h *handoffStore) startTicket(userID uint, pwdTS int64) (string, error) {
	code, err := randomToken(ticketBytes)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	perUser := 0
	for _, t := range h.tickets {
		if t.userID == userID {
			perUser++
		}
	}
	if perUser >= maxTicketsPerUser || len(h.tickets) >= maxTickets {
		return "", loginsdk.Errorf(loginsdk.CodeLimit, "移交票据过多，请稍后再试")
	}
	h.tickets[code] = &handoffTicket{userID: userID, pwdTS: pwdTS, expires: h.now().Add(handoffTicketTTL)}
	return code, nil
}

// consumeTicket atomically deletes and returns the ticket: single use even
// under concurrent exchange; unknown, reused, expired and PwdTS-invalidated
// tickets are indistinguishable. At the exact expiry instant the ticket is
// already expired.
func (h *handoffStore) consumeTicket(code string) (userID uint, pwdTS int64, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, found := h.tickets[code]
	if !found {
		return 0, 0, false
	}
	delete(h.tickets, code)
	if !h.now().Before(t.expires) {
		return 0, 0, false
	}
	return t.userID, t.pwdTS, true
}

// createSession registers a scoped session and returns the cookie value.
func (h *handoffStore) createSession(userID uint, username string, pwdTS int64) (value, fingerprint string, err error) {
	value, err = randomToken(sessionBytes)
	if err != nil {
		return "", "", err
	}
	fingerprint, err = randomToken(fingerprintBytes)
	if err != nil {
		return "", "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	if len(h.sessions) >= maxSessions {
		return "", "", loginsdk.Errorf(loginsdk.CodeLimit, "连接会话过多，请稍后再试")
	}
	h.sessions[value] = &connectSession{
		userID:      userID,
		username:    username,
		pwdTS:       pwdTS,
		fingerprint: fingerprint,
		expires:     h.now().Add(handoffSessionTTL),
	}
	return value, fingerprint, nil
}

// getSession looks a session up by cookie value; at-expiry counts as expired.
func (h *handoffStore) getSession(value string) (*connectSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[value]
	if !ok {
		return nil, false
	}
	if !h.now().Before(s.expires) {
		delete(h.sessions, value)
		return nil, false
	}
	return s, true
}

// revoke deletes a session (logout). Unknown values are a no-op.
func (h *handoffStore) revoke(value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, value)
}

// remainingSeconds reports the session's remaining lifetime in whole seconds
// on the store's clock; at or past expiry it is 0.
func (h *handoffStore) remainingSeconds(expires time.Time) int {
	d := expires.Sub(h.now())
	if d <= 0 {
		return 0
	}
	return int(d / time.Second)
}

// ---- cookie helpers ----

// connectCookieNameFor derives the deployed cookie name from the request host
// (which carries the port) and the site base path: host-only cookies are not
// port-scoped, so two OpenList instances sharing one hostname on different
// ports must not collide in the browser's cookie jar.
func connectCookieNameFor(c *gin.Context) string {
	base := basePath()
	host := ""
	if c.Request != nil {
		host = c.Request.Host
	}
	sum := sha256.Sum256([]byte(host + "\x00" + base))
	return connectCookieBase + "_" + hex.EncodeToString(sum[:4])
}

// cookiePath is the cookie's Path attribute: exactly the connect admin API
// under the site's base path — the only routes the cookie is authorized for,
// including /auth/me and /auth/logout inside that group.
func cookiePath() string {
	if base := basePath(); base != "" {
		return base + "/api/admin/connect"
	}
	return "/api/admin/connect"
}

// setConnectCookie writes the scoped session cookie: HttpOnly, SameSite=Strict,
// host-only (no domain attribute), Secure on https requests, absolute Max-Age.
func setConnectCookie(c *gin.Context, value string) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(connectCookieNameFor(c), value, handoffSessionSeconds, cookiePath(), "", requestScheme(c) == "https", true)
}

// clearConnectCookie expires the cookie in the browser.
func clearConnectCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(connectCookieNameFor(c), "", -1, cookiePath(), "", requestScheme(c) == "https", true)
}

// requestScheme resolves the request's own scheme for security decisions:
// native TLS, the documented X-Forwarded-Proto proxy signal of a
// TLS-terminating proxy, or an https site URL. X-Forwarded-Host is never
// consulted.
func requestScheme(c *gin.Context) string {
	if c.Request != nil && c.Request.TLS != nil {
		return "https"
	}
	if c.GetHeader("X-Forwarded-Proto") == "https" {
		return "https"
	}
	if strings.HasPrefix(strings.ToLower(conf.Conf.SiteURL), "https://") {
		return "https"
	}
	return "http"
}

// hostPort splits a Host/Origin authority into lowered host and port.
func hostPort(authority string) (host, port string) {
	authority = strings.ToLower(strings.TrimSpace(authority))
	if i := strings.LastIndexByte(authority, ']'); i >= 0 { // [v6]:port
		if len(authority) > i+1 && authority[i+1] == ':' {
			return authority[:i+1], authority[i+2:]
		}
		return authority, ""
	}
	if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		return authority[:i], authority[i+1:]
	}
	return authority, ""
}

// sameAuthority compares two authorities of the given schemes; a missing port
// means the scheme's default port, so https:443 never equals http's default.
func sameAuthority(a, aScheme, b, bScheme string) bool {
	ha, pa := hostPort(a)
	hb, pb := hostPort(b)
	if ha != hb {
		return false
	}
	defaultPort := func(scheme, p string) string {
		if p != "" {
			return p
		}
		if scheme == "https" {
			return "443"
		}
		return "80"
	}
	return defaultPort(aScheme, pa) == defaultPort(bScheme, pb)
}

// originAllowed enforces the strict same-origin rule of cookie mutations:
// exactly one Origin header, well formed (http(s), no userinfo, no path — not
// even a bare "/", no query, fragment or opaque part, never "null"), scheme
// equal to the request's resolved scheme, and the authority equal to the
// request's own Host.
func originAllowed(c *gin.Context) bool {
	origins := c.Request.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin := strings.TrimSpace(origins[0])
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// Origin serialization has no path/query/fragment; reject them outright,
	// including the bare forms that url.Parse erases ("/", "?", "#").
	if u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") {
		return false
	}
	scheme := requestScheme(c)
	if u.Scheme != scheme {
		return false
	}
	return sameAuthority(u.Host, u.Scheme, c.Request.Host, scheme)
}

// requireMutationOrigin rejects requests without a strict same-origin Origin.
func requireMutationOrigin(c *gin.Context) bool {
	if originAllowed(c) {
		return true
	}
	c.Header("Cache-Control", "no-store")
	common.ErrorStrResp(c, "跨站或来源不明的请求已被拒绝", http.StatusForbidden)
	c.Abort()
	return false
}

// ---- browser-request checks for the scoped cookie ----

// isMutatingMethod reports whether the method can change state.
func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// fetchSiteAllowed applies Fetch Metadata to cookie-authenticated requests:
// when the browser sends Sec-Fetch-Site it must be same-origin (or absent for
// non-browser clients). Same-site cross-port pages are refused too —
// SameSite=Strict alone would still serve them.
func fetchSiteAllowed(c *gin.Context) bool {
	switch c.GetHeader("Sec-Fetch-Site") {
	case "":
		return true
	case "same-origin", "none":
		return true
	default:
		return false
	}
}

// hasRequestBody reports whether the request carries a body.
func hasRequestBody(c *gin.Context) bool {
	return c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0
}

// jsonContentType reports whether the request declares a JSON media type.
func jsonContentType(c *gin.Context) bool {
	ct := c.Request.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// cookieMutationAllowed is the CSRF gate for cookie-authenticated mutations:
// strict same-origin Origin plus a JSON content type when a body is carried.
func cookieMutationAllowed(c *gin.Context) bool {
	if !originAllowed(c) {
		return false
	}
	if hasRequestBody(c) && !jsonContentType(c) {
		return false
	}
	return true
}

// ---- scoped user validation ----

// connectSessionUser re-checks the session's user against the live row: the
// account must still exist, still be an admin, not be disabled and have an
// unchanged password (PwdTS) since the cookie was minted.
func connectSessionUser(sess *connectSession) (*model.User, error) {
	u, err := db.GetUserById(sess.userID)
	if err != nil || u == nil {
		return nil, errors.New("user not found")
	}
	if u.Username != sess.username {
		return nil, errors.New("user renamed")
	}
	if !u.IsAdmin() {
		return nil, errors.New("no longer admin")
	}
	if u.Disabled {
		return nil, errors.New("user disabled")
	}
	if u.PwdTS != sess.pwdTS {
		return nil, errors.New("password changed")
	}
	return u, nil
}

// ---- auth chain ----

// scopedCookieAuth is the first element of the module's auth chain. Without a
// cookie it falls through to the legacy bearer middlewares untouched; with a
// valid scoped cookie it authenticates the request itself, applies the
// browser-only checks (Fetch Metadata on everything, same-origin + JSON on
// every mutation) and marks it so the bearer middlewares are skipped — the
// cookie wins, a header must never override a wizard session — and so
// handoff/start can refuse to mint. A stale cookie value is cleared and
// ignored; a cookie whose user no longer passes the live checks is rejected
// outright.
func (s *server) scopedCookieAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		value, err := c.Cookie(connectCookieNameFor(c))
		if err != nil || value == "" {
			c.Next()
			return
		}
		sess, ok := s.hf.getSession(value)
		if !ok {
			clearConnectCookie(c)
			c.Next()
			return
		}
		user, err := connectSessionUser(sess)
		if err != nil {
			s.hf.revoke(value)
			clearConnectCookie(c)
			c.Header("Cache-Control", "no-store")
			common.ErrorStrResp(c, "连接会话已失效，请重新从 cloud2api 进入或重新登录", http.StatusUnauthorized)
			c.Abort()
			return
		}
		if !fetchSiteAllowed(c) || (isMutatingMethod(c.Request.Method) && !cookieMutationAllowed(c)) {
			c.Header("Cache-Control", "no-store")
			common.ErrorStrResp(c, "跨站或来源不明的请求已被拒绝", http.StatusForbidden)
			c.Abort()
			return
		}
		common.GinAppendValues(c, conf.UserKey, user)
		c.Set(cKeyScopedDone, true)
		c.Set(cKeyOwnerFP, sess.fingerprint)
		c.Next()
	}
}

// skipIfScoped wraps one legacy auth middleware: requests already
// authenticated by the scoped cookie continue past it unchanged.
func skipIfScoped(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetBool(cKeyScopedDone) {
			c.Next()
			return
		}
		next(c)
	}
}

// scopedChain prepends the cookie branch to the configured admin auth chain.
func scopedChain(s *server, adminAuth []gin.HandlerFunc) []gin.HandlerFunc {
	chain := make([]gin.HandlerFunc, 0, len(adminAuth)+1)
	chain = append(chain, s.scopedCookieAuth())
	for _, a := range adminAuth {
		chain = append(chain, skipIfScoped(a))
	}
	return chain
}

// ---- handlers ----

type handoffStartResp struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"`
}

// handleHandoffStart mints a ticket. Bearer admin auth only: a request that
// arrived on the scoped cookie is refused even when a bearer header rides
// along, so a browser session can never mint further tickets.
func (s *server) handleHandoffStart(c *gin.Context) {
	if c.GetBool(cKeyScopedDone) {
		c.Header("Cache-Control", "no-store")
		common.ErrorStrResp(c, "连接会话不能签发移交票据", http.StatusForbidden)
		return
	}
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	user, _ := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user == nil {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeInvalidInput, "未识别的管理员身份"))
		return
	}
	code, err := s.hf.startTicket(owner.UserID, user.PwdTS)
	if err != nil {
		sdkErr(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	common.SuccessResp(c, handoffStartResp{Code: code, ExpiresIn: int(handoffTicketTTL / time.Second)})
}

type handoffExchangeReq struct {
	Code string `json:"code"`
}

type handoffExchangeResp struct {
	Username  string `json:"username"`
	ExpiresIn int    `json:"expires_in"`
}

// handleHandoffExchange trades a minted ticket for a scoped cookie. Public
// route, but strictly same-origin, JSON-only, single-use, short-lived and
// bound to the admin's PwdTS at mint time; the response carries metadata only
// — the credential is the cookie.
func (s *server) handleHandoffExchange(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !requireMutationOrigin(c) {
		return
	}
	if !jsonContentType(c) {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请求必须是 application/json"))
		return
	}
	var req handoffExchangeReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	rawUserID, rawPwdTS, ok := s.hf.consumeTicket(strings.TrimSpace(req.Code))
	if !ok {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeExpired, "移交链接已失效或已被使用"))
		return
	}
	u, err := db.GetUserById(uint(rawUserID))
	if err != nil || u == nil || !u.IsAdmin() || u.Disabled || u.PwdTS != rawPwdTS {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeExpired, "移交链接已失效或已被使用"))
		return
	}
	// Multi-tab: when this browser already holds a valid scoped session for
	// the same admin, keep it — its owner fingerprint and original absolute
	// expiry stay intact, so wizard sessions opened in other tabs of the same
	// browser remain resumable. The ticket is consumed either way; the
	// response reports the remaining lifetime (never a renewed 30 minutes).
	// A new cookie is minted only when none, a stale one, or a different
	// admin's session is present.
	if value, err := c.Cookie(connectCookieNameFor(c)); err == nil && value != "" {
		if sess, ok := s.hf.getSession(value); ok &&
			sess.userID == u.ID && sess.username == u.Username && sess.pwdTS == u.PwdTS {
			if remaining := s.hf.remainingSeconds(sess.expires); remaining > 0 {
				common.SuccessResp(c, handoffExchangeResp{Username: u.Username, ExpiresIn: remaining})
				return
			}
		}
	}
	value, _, err := s.hf.createSession(u.ID, u.Username, u.PwdTS)
	if err != nil {
		sdkErr(c, err)
		return
	}
	setConnectCookie(c, value)
	common.SuccessResp(c, handoffExchangeResp{Username: u.Username, ExpiresIn: handoffSessionSeconds})
}

// handleAuthMe answers with the authenticated admin's identity so the page
// never needs the upstream /api/me. scope tells the page which credential is
// active: the scoped cookie ("connect") or the legacy bearer token.
func (s *server) handleAuthMe(c *gin.Context) {
	user, ok := c.Request.Context().Value(conf.UserKey).(*model.User)
	if !ok || user == nil {
		c.Header("Cache-Control", "no-store")
		common.ErrorStrResp(c, "未识别的管理员身份", http.StatusUnauthorized)
		return
	}
	scope := "bearer"
	if c.GetBool(cKeyScopedDone) {
		scope = "connect"
	}
	c.Header("Cache-Control", "no-store")
	common.SuccessResp(c, gin.H{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
		"scope":    scope,
	})
}

// handleAuthLogout revokes the scoped session and expires the cookie; on the
// bearer path it mirrors the upstream logout and invalidates the token.
// Mutation: a strict same-origin Origin is required in both modes.
func (s *server) handleAuthLogout(c *gin.Context) {
	if !requireMutationOrigin(c) {
		return
	}
	c.Header("Cache-Control", "no-store")
	if value, err := c.Cookie(connectCookieNameFor(c)); err == nil && value != "" {
		s.hf.revoke(value)
	}
	if !c.GetBool(cKeyScopedDone) {
		_ = common.InvalidateToken(c.GetHeader("Authorization"))
	}
	clearConnectCookie(c)
	common.SuccessResp(c, nil)
}
