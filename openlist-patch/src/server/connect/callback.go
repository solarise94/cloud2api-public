package connect

import (
	"fmt"
	"html"
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/gin-gonic/gin"
)

// The callback result page carries no script and reflects nothing from the
// request, so it is locked down to inline styles only.
const callbackCSP = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

const callbackPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>%s · OpenList</title>
<style>
body{margin:0;font:15px/1.7 system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;background:#f5f6f8;color:#1f2328}
main{max-width:34em;margin:10vh auto 0;padding:2.2em 2em;background:#fff;border-radius:12px;text-align:center}
.platform{color:#6a737d;margin:0}
h1{font-size:1.45em;margin:.35em 0}
.msg{margin:1.2em 0;color:#333;overflow-wrap:anywhere}
.hint{color:#6a737d;font-size:.9em}
a{color:#0969da}
</style>
</head>
<body>
<main>
<p class="platform">%s</p>
<h1>%s</h1>
<p class="msg">%s</p>
<p class="hint">原来的「登录配置」页面会自动继续，本页面可以直接关闭。</p>
<p><a href="%s" rel="noopener">返回登录配置页面</a></p>
</main>
</body>
</html>`

// handleCallback is the public OAuth redirect target. A state problem is a
// 400 page; a provider failure is a normal outcome page (the SDK already
// scrubbed the message).
func (s *server) handleCallback(c *gin.Context) {
	outcome, err := s.mgr.Callback(c.Request.Context(), c.Request.URL.Query())
	if err != nil {
		s.renderCallbackPage(c, http.StatusBadRequest, "", "授权失败", errMessage(err), basePath()+"/@connect/")
		return
	}
	heading := "授权失败"
	switch outcome.Phase {
	case loginsdk.PhaseAuthorized:
		heading = "授权完成"
	case loginsdk.PhaseCancelled:
		heading = "授权已取消"
	}
	s.renderCallbackPage(c, http.StatusOK, outcome.PlatformName, heading, outcome.Message,
		basePath()+"/@connect/#/s/"+outcome.SessionID)
}

// renderCallbackPage escapes every dynamic value; only the four headings and
// the fixed strings are ever written verbatim.
func (s *server) renderCallbackPage(c *gin.Context, status int, platform, heading, message, link string) {
	body := fmt.Sprintf(callbackPage,
		html.EscapeString(heading),
		html.EscapeString(platform),
		html.EscapeString(heading),
		html.EscapeString(message),
		html.EscapeString(link))
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", callbackCSP)
	c.Data(status, "text/html; charset=utf-8", []byte(body))
}
