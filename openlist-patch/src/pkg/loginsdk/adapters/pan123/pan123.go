// Package pan123 is the loginsdk adapter for OpenList's 123Pan driver: a
// plain account+password login with no developer application. Submit makes
// no network call on purpose: the driver logs in itself when the storage is
// saved, and a second login attempt here could trip 123pan's risk control.
package pan123

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var meta = loginsdk.Meta{
	ID:       "123pan",
	Name:     "123云盘",
	Driver:   "123Pan",
	Method:   loginsdk.MethodForm,
	AppModes: []loginsdk.AppMode{loginsdk.AppNone},
	FormFields: []loginsdk.Field{
		{Key: "username", Label: "手机号或邮箱", Type: "text", Required: true},
		{Key: "password", Label: "密码", Type: "password", Required: true, Secret: true},
	},
	RootField:   "root_folder_id",
	RootDefault: "0",
	RootLabel:   "根目录 ID",
	RootHelp:    "123 云盘文件夹 ID；保持 0 使用整个网盘",
	ReauthFields: []string{
		"username", "password", "AccessToken",
	},
	Notes: []string{
		"使用普通账号密码登录（123Pan 驱动），不需要开发者应用；与「123 Open」开放平台驱动是不同的适配，两者的凭据不能混用",
		"账号密码在保存时由驱动登录验证；上游说明该驱动不再主动维护，登录可能触发风控",
		"首版只验证账号密码路径",
	},
	DocsURL:  "https://doc.oplist.org/guide/drivers/123",
	Verified: "2026-10-14 账号密码",
}

// New returns the 123Pan login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.FormAdapter for 123Pan.
type Adapter struct{}

func (Adapter) Meta() loginsdk.Meta { return meta }

func (Adapter) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppNone {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "123云盘不需要应用配置，请使用账号密码登录")
	}
	return nil
}

func (Adapter) Submit(_ context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	username := strings.TrimSpace(in.Values["username"])
	password := in.Values["password"]
	if username == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写手机号或邮箱")
	}
	if password == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写密码")
	}
	if utf8.RuneCountInString(username) > 256 {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "手机号或邮箱过长")
	}
	if utf8.RuneCountInString(password) > 256 {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "密码过长")
	}
	return &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "account_password",
		AppMode:        loginsdk.AppNone,
		Renewal:        "123Pan 驱动在令牌失效时使用账号密码重新登录",
		Account:        maskUsername(username),
		// The driver's Addition has AccessToken with no json tag, so the JSON
		// key is literally "AccessToken"; empty forces a fresh login on save.
		Fields: map[string]any{
			"username":    username,
			"password":    password,
			"AccessToken": "",
		},
		Secrets: []string{password},
	}, nil
}

// maskUsername keeps the first 3 and last 2 characters of a phone number; an
// e-mail keeps the first 3 characters of the local part and the whole domain.
func maskUsername(s string) string {
	if at := strings.LastIndex(s, "@"); at > 0 {
		local, domain := s[:at], s[at:]
		r := []rune(local)
		if len(r) <= 3 {
			return strings.Repeat("*", len(r)) + domain
		}
		return string(r[:3]) + strings.Repeat("*", len(r)-3) + domain
	}
	r := []rune(s)
	if len(r) <= 5 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-5) + string(r[len(r)-2:])
}
