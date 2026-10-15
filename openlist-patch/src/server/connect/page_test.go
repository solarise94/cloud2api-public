package connect

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
)

func pageEngine(t *testing.T) *gin.Engine {
	t.Helper()
	e := gin.New()
	// Stand in for the SPA NoRoute fallback of server/router.go: nothing
	// under /@connect may reach it.
	e.NoRoute(func(c *gin.Context) {
		c.String(200, "SPA")
	})
	// Production mounts on e.Group(conf.URL.Path); mirror the base path.
	g := e.Group(basePath())
	newServerWithConfig(loginsdk.NewRegistry(), loginsdk.Config{}).register(g, fakeAdminAuth)
	return e
}

func TestPageRedirect(t *testing.T) {
	e := pageEngine(t)

	w := callRaw(e, "GET", "/@connect?x=1&y=%E4%B8%AD", "", "")
	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/@connect/?x=1&y=%E4%B8%AD" {
		t.Errorf("location = %q", loc)
	}

	w = callRaw(e, "HEAD", "/@connect", "", "")
	if w.Code != 301 || w.Header().Get("Location") != "/@connect/" {
		t.Errorf("HEAD redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestPageFiles(t *testing.T) {
	e := pageEngine(t)

	index, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	appJS, _ := webFiles.ReadFile("web/app.js")
	appCSS, _ := webFiles.ReadFile("web/app.css")

	cases := []struct {
		target      string
		contentType string
		cache       string
		body        string
	}{
		{"/@connect/", "text/html; charset=utf-8", "no-store", string(index)},
		{"/@connect/app.js", "text/javascript; charset=utf-8", "no-cache", string(appJS)},
		{"/@connect/app.css", "text/css; charset=utf-8", "no-cache", string(appCSS)},
	}
	for _, tc := range cases {
		w := callRaw(e, "GET", tc.target, "", "")
		if w.Code != 200 {
			t.Errorf("%s: status = %d", tc.target, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != tc.contentType {
			t.Errorf("%s: content-type = %q", tc.target, ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != tc.cache {
			t.Errorf("%s: cache-control = %q", tc.target, cc)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", tc.target)
		}
		if w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing referrer policy", tc.target)
		}
		if csp := w.Header().Get("Content-Security-Policy"); csp != pageCSP {
			t.Errorf("%s: csp = %q", tc.target, csp)
		}
		if w.Body.String() != tc.body {
			t.Errorf("%s: body mismatch", tc.target)
		}
	}

	// HEAD answers the same way.
	w := callRaw(e, "HEAD", "/@connect/app.js", "", "")
	if w.Code != 200 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("HEAD app.js: %d", w.Code)
	}

	// Unknown paths under /@connect never fall through to the SPA.
	for _, target := range []string{"/@connect/mock.js", "/@connect/x/y", "/@connect/index.html", "/@connect/app.js.bak"} {
		w := callRaw(e, "GET", target, "", "")
		if w.Code != 404 {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
		if w.Body.String() == "SPA" {
			t.Errorf("%s fell through to the SPA fallback", target)
		}
	}

	// Other methods are answered here too, not by the SPA.
	w = callRaw(e, "POST", "/@connect/app.js", "", "")
	if w.Code != 405 || w.Body.String() == "SPA" {
		t.Errorf("POST app.js: %d %q", w.Code, w.Body.String())
	}
}

func TestPageWithBasePath(t *testing.T) {
	withBase(t, "/sub")
	e := pageEngine(t)

	w := callRaw(e, "GET", "/sub/@connect?keep=1", "", "")
	if w.Code != 301 || w.Header().Get("Location") != "/sub/@connect/?keep=1" {
		t.Errorf("redirect under base: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := callRaw(e, "GET", "/sub/@connect/", "", ""); w.Code != 200 {
		t.Errorf("index under base: %d", w.Code)
	}
	if w := callRaw(e, "GET", "/sub/@connect/nope", "", ""); w.Code != 404 {
		t.Errorf("404 under base: %d", w.Code)
	}
}

// TestRealMiddlewareChain runs the admin API behind upstream's real
// Auth(false) + AuthAdmin chain.
func TestRealMiddlewareChain(t *testing.T) {
	common.SecretKey = []byte("connect-test-jwt-secret")
	// A non-empty api token keeps an empty Authorization header off the admin
	// fast path (setting.GetStr compares against it first).
	if err := db.SaveSettingItem(&model.SettingItem{
		Key: conf.Token, Value: "connect-test-api-token", Type: "string", Flag: 1,
	}); err != nil {
		t.Fatalf("save token setting: %v", err)
	}

	admin := &model.User{Username: "connect-admin", Role: model.ADMIN}
	admin.SetPassword("pw-admin-123")
	if err := op.CreateUser(admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	plain := &model.User{Username: "connect-plain", Role: model.GENERAL}
	plain.SetPassword("pw-plain-123")
	if err := op.CreateUser(plain); err != nil {
		t.Fatalf("create plain user: %v", err)
	}
	guest := &model.User{Username: "guest", Role: model.GUEST, Disabled: true}
	if err := op.CreateUser(guest); err != nil {
		t.Fatalf("create guest: %v", err)
	}

	adminToken, err := common.GenerateToken(admin)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	plainToken, err := common.GenerateToken(plain)
	if err != nil {
		t.Fatalf("generate plain token: %v", err)
	}

	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters(),
		middlewares.Auth(false), middlewares.AuthAdmin)

	_, env := callJSON(t, e, "GET", "/api/admin/connect/platforms", adminToken, nil)
	wantOK(t, env, "admin via real middleware")

	_, env = callJSON(t, e, "GET", "/api/admin/connect/platforms", plainToken, nil)
	if env.Code != 403 {
		t.Errorf("non-admin: envelope code = %d (%s), want 403", env.Code, env.Message)
	}

	_, env = callJSON(t, e, "GET", "/api/admin/connect/platforms", "", nil)
	if env.Code != 401 {
		t.Errorf("no token: envelope code = %d (%s), want 401", env.Code, env.Message)
	}
}
