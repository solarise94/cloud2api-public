// Package example is the contributor reference adapter and the end-to-end
// test harness of the login SDK: it connects another OpenList instance with
// account+password, so it runs against no external service. It is registered
// only under the loginsdk_example build tag (see adapters/all). Copy this
// package to add a platform.
package example

import (
	"context"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var meta = loginsdk.Meta{
	// ID: stable adapter id; the connect URLs and app profiles key off it.
	ID:   "example-openlist",
	Name: "示例：另一个 OpenList（账号密码）",
	// Driver must equal the driver name the upstream storage table uses, so
	// re-authorization finds this adapter for existing storages.
	Driver: "OpenList",
	Method: loginsdk.MethodForm,
	// AppModes: []AppMode{AppNone} because a plain account login needs no
	// developer application (Validate forbids mixing AppNone with others).
	AppModes: []loginsdk.AppMode{loginsdk.AppNone},
	FormFields: []loginsdk.Field{
		// CreateOnly marks a connection setting of the new storage (here the
		// server address): the page hides it during re-authorization and the
		// adapter must not require it then.
		{Key: "url", Label: "OpenList 地址", Type: "url", Required: true, CreateOnly: true, Help: "例如 http://127.0.0.1:5244"},
		{Key: "username", Label: "用户名", Type: "text", Required: true},
		{Key: "password", Label: "密码", Type: "password", Required: true, Secret: true},
	},
	// RootField/RootDefault/RootLabel tell the storage step which root
	// setting to collect; the chosen value goes to the addition's root key.
	RootField:    "root_folder_path",
	RootDefault:  "/",
	RootLabel:    "根目录路径",
	ReauthFields: []string{"username", "password", "token"},
	Notes:        []string{"开发示例，只在 loginsdk_example 构建标签下注册；用于贡献者参考和端到端测试"},
}

// New returns the example login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.FormAdapter for the OpenList driver.
type Adapter struct{}

func (Adapter) Meta() loginsdk.Meta { return meta }

func (Adapter) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppNone {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台不需要配置应用")
	}
	return nil
}

// Submit validates the typed fields and stages the credential. It performs no
// network call: the OpenList driver logs in itself when the storage is saved
// (and re-logs-in whenever its token expires), so calling /auth/login here
// would duplicate that and race it.
func (Adapter) Submit(_ context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	username := strings.TrimSpace(in.Values["username"])
	password := in.Values["password"]
	if username == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写用户名")
	}
	if password == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写密码")
	}
	cfg := &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "account_password",
		AppMode:        loginsdk.AppNone,
		Renewal:        "OpenList 驱动在令牌失效时用账号密码重新登录",
		// Account is a non-secret label for the confirm page; adapters that
		// must query the platform for it leave it empty when that fails.
		Account: username,
		// Fields are written on create AND re-authorization; every key must
		// be declared in Meta.ReauthFields. An empty token forces the driver
		// to log in with the new account.
		Fields: map[string]any{
			"username": username,
			"password": password,
			"token":    "",
		},
		Secrets: []string{password},
	}
	if !in.Reauth {
		raw := strings.TrimSpace(in.Values["url"])
		if raw == "" {
			return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写 OpenList 地址")
		}
		if !validAddress(raw) {
			return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "OpenList 地址必须是完整的 http(s) 地址（不带查询串）")
		}
		// CreateFields are written only when a new storage is created; they
		// must not collide with the root field or any reauth field.
		cfg.CreateFields = map[string]any{"url": strings.TrimRight(raw, "/")}
	}
	return cfg, nil
}

// validAddress reports whether raw is an absolute http(s) URL with a host and
// no userinfo, query or fragment.
func validAddress(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
