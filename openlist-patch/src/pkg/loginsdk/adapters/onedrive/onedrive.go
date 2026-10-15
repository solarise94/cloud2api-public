// Package onedrive is the loginsdk adapter for OpenList's Onedrive driver.
// The default login pastes the refresh token shown by the OpenList online
// authorization service (the driver then renews through that service); the
// administrator's own Microsoft Entra app uses an OAuth authorization-code
// login with PKCE, covering the global region (personal and work/school
// accounts). The adapter only redeems authorization codes; it never calls the
// refresh endpoint.
package onedrive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// Provider endpoints, package vars so tests can point them at a fake provider.
var (
	authorizeURL = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	tokenURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	driveURL     = "https://graph.microsoft.com/v1.0/me/drive"
)

const scope = "offline_access Files.ReadWrite.All"

var meta = loginsdk.Meta{
	ID:          "onedrive",
	Name:        "OneDrive",
	Driver:      "Onedrive",
	Method:      loginsdk.MethodForm,
	AppModes:    []loginsdk.AppMode{loginsdk.AppOnlineService, loginsdk.AppOwn},
	ModeMethods: map[loginsdk.AppMode]loginsdk.Method{loginsdk.AppOwn: loginsdk.MethodOAuth},
	Scopes:      []string{"offline_access", "Files.ReadWrite.All"},
	PKCE:        true,
	AllowOOB:    false,
	FormLabel:   "粘贴令牌",
	FormFields: []loginsdk.Field{
		{Key: "refresh_token", Label: "刷新令牌（Refresh Token）", Type: "password", Required: true, Secret: true,
			Help: "粘贴 OpenList 授权页显示的刷新令牌"},
	},
	RootField:   "root_folder_path",
	RootDefault: "/",
	RootLabel:   "根目录路径",
	RootHelp:    "OneDrive 中的路径；保持 / 使用整个 OneDrive",
	ReauthFields: []string{
		"refresh_token", "use_online_api", "client_id", "client_secret", "redirect_uri",
		"api_url_address",
	},
	Notes: []string{
		"默认「粘贴令牌」：在 OpenList 授权页获取刷新令牌后粘贴，驱动通过该服务续期（use_online_api=true），无需配置应用",
		"自有应用模式覆盖 global 区域的个人账号和工作/学校账号；其他区域和 SharePoint 使用 OpenList 原生高级配置",
		"自有应用需要在 Microsoft Entra 注册（支持任意组织目录与个人 Microsoft 账户，平台选 Web），重定向 URI 填写本页显示的回调地址，非 localhost 地址需要 HTTPS",
		"工作/学校账号可能需要组织管理员同意",
	},
	DocsURL:  "https://doc.oplist.org/guide/drivers/onedrive",
	Verified: "2026-10-14 用户实测",
}

// New returns the Onedrive login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.FormAdapter for the online service,
// loginsdk.OAuthAdapter and loginsdk.ReauthChecker for an own app.
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
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写应用 ID（client_id）")
		}
		if app.ClientSecret == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写应用密钥（client_secret）")
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
		Renewal:        "Onedrive 驱动通过 OpenList 在线授权服务 " + svc + " 续期（use_online_api=true）",
		Fields: map[string]any{
			"refresh_token":   rt,
			"use_online_api":  true,
			"api_url_address": svc + "/onedrive/renewapi",
			"client_id":       "",
			"client_secret":   "",
			"redirect_uri":    svc + "/onedrive/callback",
		},
		CreateFields: map[string]any{"region": "global", "is_sharepoint": false},
		Secrets:      []string{rt},
	}, nil
}

// FormGuide points at the OpenList token page; its callback hash preselects
// the OneDrive option (onedrive_pr serves personal and work/school accounts).
func (Adapter) FormGuide(app loginsdk.App, _ bool) *loginsdk.Guide {
	if app.Mode != loginsdk.AppOnlineService {
		return nil
	}
	hash := base64.StdEncoding.EncodeToString([]byte(`{"driver_txt":"onedrive_pr","server_use":"true"}`))
	return &loginsdk.Guide{
		URL:   serviceURL(app) + "/#" + hash,
		Label: "打开 OpenList 授权页",
		Steps: []string{
			"在打开的页面选择「OneDrive - 个人版本 (OAuth App)」（个人账号和工作/学校账号都选这一项），并勾选「使用 OpenList 提供的参数」",
			"点「获取 Token」，登录 Microsoft 账号并同意授权",
			"复制「刷新令牌（Refresh Token）」，粘贴到下面",
		},
	}
}

func (Adapter) AuthorizeURL(app loginsdk.App, state string, pkce loginsdk.PKCE) (string, error) {
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", app.RedirectURI)
	q.Set("response_mode", "query")
	q.Set("scope", scope)
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt", "select_account")
	return loginsdk.BuildURL(authorizeURL, q), nil
}

func (Adapter) Exchange(ctx context.Context, app loginsdk.App, code string, pkce loginsdk.PKCE) (*loginsdk.AuthConfig, error) {
	if code == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "缺少授权码")
	}
	form := url.Values{}
	form.Set("client_id", app.ClientID)
	form.Set("client_secret", app.ClientSecret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", app.RedirectURI)
	form.Set("code_verifier", pkce.Verifier)
	form.Set("scope", scope)
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
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Microsoft 授权失败：%s",
			loginsdk.Scrub(desc, app.ClientSecret, code, tok.AccessToken, tok.RefreshToken))
	}
	if tok.RefreshToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Microsoft 未返回 refresh_token，请确认应用请求了 offline_access 权限")
	}
	if tok.AccessToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "Microsoft 未返回 access_token")
	}
	return &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "oauth_refresh_token",
		AppMode:        app.Mode,
		AppIdentity:    app.ClientID,
		Renewal:        "Onedrive 驱动使用自有应用（client_id/client_secret/redirect_uri）直接向 login.microsoftonline.com 续期（use_online_api=false，region=global）",
		Account:        accountLabel(ctx, tok.AccessToken),
		Fields: map[string]any{
			"refresh_token":  tok.RefreshToken,
			"use_online_api": false,
			"client_id":      app.ClientID,
			"client_secret":  app.ClientSecret,
			"redirect_uri":   app.RedirectURI,
		},
		CreateFields: map[string]any{"region": "global", "is_sharepoint": false},
		Secrets:      []string{tok.RefreshToken, tok.AccessToken, app.ClientSecret, code},
	}, nil
}

// CheckReauth only allows re-authorizing global-region personal/work OneDrive
// storages; addition values may arrive as bool, string or json.Number.
func (Adapter) CheckReauth(addition map[string]any) error {
	if r := anyString(addition["region"]); r != "" && r != "global" {
		return unsupported()
	}
	if anyBool(addition["is_sharepoint"]) {
		return unsupported()
	}
	return nil
}

func unsupported() *loginsdk.Error {
	return loginsdk.Errorf(loginsdk.CodeUnsupported,
		"首版只支持重新授权 global 区域的个人/企业 OneDrive；其他区域和 SharePoint 请在 OpenList 原生表单中更新")
}

// accountLabel is the optional best-effort "who am I" lookup; failures are
// ignored and never fail the login.
func accountLabel(ctx context.Context, accessToken string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var resp struct {
		Owner struct {
			User struct {
				DisplayName string `json:"displayName"`
				Email       string `json:"email"`
			} `json:"user"`
		} `json:"owner"`
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: driveURL, Header: h}, &resp); err != nil {
		return ""
	}
	if resp.Owner.User.DisplayName != "" {
		return resp.Owner.User.DisplayName
	}
	return resp.Owner.User.Email
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

func anyBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	case json.Number:
		return t.String() == "1"
	}
	return false
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
