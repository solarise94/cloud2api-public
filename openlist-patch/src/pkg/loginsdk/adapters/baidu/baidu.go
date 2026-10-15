// Package baidu is the loginsdk adapter for OpenList's BaiduNetdisk driver.
// It supports three application modes: the default public app (scan a QR
// code; the app is the one api.oplist.org publishes for 「百度网盘 - 手动
// 登录」, and the driver renews directly with Baidu using it), the OpenList
// online authorization service (paste the refresh token copied from its page;
// the driver renews through the service), and the administrator's own
// BaiduNetdisk open-platform app (OAuth authorization code). Only codes and
// device tokens are redeemed here; the adapter never calls the refresh
// endpoint.
package baidu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// Provider endpoints, package vars so tests can point them at a fake provider.
var (
	authorizeURL  = "https://openapi.baidu.com/oauth/2.0/authorize"
	tokenURL      = "https://openapi.baidu.com/oauth/2.0/token"
	deviceCodeURL = "https://openapi.baidu.com/oauth/2.0/device/code"
	uinfoURL      = "https://pan.baidu.com/rest/2.0/xpan/nas"
)

// The public app of the OpenList token page's 「百度网盘 - 手动登录」 flow,
// published in OpenList-APIPages. Its secret never reaches the browser.
var publicApp = loginsdk.App{
	Mode:         loginsdk.AppPublic,
	ClientID:     "NqOMXF6XGhGRIGemsQ9nG0Na",
	ClientSecret: "SVT6xpMdLcx6v4aCR4wT8BBOTbzFO8LM",
	RedirectURI:  "oob",
}

var meta = loginsdk.Meta{
	ID:       "baidu",
	Name:     "百度网盘",
	Driver:   "BaiduNetdisk",
	Method:   loginsdk.MethodQR,
	AppModes: []loginsdk.AppMode{loginsdk.AppPublic, loginsdk.AppOnlineService, loginsdk.AppOwn},
	ModeMethods: map[loginsdk.AppMode]loginsdk.Method{
		loginsdk.AppOnlineService: loginsdk.MethodForm,
		loginsdk.AppOwn:           loginsdk.MethodOAuth,
	},
	Scopes:         []string{"basic", "netdisk"},
	PKCE:           false,
	AllowOOB:       true,
	PublicAppLabel: "api.oplist.org「百度网盘 - 手动登录」使用的公开应用",
	FormLabel:      "粘贴令牌",
	FormFields: []loginsdk.Field{
		{Key: "refresh_token", Label: "刷新令牌（Refresh Token）", Type: "password", Required: true, Secret: true,
			Help: "粘贴 OpenList 授权页显示的刷新令牌"},
	},
	RootField:   "root_folder_path",
	RootDefault: "/",
	RootLabel:   "根目录路径",
	RootHelp:    "网盘中的路径，例如 /我的资源；保持 / 使用整个网盘",
	ReauthFields: []string{
		"refresh_token", "AccessToken", "use_online_api", "client_id", "client_secret",
		"api_url_address",
	},
	Notes: []string{
		"默认扫码登录，使用公开应用（api.oplist.org「百度网盘 - 手动登录」发布的应用），驱动用该公开应用直接向百度续期，无需配置",
		"「粘贴令牌」模式通过 OpenList 在线授权服务获取刷新令牌，驱动经该服务续期（use_online_api=true）",
		"自有应用（AppKey/SecretKey）的回调地址需要在应用中登记；无法登记时可把回调地址设为 oob，授权后粘贴页面显示的授权码",
	},
	DocsURL:  "https://doc.oplist.org/guide/drivers/baidu",
	Verified: "2026-10-14 用户实测",
}

// New returns the BaiduNetdisk login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements the BaiduNetdisk login: QRAdapter for the public app,
// FormAdapter for the online service, OAuthAdapter for an own app.
type Adapter struct{}

func (Adapter) Meta() loginsdk.Meta { return meta }

// PublicApp returns the published app the scan login runs under.
func (Adapter) PublicApp() loginsdk.App { return publicApp }

func (Adapter) CheckApp(app loginsdk.App) error {
	switch app.Mode {
	case loginsdk.AppPublic:
		// The credentials always come from the adapter; there is nothing to
		// configure.
		return nil
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
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写 AppKey（client_id）")
		}
		if app.ClientSecret == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写 SecretKey（client_secret）")
		}
		if app.RedirectURI == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写回调地址")
		}
		if app.RedirectURI != "oob" && !validHTTPURL(app.RedirectURI, false) {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "回调地址必须是完整的 http(s) 地址或 oob")
		}
		return nil
	}
	return loginsdk.Errorf(loginsdk.CodeAppInvalid, "不支持的应用模式")
}

// serviceURL is the canonical online-service base URL (trailing "/" trimmed).
func serviceURL(app loginsdk.App) string {
	return strings.TrimRight(app.ServiceURL, "/")
}

// StartQR runs the device-code flow of the public app (RFC 8628).
func (Adapter) StartQR(ctx context.Context, app loginsdk.App) (*loginsdk.QRCode, error) {
	if app.Mode != loginsdk.AppPublic {
		return nil, loginsdk.Errorf(loginsdk.CodeAppInvalid, "仅公开应用模式使用扫码登录")
	}
	q := url.Values{}
	q.Set("response_type", "device_code")
	q.Set("client_id", app.ClientID)
	q.Set("scope", "basic,netdisk")
	var resp struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		VerificationURL  string `json:"verification_url"`
		QrCodeURL        string `json:"qrcode_url"`
		ExpiresIn        any    `json:"expires_in"`
		Interval         any    `json:"interval"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: deviceCodeURL, Query: q}, &resp); err != nil {
		return nil, err
	}
	// Baidu signals rejections in the body regardless of the HTTP status.
	if resp.Error != "" {
		desc := resp.ErrorDescription
		if desc == "" {
			desc = resp.Error
		}
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度扫码登录失败：%s",
			loginsdk.Scrub(desc, app.ClientSecret))
	}
	if resp.DeviceCode == "" || resp.UserCode == "" || resp.QrCodeURL == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度未返回设备码或二维码地址")
	}
	if !validHTTPSURL(resp.QrCodeURL) {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度返回的二维码地址无效")
	}
	ttl, ok := secondsOf(resp.ExpiresIn)
	if !ok || ttl <= 0 {
		ttl = 300 * time.Second
	}
	if ttl > 15*time.Minute {
		ttl = 15 * time.Minute
	}
	pollEvery := 5 * time.Second
	if interval, ok := secondsOf(resp.Interval); ok && interval > pollEvery {
		pollEvery = interval
	}
	return &loginsdk.QRCode{
		ImageURL:  resp.QrCodeURL,
		Message:   "也可以在电脑浏览器打开 https://openapi.baidu.com/device 输入代码 " + resp.UserCode,
		Private:   map[string]string{"device_code": resp.DeviceCode},
		ExpiresAt: time.Now().Add(ttl),
		PollEvery: pollEvery,
	}, nil
}

// PollQR redeems the device code. The error field decides, whatever the HTTP
// status is; this is a device-token exchange, never a refresh.
func (Adapter) PollQR(ctx context.Context, app loginsdk.App, qr *loginsdk.QRCode) (*loginsdk.QRPoll, error) {
	code := qr.Private["device_code"]
	if code == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInternal, "扫码会话缺少设备码")
	}
	q := url.Values{}
	q.Set("grant_type", "device_token")
	q.Set("code", code)
	q.Set("client_id", app.ClientID)
	q.Set("client_secret", app.ClientSecret)
	var tok loginsdk.TokenResponse
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: tokenURL, Query: q}, &tok); err != nil {
		return nil, err
	}
	switch tok.Error {
	case "":
	case "authorization_pending", "slow_down":
		return &loginsdk.QRPoll{Status: loginsdk.QRWaiting}, nil
	case "expired_token", "invalid_grant":
		return &loginsdk.QRPoll{Status: loginsdk.QRExpired}, nil
	case "access_denied":
		return &loginsdk.QRPoll{Status: loginsdk.QRCancelled}, nil
	default:
		desc := tok.ErrorDescription
		if desc == "" {
			desc = tok.Error
		}
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度扫码登录失败：%s",
			loginsdk.Scrub(desc, app.ClientSecret, tok.AccessToken, tok.RefreshToken))
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度授权失败：百度未返回 %s",
			missingToken(tok.AccessToken, tok.RefreshToken))
	}
	return &loginsdk.QRPoll{
		Status: loginsdk.QRConfirmed,
		Auth: &loginsdk.AuthConfig{
			Driver:         meta.Driver,
			CredentialType: "oauth_refresh_token",
			AppMode:        app.Mode,
			AppIdentity:    app.ClientID,
			Renewal:        "BaiduNetdisk 驱动用该公开应用直接向百度续期（use_online_api=false）",
			Account:        accountLabel(ctx, tok.AccessToken),
			// "AccessToken" has no json tag in the driver's Addition, so the
			// JSON key is literally "AccessToken"; a fresh access token
			// saves the driver an immediate refresh on Init.
			Fields: map[string]any{
				"refresh_token":  tok.RefreshToken,
				"AccessToken":    tok.AccessToken,
				"use_online_api": false,
				"client_id":      app.ClientID,
				"client_secret":  app.ClientSecret,
			},
			Secrets: []string{tok.RefreshToken, tok.AccessToken, app.ClientSecret},
		},
	}, nil
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
		Renewal:        "BaiduNetdisk 驱动通过 OpenList 在线授权服务 " + svc + " 续期（use_online_api=true）",
		Fields: map[string]any{
			"refresh_token":   rt,
			"AccessToken":     "",
			"use_online_api":  true,
			"api_url_address": svc + "/baiduyun/renewapi",
			"client_id":       "",
			"client_secret":   "",
		},
		Secrets: []string{rt},
	}, nil
}

// FormGuide points at the OpenList token page; its callback hash preselects
// the Baidu option and ticks 「使用 OpenList 提供的参数」.
func (Adapter) FormGuide(app loginsdk.App, _ bool) *loginsdk.Guide {
	if app.Mode != loginsdk.AppOnlineService {
		return nil
	}
	hash := base64Std(map[string]string{"driver_txt": "baiduyun_go", "server_use": "true"})
	return &loginsdk.Guide{
		URL:   serviceURL(app) + "/#" + hash,
		Label: "打开 OpenList 授权页",
		Steps: []string{
			"在打开的页面确认已选中「百度网盘 - 验证登录 (OAuth App)」并勾选「使用 OpenList 提供的参数」",
			"点「获取 Token」，登录百度账号并同意授权",
			"回到授权页，复制「刷新令牌（Refresh Token）」，粘贴到下面",
		},
	}
}

func (Adapter) AuthorizeURL(app loginsdk.App, state string, _ loginsdk.PKCE) (string, error) {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", app.ClientID)
	q.Set("redirect_uri", app.RedirectURI)
	q.Set("scope", "basic,netdisk")
	q.Set("state", state)
	q.Set("display", "page")
	return loginsdk.BuildURL(authorizeURL, q), nil
}

func (Adapter) Exchange(ctx context.Context, app loginsdk.App, code string, _ loginsdk.PKCE) (*loginsdk.AuthConfig, error) {
	if code == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "缺少授权码")
	}
	q := url.Values{}
	q.Set("grant_type", "authorization_code")
	q.Set("code", code)
	q.Set("client_id", app.ClientID)
	q.Set("client_secret", app.ClientSecret)
	q.Set("redirect_uri", app.RedirectURI)
	var tok loginsdk.TokenResponse
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: tokenURL, Query: q}, &tok); err != nil {
		return nil, err
	}
	// Baidu signals rejections in the body regardless of the HTTP status.
	secrets := []string{app.ClientSecret, code, tok.AccessToken, tok.RefreshToken}
	if tok.Error != "" {
		desc := tok.ErrorDescription
		if desc == "" {
			desc = tok.Error
		}
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度授权失败：%s",
			loginsdk.Scrub(desc, secrets...))
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "百度授权失败：百度未返回 %s",
			missingToken(tok.AccessToken, tok.RefreshToken))
	}
	return &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "oauth_refresh_token",
		AppMode:        app.Mode,
		AppIdentity:    app.ClientID,
		Renewal:        "BaiduNetdisk 驱动使用该应用的 client_id/client_secret 直接向百度续期（use_online_api=false）",
		Account:        accountLabel(ctx, tok.AccessToken),
		Fields: map[string]any{
			"refresh_token":  tok.RefreshToken,
			"AccessToken":    tok.AccessToken,
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
	q := url.Values{}
	q.Set("method", "uinfo")
	q.Set("access_token", accessToken)
	var resp struct {
		NetdiskName string `json:"netdisk_name"`
		BaiduName   string `json:"baidu_name"`
	}
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{URL: uinfoURL, Query: q}, &resp); err != nil {
		return ""
	}
	if resp.NetdiskName != "" {
		return resp.NetdiskName
	}
	return resp.BaiduName
}

func missingToken(access, refresh string) string {
	switch {
	case access == "" && refresh == "":
		return "access_token 和 refresh_token"
	case access == "":
		return "access_token"
	default:
		return "refresh_token"
	}
}

// secondsOf reads a JSON number-ish value (float64, json.Number or string) as
// a duration in whole seconds.
func secondsOf(v any) (time.Duration, bool) {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case json.Number:
		p, err := t.Float64()
		if err != nil {
			return 0, false
		}
		f = p
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		f = p
	default:
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// base64Std encodes v as the standard padded base64 the token page reads back
// with atob.
func base64Std(v map[string]string) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// validHTTPSURL reports whether raw is an absolute https URL with a host.
func validHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
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
