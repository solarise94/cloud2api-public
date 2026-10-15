// Package openlist provides a thin REST client for the OpenList API. It
// owns the endpoint and admin token and supports both buffered JSON calls
// and streaming request/response bodies (DESIGN.md §7).
//
// The admin token can be swapped at runtime (SetToken) and, when OpenList
// credentials are configured (SetCredentials), the client can heal an
// invalidated token itself: re-login via /api/auth/login, re-read the
// permanent settings token via /api/admin/setting/get?key=token, swap it in
// atomically and retry the failed request once (v0.3.1, fixes the 48h JWT
// expiry of login tokens).
package openlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds metadata/JSON calls (DESIGN.md §7).
	DefaultTimeout = 120 * time.Second
	// RelaxedTimeout bounds streaming payload transfers (uploads, raw
	// downloads), which may legitimately run longer than DefaultTimeout.
	RelaxedTimeout = 30 * time.Minute
)

// healCooldown bounds how often a failing re-login is retried, so a wrong
// password cannot turn every request into a login attempt.
const healCooldown = 10 * time.Second

// hopHeader mirrors openlist-patch pkg/goapiclient.HopHeader (the two
// modules cannot import each other).
const hopHeader = "X-GoAPI-Hop"

// hopContextKey carries the validated relay hop count of the agent request.
type hopContextKey struct{}

// WithHop returns a context carrying the relay hop count; NewRequest
// forwards it to OpenList as X-GoAPI-Hop so its GoAPI driver can add one on
// its own outbound calls and relay chains stay bounded end to end.
func WithHop(ctx context.Context, hop int) context.Context {
	if hop <= 0 {
		return ctx
	}
	return context.WithValue(ctx, hopContextKey{}, hop)
}

func hopFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	h, _ := ctx.Value(hopContextKey{}).(int)
	if h < 0 {
		return 0
	}
	return h
}

type Client struct {
	base *url.URL

	tokenMu sync.RWMutex
	token   string

	hc *http.Client

	credsMu sync.RWMutex
	creds   *credentials

	healMu     sync.Mutex
	healFailed time.Time // last unsuccessful heal attempt (for cooldown)
}

type credentials struct {
	username string
	password string
}

func New(endpoint, token string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse openlist endpoint: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("openlist endpoint %q must be an absolute http(s) URL", endpoint)
	}
	c := &Client{
		base:  u,
		token: token,
		hc: &http.Client{
			// No overall timeout: callers bound every request through its
			// context. Upstream redirects are passed through to the client
			// instead of being followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ResponseHeaderTimeout: DefaultTimeout,
			},
		},
	}
	return c, nil
}

// Token returns the current admin token.
func (c *Client) Token() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.token
}

// SetToken atomically replaces the admin token. It is safe for concurrent
// use with in-flight requests: they keep the token they were built with.
func (c *Client) SetToken(token string) {
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
}

// SetCredentials enables token self-heal: when OPENLIST-style credentials
// are known, a 401 from OpenList triggers a re-login and a swap to the
// permanent settings token. Empty values disable healing.
func (c *Client) SetCredentials(username, password string) {
	c.credsMu.Lock()
	defer c.credsMu.Unlock()
	if username == "" || password == "" {
		c.creds = nil
		return
	}
	c.creds = &credentials{username: username, password: password}
}

// NewRequest builds a request against the OpenList endpoint. u carries only
// path and query; scheme and host are filled in from the endpoint. The
// OpenList token replaces any Authorization header in header.
func (c *Client) NewRequest(ctx context.Context, method string, u url.URL, body io.Reader, contentLength int64, header http.Header) (*http.Request, error) {
	u.Scheme = c.base.Scheme
	u.Host = c.base.Host
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if header != nil {
		req.Header = header.Clone()
	}
	if hop := hopFromContext(ctx); hop > 0 {
		req.Header.Set(hopHeader, strconv.Itoa(hop))
	}
	req.Header.Set("Authorization", c.Token())
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	return req, nil
}

// Do executes the request.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.hc.Do(req)
}

// DoAutoHeal executes req and, when OpenList rejects the token, healing is
// enabled and the request body is replayable, it re-logins, swaps in the
// permanent settings token and retries the request exactly once. Callers
// with non-replayable (streaming) bodies must use Do instead; for those
// requests the rejection is returned as-is.
//
// OpenList signals an invalidated token either as HTTP 401 or — as observed
// on the live instance — as HTTP 200 with an error envelope {"code":401}.
// Both are detected. Only JSON responses are inspected for the envelope, so
// streamed download bodies are never buffered (a 64 KiB prefix peek is
// reattached before the response is returned).
func (c *Client) DoAutoHeal(req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil {
		return resp, err
	}
	invalid, resp, err := c.inspectTokenInvalid(resp)
	if err != nil {
		return nil, err
	}
	if !invalid || !c.healingEnabled() || !replayable(req) {
		return resp, nil
	}
	// The token that actually failed is the one the request carries —
	// c.Token() may already have been swapped by a concurrent healer.
	failed := req.Header.Get("Authorization")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err := c.heal(req.Context(), failed); err != nil {
		return nil, fmt.Errorf("openlist token self-heal failed: %w", err)
	}
	// Reset both the body (if any) and the Authorization header: the request
	// was built with the stale token.
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	req.Header.Set("Authorization", c.Token())
	return c.Do(req)
}

// tokenInvalidPeek is the prefix size inspected for the {"code":401}
// envelope; OpenList puts code first, so this is far more than enough.
const tokenInvalidPeek = 64 << 10

// inspectTokenInvalid reports whether resp is an OpenList token rejection.
// Non-rejections get their body restored: up to the peeked prefix is
// reattached in front of the live body, so streaming callers are unaffected.
func (c *Client) inspectTokenInvalid(resp *http.Response) (bool, *http.Response, error) {
	if resp.StatusCode == http.StatusUnauthorized {
		return true, resp, nil
	}
	if resp.Body == nil {
		return false, resp, nil
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		// Not an API envelope (e.g. a streamed download): never buffered.
		return false, resp, nil
	}
	buf := make([]byte, tokenInvalidPeek)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, resp, err
	}
	buf = buf[:n]
	restored := *resp
	restored.Body = &reappendReader{prefix: buf, rest: resp.Body}
	// The restored copy serves both paths: without healing the caller reads
	// the reassembled body; with healing it is drained and closed.
	if !looksLikeTokenInvalid(buf) {
		return false, &restored, nil
	}
	return true, &restored, nil
}

// looksLikeTokenInvalid checks the buffered prefix for the OpenList error
// envelope with code 401. The envelope starts with the code field, so the
// exact-JSON check succeeds whenever the body is complete; the contains
// fallback covers a truncated prefix.
func looksLikeTokenInvalid(buf []byte) bool {
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(buf, &envelope); err == nil {
		return envelope.Code == http.StatusUnauthorized
	}
	return bytes.Contains(buf, []byte(`"code":401`))
}

// reappendReader serves the already-read prefix bytes first, then the
// remainder of the original body.
type reappendReader struct {
	prefix []byte
	rest   io.Reader
}

func (r *reappendReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.rest.Read(p)
}

func (r *reappendReader) Close() error {
	if closer, ok := r.rest.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// healingEnabled reports whether credentials are configured.
func (c *Client) healingEnabled() bool {
	c.credsMu.RLock()
	defer c.credsMu.RUnlock()
	return c.creds != nil
}

// replayable reports whether req can be sent again after a heal. Requests
// without a body trivially can; buffered bodies can via GetBody.
func replayable(req *http.Request) bool {
	return req.Body == nil || req.GetBody != nil
}

// heal re-logins and swaps in the permanent settings token. Concurrent
// healers serialize on healMu; a healer whose failed token was already
// replaced by another goroutine returns immediately.
func (c *Client) heal(ctx context.Context, failedToken string) error {
	c.healMu.Lock()
	defer c.healMu.Unlock()
	if c.Token() != failedToken {
		// Another goroutine already refreshed the token.
		return nil
	}
	c.credsMu.RLock()
	creds := c.creds
	c.credsMu.RUnlock()
	if creds == nil {
		return fmt.Errorf("no credentials configured")
	}
	if time.Since(c.healFailed) < healCooldown {
		return fmt.Errorf("re-login throttled (last attempt failed recently)")
	}
	jwt, err := c.login(ctx, creds)
	if err != nil {
		c.healFailed = time.Now()
		return err
	}
	perm, err := c.settingsToken(ctx, jwt)
	if err != nil {
		c.healFailed = time.Now()
		return err
	}
	c.SetToken(perm)
	return nil
}

// login performs POST /api/auth/login and returns the (48h) JWT.
func (c *Client) login(ctx context.Context, creds *credentials) (string, error) {
	payload, err := json.Marshal(map[string]string{"username": creds.username, "password": creds.password})
	if err != nil {
		return "", err
	}
	u := *c.base
	u.Path = "/api/auth/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	if doc.Code != http.StatusOK || doc.Data.Token == "" {
		return "", fmt.Errorf("openlist login failed: code=%d message=%s", doc.Code, doc.Message)
	}
	return doc.Data.Token, nil
}

// settingsToken reads the permanent admin token from the OpenList settings
// ("其他设置 → 令牌") using the given (login) token for authorization.
func (c *Client) settingsToken(ctx context.Context, auth string) (string, error) {
	u := *c.base
	u.Path = "/api/admin/setting/get"
	u.RawQuery = "key=token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var doc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("decode setting/get response: %w", err)
	}
	if doc.Code != http.StatusOK || doc.Data.Value == "" {
		return "", fmt.Errorf("openlist setting/get token: code=%d message=%s", doc.Code, doc.Message)
	}
	return doc.Data.Value, nil
}
