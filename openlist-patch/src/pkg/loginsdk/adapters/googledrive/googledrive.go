// Package googledrive is the loginsdk adapter for OpenList's GoogleDrive
// driver. The default login pastes the refresh token shown by the OpenList
// online authorization service (the driver then renews through that service);
// the administrator's own Google Cloud OAuth client uses an OAuth
// authorization-code login with PKCE. The adapter only redeems authorization
// codes; it never calls the refresh endpoint.
package googledrive

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// Provider endpoints, package vars so tests can point them at a fake provider.
var (
	authorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL     = "https://oauth2.googleapis.com/token"
	aboutURL     = "https://www.googleapis.com/drive/v3/about"
)

var meta = loginsdk.Meta{
	ID:          "googledrive",
	Name:        "Google Drive",
	Driver:      "GoogleDrive",
	Method:      loginsdk.MethodForm,
	AppModes:    []loginsdk.AppMode{loginsdk.AppOnlineService, loginsdk.AppOwn},
	ModeMethods: map[loginsdk.AppMode]loginsdk.Method{loginsdk.AppOwn: loginsdk.MethodOAuth},
	Scopes:      []string{"https://www.googleapis.com/auth/drive"},
	PKCE:        true,
	AllowOOB:    false,
	FormLabel:   "粘贴令牌",
	FormFields: []loginsdk.Field{
		{Key: "refresh_token", Label: "刷新令牌（Refresh Token）", Type: "password", Required: true, Secret: true,
			Help: "粘贴 OpenList 授权页显示的刷新令牌"},
	},
	RootField:   "root_folder_id",
	RootDefault: "root",
	RootLabel:   "根目录 ID",
	RootHelp:    "Google Drive 文件夹 ID；保持 root 使用「我的云端硬盘」",
	ReauthFields: []string{
		"refresh_token", "use_online_api", "client_id", "client_secret",
		"api_url_address",
	},
	Notes: []string{
		"默认「粘贴令牌」：在 OpenList 授权页获取刷新令牌后粘贴，驱动通过该服务续期（use_online_api=true），无需配置应用",
		"自有应用的 OAuth 同意屏幕处于「测试」状态时 refresh token 7 天后失效，长期使用需发布为正式版本",
		"自有应用 Web 客户端的已获授权重定向 URI 需登记本页显示的回调地址，非 localhost 地址需要 HTTPS",
		"只接入「我的云端硬盘」，共享云端硬盘尚未验证",
	},
	DocsURL: "https://doc.oplist.org/guide/drivers/google_drive",
}

// New returns the GoogleDrive login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.FormAdapter for the online service and
// loginsdk.OAuthAdapter for an own app.
type Adapter struct{}

func (Adapter) Meta() loginsdk.Meta { return meta }

func (Adapter) CheckApp(app loginsdk.App) error {
	switch app.Mode {
	case loginsdk.AppOnlineService:
		if app.ServiceURL == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写在线授权服务地址")
		}
		if !validHTTPURL(serviceURL(app), true) {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "在线授权服务地址必须是完整的 http(s) 地址（不带查询串）")
		}
		return nil
	case loginsdk.AppOwn:
		if app.ClientID == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写客户端 ID（client_id）")
		}
		if app.ClientSecret == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写客户端密钥（client_secret）")
		}
		if app.RedirectURI == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写回调地址")
		}
		if !validHTTPURL(app.RedirectURI, false) {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "回调地址必须是完整的 http(s) 地址（不支持 oob）")
		}
		return nil
	}
	return loginsdk.Errorf(loginsdk.CodeAppInvalid, "不支持的应用模式")
}

// serviceURL is the canonical online-service base URL (trailing "/" trimmed).
func serviceURL(app loginsdk.App) string {
	return strings.TrimRight(app.ServiceURL, "/")
}

// Submit finishes the online-service login with the pasted refresh token.
func (Adapter) Submit(_ context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	if in.App.Mode != loginsdk.AppOnlineService {
		return nil, loginsdk.Errorf(loginsdk.CodeAppInvalid, "粘贴令牌仅用于在线授权服务模式")
	}
	rt, err := loginsdk.ParsePastedToken(in.Values["refresh_token"])
	if err != nil {
		return nil, err
	}
	svc := serviceURL(in.App)
	return &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "oauth_refresh_token",
		AppMode:        in.App.Mode,
		AppIdentity:    svc,
		Renewal:        "GoogleDrive 驱动通过 OpenList 在线授权服务 " + svc + " 续期（use_online_api=true）",
		Fields: map[string]any{
			"refresh_token":   rt,
			"use_online_api":  true,
			"api_url_address": svc + "/googleui/renewapi",
			"client_id":       "",
			"client_secret":   "",
		},
		Secrets: []string{rt},
	}, nil
}

// FormGuide links the token page without a callback hash: the hash would
// suppress the page's Google consent notice.
func (Adapter) FormGuide(app loginsdk.App, _ bool) *loginsdk.Guide {
	if app.Mode != loginsdk.AppOnlineService {
		return nil
	}
	return &loginsdk.Guide{
		URL:   serviceURL(app) + "/",
		Label: "打开 OpenList 授权页",
		Steps: []string{
			"选择「Google Drive OAuth2 (OAuth App)」，阅读弹出的说明后勾选「使用 OpenList 提供的参数」",
			"点「获取 Token」，登录 Google 账号并允许访问 Google Drive",
			"复制「刷新令牌（Refresh Token）」，粘贴到下面",
		},
	}
}

func (Adapter) AuthorizeURL(app loginsdk.App, state string, pkce loginsdk.PKCE) (string, error) {
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("redirect_uri", app.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(meta.Scopes, " "))
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	q.Set("include_granted_scopes", "true")
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	return loginsdk.BuildURL(authorizeURL, q), nil
}

func (Adapter) Exchange(ctx context.Context, app loginsdk.App, code string, pkce loginsdk.PKCE) (*loginsdk.AuthConfig, error) {
	if code == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "缺少授权码")
	}
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	form.Set("redirect_uri", app.RedirectURI)
	form.Set("grant_type", "authorization_code")
	form.Set("code_verifier", pkce.Verifier)
	var tok loginsdk.TokenResponse
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
		Method: http.MethodPost,
		URL:    tokenURL,
		Form:   form,
	}, &tok); err != nil {
		return nil, err
	}
	if tok.Error != "" {
		desc := tok.ErrorDescription
		if desc == "" {
			desc = tok.Error
		}
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Google 授权失败：%s",
			loginsdk.Scrub(desc, app.ClientSecret, code, tok.AccessToken, tok.RefreshToken))
	}
	if tok.RefreshToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Google 未返回 refresh_token，请在 Google 账号中移除该应用的授权后重试")
	}
	// The GoogleDrive driver reads a refresh_token that looks like a path as a
	// service-account file; such a value can never come from this login.
	if strings.HasPrefix(tok.RefreshToken, "/") || strings.HasPrefix(tok.RefreshToken, ".") {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Google 返回的 refresh_token 无效")
	}
	if tok.AccessToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Google 未返回 access_token")
	}
	return &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "oauth_refresh_token",
		AppMode:        app.Mode,
		AppIdentity:    app.ClientID,
		Renewal:        "GoogleDrive 驱动使用自有 OAuth 客户端直接向 Google 续期（use_online_api=false）",
		Account:        accountLabel(ctx, tok.AccessToken),
		Fields: map[string]any{
			"refresh_token":  tok.RefreshToken,
			"use_online_api": false,
			"client_id":      app.ClientID,
			"client_secret":  app.ClientSecret,
		},
		Secrets: []string{tok.RefreshToken, tok.AccessToken, app.ClientSecret, code},
	}, nil
}

// accountLabel is the optional best-effort "who am I" lookup; failures are
// ignored and never fail the login.
func accountLabel(ctx context.Context, accessToken string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var resp struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	q := url.Values{}
	q.Set("fields", "user")
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: aboutURL, Query: q, Header: h}, &resp); err != nil {
		return ""
	}
	return resp.User.EmailAddress
}

// validHTTPURL reports whether raw is an absolute http(s) URL with a host and
// no userinfo or fragment. noQuery additionally rejects a query string.
func validHTTPURL(raw string, noQuery bool) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if u.User != nil || u.Fragment != "" {
		return false
	}
	return !noQuery || u.RawQuery == ""
}
