package connect

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// The setup page is exactly these three files; it must not fall through to
// the OpenList SPA (server/router.go registers it as the NoRoute fallback
// after this module).
//
//go:embed web/index.html web/app.js web/app.css
var webFiles embed.FS

const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' https: data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// registerPage mounts /@connect (redirect) and /@connect/ plus its two asset
// files; every other path or method under /@connect is answered here (404/405
// plain text) instead of the SPA fallback.
func registerPage(g *gin.RouterGroup) {
	g.GET("/@connect", pageRedirect)
	g.HEAD("/@connect", pageRedirect)
	g.GET("/@connect/*file", pageFile)
	g.HEAD("/@connect/*file", pageFile)
	for _, m := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodConnect, http.MethodTrace,
	} {
		g.Handle(m, "/@connect", pageMethodNotAllowed)
		g.Handle(m, "/@connect/*file", pageMethodNotAllowed)
	}
}

// pageRedirect sends /@connect to the canonical /@connect/ (keeping the query
// string) so relative references inside the page resolve correctly.
func pageRedirect(c *gin.Context) {
	target := basePath() + "/@connect/"
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	c.Redirect(http.StatusMovedPermanently, target)
}

func pageMethodNotAllowed(c *gin.Context) {
	c.String(http.StatusMethodNotAllowed, "405 method not allowed\n")
}

func pageFile(c *gin.Context) {
	switch c.Param("file") {
	case "/":
		serveWebFile(c, "web/index.html", "text/html; charset=utf-8", "no-store")
	case "/app.js":
		serveWebFile(c, "web/app.js", "text/javascript; charset=utf-8", "no-cache")
	case "/app.css":
		serveWebFile(c, "web/app.css", "text/css; charset=utf-8", "no-cache")
	default:
		c.String(http.StatusNotFound, "404 not found\n")
	}
}

func serveWebFile(c *gin.Context, name, contentType, cacheControl string) {
	data, err := webFiles.ReadFile(name)
	if err != nil {
		c.String(http.StatusInternalServerError, "500 internal error\n")
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy", pageCSP)
	c.Header("Cache-Control", cacheControl)
	c.Data(http.StatusOK, contentType, data)
}
