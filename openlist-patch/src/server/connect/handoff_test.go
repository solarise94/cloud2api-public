package connect

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/gin-gonic/gin"
)

// httptest.NewRequest defaults the Host to "example.com"; same-origin
// requests in these tests carry this Origin.
const testOrigin = "http://example.com"

// ticketPattern is the wire format main's client accepts: 32 random bytes,
// base64url without padding (43 characters).
var ticketPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// reqOpt carries the optional headers of a handoff test request.
type reqOpt struct {
	cookie string // raw Cookie header value ("name=value"), "" for none
	origin string // Origin header, "" for none
	site   string // Sec-Fetch-Site header, "" for none
	xfp    string // X-Forwarded-Proto header, "" for none
	xfh    string // X-Forwarded-Host header, "" for none
	tls    bool   // mark the request as received over TLS
	host   string // override req.Host
	ct     string // override Content-Type ("" + body → application/json)
	body   bool   // send a body even when it is empty
}

// buildReq materializes one request (used directly when headers must be
// mutated before sending).
func buildReq(t *testing.T, method, target string, payload any, opt reqOpt) *http.Request {
	t.Helper()
	var rdr io.Reader
	if payload != nil || opt.body {
		if payload == nil {
			payload = struct{}{}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, target, rdr)
	if payload != nil || opt.body {
		ct := opt.ct
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	if opt.cookie != "" {
		req.Header.Set("Cookie", opt.cookie)
	}
	if opt.origin != "" {
		req.Header.Set("Origin", opt.origin)
	}
	if opt.site != "" {
		req.Header.Set("Sec-Fetch-Site", opt.site)
	}
	if opt.xfp != "" {
		req.Header.Set("X-Forwarded-Proto", opt.xfp)
	}
	if opt.xfh != "" {
		req.Header.Set("X-Forwarded-Host", opt.xfh)
	}
	if opt.tls {
		req.TLS = &tls.ConnectionState{}
	}
	if opt.host != "" {
		req.Host = opt.host
	}
	return req
}

// sendReq runs one request against the engine.
func sendReq(e *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// handoffReq sends one request and returns the raw recorder.
func handoffReq(t *testing.T, e *gin.Engine, method, target string, payload any, opt reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	return sendReq(e, buildReq(t, method, target, payload, opt))
}

// envOf decodes an envelope out of a recorder.
func envOf(t *testing.T, w *httptest.ResponseRecorder) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a JSON envelope: %q: %v", w.Body.String(), err)
	}
	return env
}

// ensureHandoffAdminRow mirrors the bearer-mapped admin as a live user row:
// the scoped cookie re-checks the database on every request.
func ensureHandoffAdminRow(t *testing.T) {
	t.Helper()
	pristine := func() {
		u, err := db.GetUserById(adminUser.ID)
		if err != nil {
			return
		}
		u.Username = adminUser.Username
		u.Role = model.ADMIN
		u.Disabled = false
		u.PwdTS = adminUser.PwdTS
		if err := db.UpdateUser(u); err != nil {
			t.Fatalf("reset handoff admin row: %v", err)
		}
	}
	if _, err := db.GetUserById(adminUser.ID); err != nil {
		if err := db.CreateUser(&model.User{
			ID: adminUser.ID, Username: adminUser.Username,
			Role: model.ADMIN, PwdTS: adminUser.PwdTS,
		}); err != nil {
			t.Fatalf("create handoff admin row: %v", err)
		}
	}
	pristine()
	t.Cleanup(pristine)
}

// mintTicket drives POST /api/admin/connect/handoff/start with the bearer
// token and asserts the wire format main's client depends on.
func mintTicket(t *testing.T, e *gin.Engine) string {
	t.Helper()
	w, env := callJSON(t, e, "POST", "/api/admin/connect/handoff/start", "tok-A", nil)
	wantOK(t, env, "handoff/start")
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("mint response Cache-Control = %q, want no-store", cc)
	}
	var data struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	into(t, env, &data)
	if !ticketPattern.MatchString(data.Code) {
		t.Errorf("ticket %q does not match the 32-byte base64url format", data.Code)
	}
	if data.ExpiresIn != 60 {
		t.Errorf("expires_in = %d, want 60", data.ExpiresIn)
	}
	return data.Code
}

// exchangeTicket posts the ticket to the public exchange endpoint.
func exchangeTicket(t *testing.T, e *gin.Engine, code, origin string, opt reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	opt.origin = origin
	return handoffReq(t, e, "POST", "/api/connect/handoff/exchange", map[string]string{"code": code}, opt)
}

// sessionCookie returns the scoped cookie set by an exchange response.
func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, ck := range w.Result().Cookies() {
		if ck.Value != "" {
			return ck.Name + "=" + ck.Value
		}
	}
	t.Fatalf("exchange response set no session cookie: %q", w.Header().Get("Set-Cookie"))
	return ""
}

// runHandshake mints and exchanges once, returning the cookie header value.
func runHandshake(t *testing.T, e *gin.Engine) string {
	t.Helper()
	ensureHandoffAdminRow(t)
	code := mintTicket(t, e)
	w := exchangeTicket(t, e, code, testOrigin, reqOpt{})
	if env := envOf(t, w); env.Code != 200 {
		t.Fatalf("exchange failed: %d (%s)", env.Code, env.Message)
	}
	return sessionCookie(t, w)
}

// ---- start ----

func TestHandoffStartMintsSingleUseTicket(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	// Unauthenticated and wrong-token mints are refused by the bearer chain.
	if _, env := callJSON(t, e, "POST", "/api/admin/connect/handoff/start", "", nil); env.Code != 401 {
		t.Errorf("anonymous start = %d (%s), want 401", env.Code, env.Message)
	}

	mintTicket(t, e)
}

func TestHandoffStartRefusesScopedCookie(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	// A connect cookie must never mint: the wizard cannot extend itself. The
	// request carries a valid Origin and JSON so the 403 provably comes from
	// the mint refusal, not the CSRF gate.
	w := handoffReq(t, e, "POST", "/api/admin/connect/handoff/start", nil, reqOpt{cookie: cookie, origin: testOrigin, body: true})
	env := envOf(t, w)
	if env.Code != 403 {
		t.Errorf("scoped-cookie start = %d (%s), want 403", env.Code, env.Message)
	}

	// The bearer chain keeps working unchanged next to it.
	mintTicket(t, e)
}

// ---- exchange ----

func TestHandoffExchangeMetadataAndCookieAttributes(t *testing.T) {
	ensureHandoffAdminRow(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code := mintTicket(t, e)

	w := exchangeTicket(t, e, code, testOrigin, reqOpt{})
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("exchange Cache-Control = %q, want no-store", cc)
	}
	env := envOf(t, w)
	wantOK(t, env, "exchange")
	if strings.Contains(w.Body.String(), "token") {
		t.Errorf("exchange response must not carry a token: %s", w.Body.String())
	}
	var data struct {
		Username  string `json:"username"`
		ExpiresIn int    `json:"expires_in"`
	}
	into(t, env, &data)
	if data.Username != adminUser.Username {
		t.Errorf("username = %q, want %q", data.Username, adminUser.Username)
	}
	if data.ExpiresIn != 1800 {
		t.Errorf("expires_in = %d, want 1800", data.ExpiresIn)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one cookie, got %d", len(cookies))
	}
	ck := cookies[0]
	if !strings.HasPrefix(ck.Name, connectCookieBase+"_") {
		t.Errorf("cookie name %q misses the site hash suffix", ck.Name)
	}
	if !ck.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if ck.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want Strict", ck.SameSite)
	}
	if ck.Domain != "" {
		t.Errorf("cookie Domain = %q, want host-only (empty)", ck.Domain)
	}
	if ck.Secure {
		t.Error("cookie Secure set on a plain-http request")
	}
	if ck.MaxAge != 1800 {
		t.Errorf("cookie Max-Age = %d, want 1800", ck.MaxAge)
	}
	if ck.Path != "/api/admin/connect" {
		t.Errorf("cookie Path = %q, want /api/admin/connect (base path empty)", ck.Path)
	}
}

func TestHandoffExchangeSingleUseAndUnknown(t *testing.T) {
	ensureHandoffAdminRow(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code := mintTicket(t, e)

	if env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{})); env.Code != 200 {
		t.Fatalf("first exchange = %d (%s)", env.Code, env.Message)
	}
	env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{}))
	wantErr(t, env, 410, loginsdk.CodeExpired, "replayed ticket")
	env = envOf(t, exchangeTicket(t, e, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", testOrigin, reqOpt{}))
	wantErr(t, env, 410, loginsdk.CodeExpired, "unknown ticket")

	// A non-JSON body is rejected before anything is consumed.
	code2 := mintTicket(t, e)
	w := handoffReq(t, e, "POST", "/api/connect/handoff/exchange", nil, reqOpt{origin: testOrigin, ct: "text/plain", body: true, cookie: ""})
	if env := envOf(t, w); env.Code != 400 {
		t.Errorf("non-JSON exchange = %d, want 400", env.Code)
	}
	if env := envOf(t, exchangeTicket(t, e, code2, testOrigin, reqOpt{})); env.Code != 200 {
		t.Errorf("ticket burned by the rejected non-JSON attempt: %d", env.Code)
	}
}

func TestHandoffExchangeTTL(t *testing.T) {
	ensureHandoffAdminRow(t)

	// Just inside the 60 s window the ticket still redeems.
	s, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code := mintTicket(t, e)
	base := time.Now()
	s.hf.now = func() time.Time { return base.Add(59 * time.Second) }
	if env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{})); env.Code != 200 {
		t.Fatalf("exchange at +59s failed: %d (%s)", env.Code, env.Message)
	}

	// A fresh engine, one second past the TTL: expired, envelope expired.
	s2, e2 := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code = mintTicket(t, e2)
	s2.hf.now = func() time.Time { return base.Add(61 * time.Second) }
	env := envOf(t, exchangeTicket(t, e2, code, testOrigin, reqOpt{}))
	wantErr(t, env, 410, loginsdk.CodeExpired, "expired ticket")
}

func TestHandoffExchangeConcurrentRedeem(t *testing.T) {
	ensureHandoffAdminRow(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code := mintTicket(t, e)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := exchangeTicket(t, e, code, testOrigin, reqOpt{})
			if env := envOf(t, w); env.Code == 200 {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Errorf("concurrent redeem winners = %d, want exactly 1", winners)
	}
}

func TestHandoffExchangeOriginRules(t *testing.T) {
	cases := []struct {
		name string
		opt  reqOpt
		want int
	}{
		{"missing origin", reqOpt{}, 403},
		{"null origin", reqOpt{origin: "null"}, 403},
		{"cross host", reqOpt{origin: "http://evil.example"}, 403},
		{"other port", reqOpt{origin: "http://example.com:9999"}, 403},
		{"https origin on plain request", reqOpt{origin: "https://example.com"}, 403},
		{"http origin on tls request", reqOpt{origin: "http://example.com", tls: true}, 403},
		{"https origin on tls request", reqOpt{origin: "https://example.com", tls: true}, 200},
		{"https via forwarded proto", reqOpt{origin: "https://example.com", xfp: "https"}, 200},
		{"forwarded host is not trusted", reqOpt{origin: "http://evil.example", xfh: "example.com"}, 403},
		{"default port origin", reqOpt{origin: "http://example.com", host: "example.com:80"}, 200},
		{"ftp scheme", reqOpt{origin: "ftp://example.com"}, 403},
		{"userinfo in origin", reqOpt{origin: "http://user:pw@example.com"}, 403},
		{"path in origin", reqOpt{origin: "http://example.com/x"}, 403},
		{"bare slash path in origin", reqOpt{origin: "http://example.com/"}, 403},
		{"query in origin", reqOpt{origin: "http://example.com/?a=1"}, 403},
		{"bare query in origin", reqOpt{origin: "http://example.com?"}, 403},
		{"fragment in origin", reqOpt{origin: "https://example.com/#x"}, 403},
		{"bare fragment in origin", reqOpt{origin: "http://example.com#"}, 403},
		{"port mismatch on same host", reqOpt{origin: "http://example.com", host: "example.com:8443"}, 403},
		{"https 443 is not http default", reqOpt{origin: "http://example.com:443"}, 403},
		{"explicit equal ports", reqOpt{origin: "http://example.com:8080", host: "example.com:8080"}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ensureHandoffAdminRow(t)
			_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
			code := mintTicket(t, e)
			env := envOf(t, handoffReq(t, e, "POST", "/api/connect/handoff/exchange",
				map[string]string{"code": code}, tc.opt))
			if env.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", env.Code, env.Message, tc.want)
			}
		})
	}

	// A site with an https site URL accepts an https origin behind a plain
	// proxy hop that does not forward the proto header.
	t.Run("https origin when site url is https", func(t *testing.T) {
		ensureHandoffAdminRow(t)
		old := conf.Conf.SiteURL
		conf.Conf.SiteURL = "https://example.com"
		t.Cleanup(func() { conf.Conf.SiteURL = old })
		_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
		code := mintTicket(t, e)
		if env := envOf(t, exchangeTicket(t, e, code, "https://example.com", reqOpt{})); env.Code != 200 {
			t.Errorf("https origin = %d (%s), want 200", env.Code, env.Message)
		}
	})

	// Two Origin headers are malformed, never an "either" match.
	t.Run("multiple origin headers", func(t *testing.T) {
		ensureHandoffAdminRow(t)
		_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
		code := mintTicket(t, e)
		req := buildReq(t, "POST", "/api/connect/handoff/exchange", map[string]string{"code": code}, reqOpt{})
		req.Header["Origin"] = []string{testOrigin, testOrigin}
		if env := envOf(t, sendReq(e, req)); env.Code != 403 {
			t.Errorf("duplicate Origin = %d, want 403", env.Code)
		}
	})

	// Rejected origins must not burn the ticket.
	t.Run("rejected origin keeps the ticket", func(t *testing.T) {
		ensureHandoffAdminRow(t)
		_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
		code := mintTicket(t, e)
		if env := envOf(t, exchangeTicket(t, e, code, "http://evil.example", reqOpt{})); env.Code != 403 {
			t.Fatalf("cross-origin exchange = %d, want 403", env.Code)
		}
		if env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{})); env.Code != 200 {
			t.Errorf("exchange after rejected origin = %d (%s), want 200", env.Code, env.Message)
		}
	})
}

// ---- scoped cookie authentication ----

func TestScopedCookieAuthenticatesWizard(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	// Connect API with the cookie alone.
	_, env := callJSON(t, e, "GET", "/api/admin/connect/platforms", "", nil)
	if env.Code != 401 {
		t.Fatalf("anonymous platforms = %d, want 401 (bearer chain intact)", env.Code)
	}
	w := handoffReq(t, e, "GET", "/api/admin/connect/platforms", nil, reqOpt{cookie: cookie})
	if env := envOf(t, w); env.Code != 200 {
		t.Fatalf("cookie platforms = %d (%s)", env.Code, env.Message)
	}

	// auth/me reports the scoped identity.
	w = handoffReq(t, e, "GET", "/api/admin/connect/auth/me", nil, reqOpt{cookie: cookie})
	env = envOf(t, w)
	wantOK(t, env, "cookie auth/me")
	var me struct {
		Username string `json:"username"`
		Role     int    `json:"role"`
		Scope    string `json:"scope"`
	}
	into(t, env, &me)
	if me.Username != adminUser.Username || me.Role != model.ADMIN || me.Scope != "connect" {
		t.Errorf("me = %+v, want admin/connect", me)
	}

	// Fetch Metadata on cookie reads is enforced at the middleware, for every
	// route the cookie can reach.
	for site, want := range map[string]int{"cross-site": 403, "same-site": 403, "same-origin": 200} {
		w := handoffReq(t, e, "GET", "/api/admin/connect/platforms", nil, reqOpt{cookie: cookie, site: site})
		if env := envOf(t, w); env.Code != want {
			t.Errorf("Sec-Fetch-Site %s platforms = %d, want %d", site, env.Code, want)
		}
	}

	// Wizard sessions started under the cookie carry the cookie's own owner
	// fingerprint.
	v := startCookieSession(t, e, cookie, "fakeqr")
	if got := cookieSessionGet(t, e, cookie, v.ID); got.Phase != loginsdk.PhasePending {
		t.Fatalf("cookie session/get = %s", got.Phase)
	}
	// The same admin's bearer tokens cannot see the cookie's sessions
	// (fingerprint isolation), and vice versa the cookie cannot see bearer
	// sessions.
	_, env = callJSON(t, e, "GET", "/api/admin/connect/session/get?id="+v.ID, "tok-A", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "bearer of same user")
	_, env = callJSON(t, e, "GET", "/api/admin/connect/session/get?id="+v.ID, "tok-B", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "other bearer of same user")

	bearerView := startSession(t, e, "tok-A", "fakeqr", 0)
	w = handoffReq(t, e, "GET", "/api/admin/connect/session/get?id="+bearerView.ID, nil, reqOpt{cookie: cookie})
	wantErr(t, envOf(t, w), 404, loginsdk.CodeNotFound, "cookie vs bearer session")

	// A valid cookie wins over an Authorization header: even with a stale
	// bearer token attached, the scoped identity answers.
	req := buildReq(t, "GET", "/api/admin/connect/auth/me", nil, reqOpt{cookie: cookie})
	req.Header.Set("Authorization", "tok-B")
	w = sendReq(e, req)
	if env := envOf(t, w); env.Code != 200 {
		t.Errorf("cookie me with stale header = %d (%s)", env.Code, env.Message)
	}
	var me2 struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(envOf(t, w).Data, &me2); err != nil || me2.Scope != "connect" {
		t.Errorf("stale-header me scope = %q (%v), want connect", me2.Scope, err)
	}
}

// startCookieSession starts a wizard session authenticated only by the cookie.
// Cookie mutations carry the same-origin Origin like the real page does.
func startCookieSession(t *testing.T, e *gin.Engine, cookie, platform string) loginsdk.View {
	t.Helper()
	w := handoffReq(t, e, "POST", "/api/admin/connect/session/start",
		map[string]any{"platform": platform}, reqOpt{cookie: cookie, origin: testOrigin})
	env := envOf(t, w)
	wantOK(t, env, "cookie session/start "+platform)
	var v loginsdk.View
	into(t, env, &v)
	return v
}

// cookieSessionGet reads a wizard session with the cookie.
func cookieSessionGet(t *testing.T, e *gin.Engine, cookie, id string) loginsdk.View {
	t.Helper()
	w := handoffReq(t, e, "GET", "/api/admin/connect/session/get?id="+id, nil, reqOpt{cookie: cookie})
	env := envOf(t, w)
	wantOK(t, env, "cookie session/get "+id)
	var v loginsdk.View
	into(t, env, &v)
	return v
}

func TestScopedCookieScopeLimited(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	// The cookie chain is mounted only on the module's routes: everything
	// else (upstream /api/me and any other API) stays outside — here the test
	// engine has no such route, so the request cannot even reach an
	// authenticator.
	w := handoffReq(t, e, "GET", "/api/me", nil, reqOpt{cookie: cookie})
	if w.Code != 404 {
		t.Errorf("cookie /api/me = %d, want 404 (no route outside the module)", w.Code)
	}
	w = handoffReq(t, e, "GET", "/api/auth/me", nil, reqOpt{cookie: cookie})
	if w.Code != 404 {
		t.Errorf("cookie /api/auth/me = %d, want 404 (helpers live inside the connect group)", w.Code)
	}
	w = handoffReq(t, e, "GET", "/api/admin/setting/get?key=token", nil, reqOpt{cookie: cookie})
	if w.Code != 404 {
		t.Errorf("cookie upstream admin API = %d, want 404", w.Code)
	}
}

func TestScopedCookieUserStateChecks(t *testing.T) {
	ensureHandoffAdminRow(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	mutate := func(field string, apply func(u *model.User)) {
		t.Helper()
		u, err := db.GetUserById(adminUser.ID)
		if err != nil {
			t.Fatalf("load user: %v", err)
		}
		apply(u)
		if err := db.UpdateUser(u); err != nil {
			t.Fatalf("update user: %v", err)
		}
		w := handoffReq(t, e, "GET", "/api/admin/connect/platforms", nil, reqOpt{cookie: cookie})
		if env := envOf(t, w); env.Code != 401 {
			t.Errorf("%s: cookie platforms = %d (%s), want 401", field, env.Code, env.Message)
		}
	}
	mutate("disabled", func(u *model.User) { u.Disabled = true })
	mutate("demoted", func(u *model.User) { u.Role = model.GUEST })
	mutate("password changed", func(u *model.User) { u.PwdTS = u.PwdTS + 1 })

	// A revoked user row makes the exchange fail too: the ticket is bound to
	// a live admin at redemption time.
	code := mintTicket(t, e)
	u, err := db.GetUserById(adminUser.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if err := db.DeleteUserById(u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{}))
	wantErr(t, env, 410, loginsdk.CodeExpired, "exchange for deleted user")
}

// Multi-tab: exchanging a second ticket in the same browser keeps the
// existing scoped session (owner fingerprint and original absolute expiry),
// so wizard sessions opened in other tabs stay resumable; the ticket is
// consumed either way. A fresh browser (or a stale cookie) gets a new cookie.
func TestHandoffExchangeReuseAndIsolation(t *testing.T) {
	ensureHandoffAdminRow(t)
	s, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	// A wizard session owned by the first cookie.
	first := startCookieSession(t, e, cookie, "fakeqr")

	// Second handoff in the same browser: reuse, no new cookie.
	code := mintTicket(t, e)
	w := exchangeTicket(t, e, code, testOrigin, reqOpt{cookie: cookie})
	env := envOf(t, w)
	wantOK(t, env, "same-browser re-exchange")
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("re-exchange set a new cookie: %v", cookies)
	}
	var data struct {
		Username  string `json:"username"`
		ExpiresIn int    `json:"expires_in"`
	}
	into(t, env, &data)
	if data.Username != adminUser.Username {
		t.Errorf("username = %q", data.Username)
	}
	if data.ExpiresIn <= 0 || data.ExpiresIn > 1800 {
		t.Errorf("remaining expires_in = %d, want (0,1800]", data.ExpiresIn)
	}
	if got := cookieSessionGet(t, e, cookie, first.ID); got.ID != first.ID || got.Phase != loginsdk.PhasePending {
		t.Errorf("first session lost after re-exchange: %+v", got)
	}

	// A fresh browser exchanging for the same admin gets its own cookie and
	// its own fingerprint: the first browser's session stays invisible.
	code = mintTicket(t, e)
	w = exchangeTicket(t, e, code, testOrigin, reqOpt{})
	if env := envOf(t, w); env.Code != 200 {
		t.Fatalf("fresh-browser exchange = %d (%s)", env.Code, env.Message)
	}
	cookie2 := sessionCookie(t, w)
	if cookie2 == cookie {
		t.Fatal("fresh browser received the first browser's cookie value")
	}
	w = handoffReq(t, e, "GET", "/api/admin/connect/session/get?id="+first.ID, nil, reqOpt{cookie: cookie2})
	wantErr(t, envOf(t, w), 404, loginsdk.CodeNotFound, "cross-browser session")
	if got := cookieSessionGet(t, e, cookie, first.ID); got.ID != first.ID {
		t.Errorf("first browser session broken by second browser: %+v", got)
	}

	// A stale cookie is replaced, not reused: past the absolute expiry the
	// exchange mints a fresh session with a fresh Max-Age.
	base := time.Now()
	s.hf.now = func() time.Time { return base.Add(31 * time.Minute) }
	code = mintTicket(t, e)
	w = exchangeTicket(t, e, code, testOrigin, reqOpt{cookie: cookie})
	if env := envOf(t, w); env.Code != 200 {
		t.Fatalf("exchange with stale cookie = %d (%s)", env.Code, env.Message)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != 1800 {
		t.Fatalf("stale cookie was not replaced: %v", cookies)
	}
	cookie = cookies[0].Name + "=" + cookies[0].Value // the browser's new jar entry
	w = handoffReq(t, e, "GET", "/api/admin/connect/platforms", nil, reqOpt{cookie: cookie})
	if env := envOf(t, w); env.Code != 200 {
		t.Errorf("replaced cookie does not authenticate: %d (%s)", env.Code, env.Message)
	}
}

// ---- auth/me and logout ----

func TestAuthMeBearerScope(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	w, env := callJSON(t, e, "GET", "/api/admin/connect/auth/me", "tok-A", nil)
	wantOK(t, env, "bearer auth/me")
	_ = w
	var me struct {
		Scope string `json:"scope"`
		Role  int    `json:"role"`
	}
	into(t, env, &me)
	if me.Scope != "bearer" || me.Role != model.ADMIN {
		t.Errorf("bearer me = %+v", me)
	}
}

func TestAuthLogoutRevokesAndClears(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	// Mutations require the same-origin Origin header (middleware and handler).
	w := handoffReq(t, e, "POST", "/api/admin/connect/auth/logout", nil, reqOpt{cookie: cookie, body: true})
	if env := envOf(t, w); env.Code != 403 {
		t.Errorf("logout without Origin = %d, want 403", env.Code)
	}
	w = handoffReq(t, e, "POST", "/api/admin/connect/auth/logout", nil, reqOpt{cookie: cookie, origin: "http://evil.example", body: true})
	if env := envOf(t, w); env.Code != 403 {
		t.Errorf("cross-origin logout = %d, want 403", env.Code)
	}

	w = handoffReq(t, e, "POST", "/api/admin/connect/auth/logout", nil, reqOpt{cookie: cookie, origin: testOrigin, body: true})
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("logout Cache-Control = %q", cc)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("logout did not expire the cookie: %v", cookies)
	}

	// The revoked value is dead server-side, not just in the browser.
	w = handoffReq(t, e, "GET", "/api/admin/connect/platforms", nil, reqOpt{cookie: cookie})
	if env := envOf(t, w); env.Code != 401 {
		t.Errorf("platforms after logout = %d (%s), want 401", env.Code, env.Message)
	}

	// Bearer logout mirrors the upstream semantics and is also origin-guarded.
	if _, env := callJSON(t, e, "POST", "/api/admin/connect/auth/logout", "tok-A", nil); env.Code != 403 {
		t.Errorf("bearer logout without Origin = %d, want 403", env.Code)
	}
	req := buildReq(t, "POST", "/api/admin/connect/auth/logout", nil, reqOpt{origin: testOrigin, body: true})
	req.Header.Set("Authorization", "tok-A")
	w = sendReq(e, req)
	if env := envOf(t, w); env.Code != 200 {
		t.Errorf("bearer logout = %d (%s), want 200", env.Code, env.Message)
	}
}

// ---- cookie CSRF on every mutating route ----

func TestCookieMutationCSRFOnEveryRoute(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	routes := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/admin/connect/app/save", map[string]any{"platform": "fakeoauth", "mode": "own_app"}},
		{"POST", "/api/admin/connect/app/delete", map[string]any{"platform": "fakeoauth"}},
		{"POST", "/api/admin/connect/session/start", map[string]any{"platform": "fakeqr"}},
		{"POST", "/api/admin/connect/session/submit", map[string]any{"id": "nope", "values": map[string]string{}}},
		{"POST", "/api/admin/connect/session/refresh", map[string]any{"id": "nope"}},
		{"POST", "/api/admin/connect/session/cancel", map[string]any{"id": "nope"}},
		{"POST", "/api/admin/connect/session/save", map[string]any{"id": "nope"}},
		{"POST", "/api/admin/connect/auth/logout", map[string]any{}},
	}
	for _, r := range routes {
		// No Origin at all: refused by the middleware, never by the handler.
		w := handoffReq(t, e, r.method, r.path, r.body, reqOpt{cookie: cookie})
		if env := envOf(t, w); env.Code != 403 {
			t.Errorf("%s without Origin = %d (%s), want 403", r.path, env.Code, env.Message)
		}
		// Origin but a non-JSON body: refused too.
		w = handoffReq(t, e, r.method, r.path, r.body, reqOpt{cookie: cookie, origin: testOrigin, ct: "text/plain"})
		if env := envOf(t, w); env.Code != 403 {
			t.Errorf("%s with non-JSON body = %d, want 403", r.path, env.Code)
		}
		// Origin + JSON passes the CSRF gate; the handler may answer with its
		// own business status (200/4xx), just not with a CSRF rejection.
		w = handoffReq(t, e, r.method, r.path, r.body, reqOpt{cookie: cookie, origin: testOrigin})
		if env := envOf(t, w); env.Code == 403 {
			t.Errorf("%s with same-origin JSON = %d, want non-403", r.path, env.Code)
		}
	}
}

// The scoped cookie cannot mint a ticket even when a bearer header rides
// along next to it.
func TestHandoffStartRefusesCookieEvenWithBearer(t *testing.T) {
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	cookie := runHandshake(t, e)

	req := buildReq(t, "POST", "/api/admin/connect/handoff/start", nil, reqOpt{cookie: cookie, origin: testOrigin, body: true})
	req.Header.Set("Authorization", "tok-A")
	if env := envOf(t, sendReq(e, req)); env.Code != 403 {
		t.Errorf("cookie+bearer start = %d (%s), want 403", env.Code, env.Message)
	}
	// The bearer path mints happily right after.
	mintTicket(t, e)
}

// A ticket is invalidated when the admin's password timestamp changes between
// mint and redeem.
func TestHandoffTicketInvalidatedByPwdChange(t *testing.T) {
	ensureHandoffAdminRow(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	code := mintTicket(t, e)

	u, err := db.GetUserById(adminUser.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	u.PwdTS++
	if err := db.UpdateUser(u); err != nil {
		t.Fatalf("update user: %v", err)
	}
	env := envOf(t, exchangeTicket(t, e, code, testOrigin, reqOpt{}))
	wantErr(t, env, 410, loginsdk.CodeExpired, "pwd changed after mint")
}

// Expiry is exact: at the expiry instant itself the entry is already dead.
func TestHandoffExpiryAtExactInstant(t *testing.T) {
	h := newHandoffStore()
	base := time.Now()
	h.now = func() time.Time { return base }

	code, err := h.startTicket(1, 0)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	h.now = func() time.Time { return base.Add(handoffTicketTTL) }
	if _, _, ok := h.consumeTicket(code); ok {
		t.Error("ticket at the exact expiry instant must be expired")
	}

	h.now = func() time.Time { return base }
	val, _, err := h.createSession(1, "u", 0)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h.now = func() time.Time { return base.Add(handoffSessionTTL) }
	if _, ok := h.getSession(val); ok {
		t.Error("session at the exact expiry instant must be expired")
	}
}

// ---- stores ----

func TestHandoffStoresBoundedAndPruned(t *testing.T) {
	s := newHandoffStore()

	// Per-user ticket cap.
	for i := 0; i < maxTicketsPerUser; i++ {
		if _, err := s.startTicket(1, 0); err != nil {
			t.Fatalf("ticket %d: %v", i+1, err)
		}
	}
	if _, err := s.startTicket(1, 0); err == nil {
		t.Error("per-user ticket cap not enforced")
	}
	// Another user still fits until the global cap; expiry frees slots.
	if _, err := s.startTicket(2, 0); err != nil {
		t.Errorf("second user blocked by per-user accounting: %v", err)
	}
	base := time.Now()
	s.now = func() time.Time { return base.Add(2 * handoffTicketTTL) }
	if _, err := s.startTicket(1, 0); err != nil {
		t.Errorf("expired tickets were not pruned: %v", err)
	}

	// Session cap.
	s2 := newHandoffStore()
	for i := 0; i < maxSessions; i++ {
		if _, _, err := s2.createSession(uint(i+1), "u", 0); err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
	}
	if _, _, err := s2.createSession(1, "u", 0); err == nil {
		t.Error("session cap not enforced")
	}

	// Live session semantics on a small store: value and fingerprint formats,
	// lookup by value, revoke.
	s3 := newHandoffStore()
	val, fp, err := s3.createSession(7, "admin", 5)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if fp == "" || len(val) != 43 {
		t.Fatalf("session value/fingerprint format wrong: %d", len(val))
	}
	if sess, ok := s3.getSession(val); !ok || sess.username != "admin" || sess.pwdTS != 5 {
		t.Errorf("session lookup wrong: %+v", sess)
	}
	s3.revoke(val)
	if _, ok := s3.getSession(val); ok {
		t.Error("revoked session still resolves")
	}

	// Absolute expiry: with the clock past the lifetime, the old session is
	// gone and the bounded map has room again after the pruning pass.
	sessionBase := time.Now()
	s3.now = func() time.Time { return sessionBase.Add(2 * handoffSessionTTL) }
	val2, _, err := s3.createSession(7, "admin", 5)
	if err != nil {
		t.Fatalf("session after pruning: %v", err)
	}
	if _, ok := s3.getSession(val); ok {
		t.Error("expired session still resolves")
	}
	if _, ok := s3.getSession(val2); !ok {
		t.Error("fresh session missing")
	}
}

func TestConnectCookieNameIsSiteScoped(t *testing.T) {
	name := func(host string) string {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "http://"+host+"/", nil)
		return connectCookieNameFor(c)
	}
	a1, a2 := name("127.0.0.1:5244"), name("127.0.0.1:5244")
	b := name("127.0.0.1:5245")
	if a1 != a2 {
		t.Errorf("cookie name unstable for the same site: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("cookie name collides across ports on one host: %q", a1)
	}
}

func TestOwnerFromMissingUserIsAnError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", nil)
	if _, err := ownerFrom(c); err == nil {
		t.Fatal("ownerFrom without a user must return an error, not panic")
	}
}
