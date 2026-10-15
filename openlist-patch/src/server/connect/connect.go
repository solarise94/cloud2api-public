// Package connect wires the login SDK and the config adaptation layer into
// OpenList's HTTP server: the admin API, the OAuth callback and the minimal
// setup page.
package connect

import (
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	// Blank import: registers the bundled login adapters into
	// loginsdk.Default at start-up (adapters/all itself imports nothing from
	// this package, so there is no cycle).
	_ "github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/all"
	"github.com/OpenListTeam/OpenList/v4/server/middlewares"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Set at build time (-ldflags -X, see openlist-patch/Containerfile).
var (
	Version        = "dev"
	SourceRevision = ""
)

// server holds the module's process-wide state: the adapter registry, the
// login session manager over it, and the handoff ticket/cookie store. It is
// built once; tests build their own with a private registry.
type server struct {
	reg *loginsdk.Registry
	mgr *loginsdk.Manager
	hf  *handoffStore
}

// defaultServer serves production traffic (registry loginsdk.Default).
var defaultServer = newServer(loginsdk.Default)

// newServer builds a server over reg with the production manager config.
func newServer(reg *loginsdk.Registry) *server {
	return newServerWithConfig(reg, loginsdk.Config{Logf: log.Infof})
}

// newServerWithConfig builds a server with a custom manager config (tests
// inject clocks and tighter limits here).
func newServerWithConfig(reg *loginsdk.Registry, cfg loginsdk.Config) *server {
	if reg == nil {
		reg = loginsdk.Default
	}
	return &server{reg: reg, mgr: loginsdk.NewManager(reg, cfg), hf: newHandoffStore()}
}

// Register mounts the connect routes on OpenList's base router group. It is
// the module's only hook into upstream code (patches/0001). A failed
// migration only disables app profiles; the rest of the module keeps working.
func Register(g *gin.RouterGroup) {
	if err := loginbridge.Migrate(); err != nil {
		log.Errorf("connect: migrate app profiles: %v", err)
	}
	defaultServer.register(g, middlewares.Auth(false), middlewares.AuthAdmin)
}

// register mounts every route; adminAuth is the auth chain of the admin API
// (production: Auth(false) + AuthAdmin; tests: a token-to-user mapping).
// scopedChain prepends the scoped-cookie branch to it: a valid connect cookie
// authenticates by itself and skips the bearer middlewares, everything else
// behaves exactly like the legacy chain.
func (s *server) register(g *gin.RouterGroup, adminAuth ...gin.HandlerFunc) {
	chain := scopedChain(s, adminAuth)
	admin := g.Group("/api/admin/connect", chain...)
	admin.GET("/platforms", s.handlePlatforms)
	admin.POST("/app/save", s.handleAppSave)
	admin.POST("/app/delete", s.handleAppDelete)
	admin.GET("/target", s.handleTarget)
	admin.GET("/mount/suggest", s.handleMountSuggest)
	admin.POST("/session/start", s.handleSessionStart)
	admin.GET("/session/get", s.handleSessionGet)
	admin.POST("/session/submit", s.handleSessionSubmit)
	admin.POST("/session/refresh", s.handleSessionRefresh)
	admin.POST("/session/cancel", s.handleSessionCancel)
	admin.POST("/session/save", s.handleSessionSave)
	// Bearer-only ticket mint for the cloud2api handoff (the scoped cookie is
	// refused inside the handler), plus the two identity helpers the page
	// uses instead of the upstream /api/me and /api/auth/logout. All of them
	// sit inside the connect admin group: that group is the exact cookie
	// scope, and /auth/me carries no bearer-independent meaning elsewhere.
	admin.POST("/handoff/start", s.handleHandoffStart)
	admin.GET("/auth/me", s.handleAuthMe)
	admin.POST("/auth/logout", s.handleAuthLogout)

	// The OAuth redirect target and the handoff exchange are public: the
	// one-time state / ticket links them to the session, so no OpenList login
	// is involved.
	g.GET("/api/connect/oauth/callback", s.handleCallback)
	g.POST("/api/connect/handoff/exchange", s.handleHandoffExchange)

	registerPage(g)
}
