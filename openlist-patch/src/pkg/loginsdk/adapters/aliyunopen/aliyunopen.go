// Package aliyunopen is the loginsdk adapter for OpenList's AliyundriveOpen
// driver: a scan-to-login either through the OpenList online authorization
// service or under the administrator's own Aliyun open-platform app. The
// adapter only redeems the QR auth code; the driver renews the token (through
// the service or directly with the own app), so the adapter never calls a
// refresh endpoint.
package aliyunopen

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// apiBase is Aliyun's open-platform endpoint, a package var so tests can
// point it at a fake provider.
var apiBase = "https://openapi.alipan.com"

var meta = loginsdk.Meta{
	ID:          "aliyun",
	Name:        "阿里云盘",
	Driver:      "AliyundriveOpen",
	Method:      loginsdk.MethodQR,
	AppModes:    []loginsdk.AppMode{loginsdk.AppOnlineService, loginsdk.AppOwn},
	Scopes:      []string{"user:base", "file:all:read", "file:all:write"},
	RootField:   "root_folder_id",
	RootDefault: "root",
	RootLabel:   "根目录 ID",
	RootHelp:    "阿里云盘文件夹 ID；保持 root 使用整个网盘",
	ReauthFields: []string{
		"refresh_token", "AccessToken", "use_online_api", "api_url_address",
		"alipan_type", "client_id", "client_secret",
	},
	Notes: []string{
		"使用阿里云盘开放平台驱动（AliyundriveOpen），只接受与所选应用匹配的凭据",
		"在线授权服务模式下令牌续期经过该服务（默认 https://api.oplist.org），无需配置应用即可扫码登录",
		"默认挂载资源库（drive_type=resource），需要备份盘时在 OpenList 原生表单中修改",
	},
	Verified: "2026-10-14 在线服务扫码",
	DocsURL:  "https://doc.oplist.org/guide/drivers/aliyundrive_open",
}

// New returns the AliyundriveOpen login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.QRAdapter for AliyundriveOpen.
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
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写 client_id")
		}
		if app.ClientSecret == "" {
			return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写 client_secret")
		}
		return nil
	}
	return loginsdk.Errorf(loginsdk.CodeAppInvalid, "不支持的应用模式")
}

// serviceURL is the canonical online-service base URL (trailing "/" trimmed).
func serviceURL(app loginsdk.App) string {
	return strings.TrimRight(app.ServiceURL, "/")
}

func (Adapter) StartQR(ctx context.Context, app loginsdk.App) (*loginsdk.QRCode, error) {
	var qrURL, sid string
	switch app.Mode {
	case loginsdk.AppOnlineService:
		// The service proxies Aliyun's QR endpoint; driver_txt/server_use tell
		// it which flow (and which of its own apps) to use.
		q := url.Values{}
		q.Set("driver_txt", "alicloud_qr")
		q.Set("server_use", "true")
		var resp struct {
			Text string `json:"text"`
			SID  string `json:"sid"`
		}
		status, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
			URL:   serviceURL(app) + "/alicloud/requests",
			Query: q,
		}, &resp)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, providerErr("OpenList 在线授权服务返回错误", resp.Text, status)
		}
		qrURL, sid = resp.Text, resp.SID
	case loginsdk.AppOwn:
		var resp struct {
			QRCodeURL string `json:"qrCodeUrl"`
			SID       string `json:"sid"`
			Code      string `json:"code"`
			Message   string `json:"message"`
		}
		status, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
			Method: http.MethodPost,
			URL:    apiBase + "/oauth/authorize/qrcode",
			JSON: map[string]any{
				"client_id":     app.ClientID,
				"client_secret": app.ClientSecret,
				"scopes":        meta.Scopes,
				"width":         300,
				"height":        300,
			},
		}, &resp)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK || resp.Code != "" {
			return nil, providerErr("阿里云盘扫码登录失败", orDefault(resp.Message, resp.Code), status, app.ClientSecret)
		}
		qrURL, sid = resp.QRCodeURL, resp.SID
	default:
		return nil, loginsdk.Errorf(loginsdk.CodeAppInvalid, "不支持的应用模式")
	}
	// The QR content is a provider-hosted image, not login text.
	if !validHTTPSURL(qrURL) {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "服务返回的二维码地址无效：%s", qrURL)
	}
	if sid == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "服务未返回二维码会话标识")
	}
	return &loginsdk.QRCode{
		ImageURL:  qrURL,
		Private:   map[string]string{"sid": sid},
		ExpiresAt: time.Now().Add(5 * time.Minute),
		PollEvery: 2 * time.Second,
	}, nil
}

func (a Adapter) PollQR(ctx context.Context, app loginsdk.App, qr *loginsdk.QRCode) (*loginsdk.QRPoll, error) {
	sid := qr.Private["sid"]
	if sid == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInternal, "二维码会话缺少 sid")
	}
	var st struct {
		Status   string `json:"status"`
		AuthCode string `json:"authCode"`
	}
	status, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
		URL: apiBase + "/oauth/qrcode/" + sid + "/status",
	}, &st)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "阿里云盘查询二维码状态失败（HTTP %d）", status)
	}
	switch st.Status {
	case "WaitLogin":
		return &loginsdk.QRPoll{Status: loginsdk.QRWaiting}, nil
	case "ScanSuccess":
		return &loginsdk.QRPoll{Status: loginsdk.QRScanned}, nil
	case "QRCodeExpired":
		return &loginsdk.QRPoll{Status: loginsdk.QRExpired}, nil
	case "LoginSuccess":
		return a.confirm(ctx, app, sid, st.AuthCode)
	default:
		// Unknown provider status: keep waiting and surface the raw value.
		return &loginsdk.QRPoll{Status: loginsdk.QRWaiting, Message: st.Status}, nil
	}
}

// confirm redeems the QR code and finishes the login.
func (a Adapter) confirm(ctx context.Context, app loginsdk.App, sid, authCode string) (*loginsdk.QRPoll, error) {
	var at, rt string
	switch app.Mode {
	case loginsdk.AppOnlineService:
		// The service keys its state off the cookies set by /alicloud/requests;
		// without them it answers 401 {"text":"No Cookies"}.
		q := url.Values{}
		q.Set("code", sid)
		h := http.Header{}
		h.Set("Cookie", "driver_txt=alicloud_qr; server_use=true")
		var resp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Text         string `json:"text"`
		}
		status, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
			URL:    serviceURL(app) + "/alicloud/callback",
			Query:  q,
			Header: h,
		}, &resp)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && strings.HasPrefix(resp.Text, "Login failed:") {
			// The service re-checked Aliyun's status and lost the race with our
			// own poll: still confirming on the phone.
			return &loginsdk.QRPoll{Status: loginsdk.QRScanned}, nil
		}
		if status != http.StatusOK || resp.AccessToken == "" || resp.RefreshToken == "" {
			return nil, providerErr("OpenList 在线授权服务兑换令牌失败", resp.Text, status, resp.AccessToken, resp.RefreshToken)
		}
		at, rt = resp.AccessToken, resp.RefreshToken
	case loginsdk.AppOwn:
		if authCode == "" {
			return nil, loginsdk.Errorf(loginsdk.CodeProvider, "阿里云盘未返回授权码")
		}
		var resp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Code         string `json:"code"`
			Message      string `json:"message"`
		}
		status, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
			Method: http.MethodPost,
			URL:    apiBase + "/oauth/access_token",
			JSON: map[string]any{
				"client_id":     app.ClientID,
				"client_secret": app.ClientSecret,
				"grant_type":    "authorization_code",
				"code":          authCode,
			},
		}, &resp)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK || resp.Code != "" || resp.AccessToken == "" || resp.RefreshToken == "" {
			return nil, providerErr("阿里云盘兑换授权码失败", orDefault(resp.Message, resp.Code), status, app.ClientSecret, resp.AccessToken, resp.RefreshToken)
		}
		at, rt = resp.AccessToken, resp.RefreshToken
	default:
		return nil, loginsdk.Errorf(loginsdk.CodeAppInvalid, "不支持的应用模式")
	}
	auth := &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "oauth_refresh_token",
		AppMode:        app.Mode,
		Account:        accountLabel(ctx, at),
		Secrets:        []string{rt, at},
	}
	if app.Mode == loginsdk.AppOnlineService {
		svc := serviceURL(app)
		auth.AppIdentity = svc
		auth.Renewal = "AliyundriveOpen 驱动通过 OpenList 在线授权服务 " + svc + " 续期（use_online_api=true）"
		auth.Fields = map[string]any{
			"refresh_token":   rt,
			"AccessToken":     at,
			"use_online_api":  true,
			"api_url_address": svc + "/alicloud/renewapi",
			"alipan_type":     "default",
			"client_id":       "",
			"client_secret":   "",
		}
	} else {
		auth.AppIdentity = app.ClientID
		auth.Renewal = "AliyundriveOpen 驱动使用自有开放平台应用直接向阿里云盘续期（use_online_api=false）"
		auth.Fields = map[string]any{
			"refresh_token":  rt,
			"AccessToken":    at,
			"use_online_api": false,
			"client_id":      app.ClientID,
			"client_secret":  app.ClientSecret,
			"alipan_type":    "default",
		}
		auth.Secrets = append(auth.Secrets, app.ClientSecret)
	}
	return &loginsdk.QRPoll{Status: loginsdk.QRConfirmed, Auth: auth}, nil
}

// accountLabel is the optional best-effort "who am I" lookup; failures are
// ignored and never fail the login.
func accountLabel(ctx context.Context, accessToken string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var resp struct {
		NickName string `json:"nick_name"`
		Name     string `json:"name"`
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	if _, _, err := loginsdk.DoJSON(ctx, loginsdk.Request{
		URL:    apiBase + "/oauth/users/info",
		Header: h,
	}, &resp); err != nil {
		return ""
	}
	if resp.NickName != "" {
		return resp.NickName
	}
	return resp.Name
}

// providerErr builds a CodeProvider error whose message carries the provider
// text (scrubbed of every secret that might have been echoed back).
func providerErr(prefix, text string, status int, secrets ...string) *loginsdk.Error {
	if text == "" {
		text = fmt.Sprintf("HTTP %d", status)
	}
	return loginsdk.Errorf(loginsdk.CodeProvider, "%s：%s", prefix, loginsdk.Scrub(text, secrets...))
}

func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// validHTTPSURL reports whether raw is an absolute https URL with a host.
func validHTTPSURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// validHTTPURL reports whether raw is an absolute http(s) URL with a host and
// no userinfo, fragment or (with noQuery) query string.
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
