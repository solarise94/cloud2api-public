package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// The whole package shares one in-memory database (as loginbridge's tests
// do), so every test uses its own mount paths and app profiles.
func init() {
	conf.Conf = conf.DefaultConfig("data")
	conf.URL = &url.URL{Path: ""}
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: conf.Conf.Database.TablePrefix},
	})
	if err != nil {
		panic("failed to connect database: " + err.Error())
	}
	db.Init(dB)
	op.RegisterDriver(func() driver.Driver { return &fakeDriver{} })
	if err := loginbridge.Migrate(); err != nil {
		panic("loginbridge migrate: " + err.Error())
	}
}

// ---- fake driver ----

const fakeDriverName = "ConnectHTTPFake"

// fakeAddition mirrors the shape of a real driver addition.
type fakeAddition struct {
	driver.RootPath
	RefreshToken string `json:"refresh_token" required:"true"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	FormUser     string `json:"form_user"`
	FormPass     string `json:"form_pass"`
}

// fakeDriver fails Init when the refresh token contains "bad", echoing the
// token in the error (to test scrubbing).
type fakeDriver struct {
	model.Storage
	addition fakeAddition
}

func (d *fakeDriver) Config() driver.Config {
	return driver.Config{Name: fakeDriverName, DefaultRoot: "/"}
}

func (d *fakeDriver) GetAddition() driver.Additional { return &d.addition }

func (d *fakeDriver) Init(ctx context.Context) error {
	if strings.Contains(d.addition.RefreshToken, "bad") {
		return fmt.Errorf("fake provider rejected refresh token %q", d.addition.RefreshToken)
	}
	return nil
}

func (d *fakeDriver) Drop(ctx context.Context) error { return nil }
func (d *fakeDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}

func (d *fakeDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, nil
}

// ---- fake adapters ----

const (
	oauthClientID     = "fake-client-id-0123456789"
	oauthClientSecret = "fake-client-secret-abcdefgh"
	oauthRedirectURI  = "https://open.example/api/connect/oauth/callback"
)

var _ loginsdk.OAuthAdapter = (*oauthFake)(nil)

// oauthFake: own-app OAuth with PKCE. "code-fail" makes Exchange fail while
// echoing the client secret; any other code yields token "rt-<code>".
type oauthFake struct{}

func (oauthFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakeoauth", Name: "模拟OAuth盘", Driver: fakeDriverName,
		Method: loginsdk.MethodOAuth, AppModes: []loginsdk.AppMode{loginsdk.AppOwn},
		Scopes: []string{"basic", "netdisk"}, PKCE: true, AllowOOB: true,
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token", "client_id", "client_secret"},
		Notes:        []string{"测试适配器"},
	}
}

func (oauthFake) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppOwn {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台仅支持自有应用")
	}
	if app.ClientID == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "client_id 不能为空")
	}
	if app.ClientSecret == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "client_secret 不能为空")
	}
	if app.RedirectURI == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "redirect_uri 不能为空")
	}
	return nil
}

func (oauthFake) AuthorizeURL(app loginsdk.App, state string, pkce loginsdk.PKCE) (string, error) {
	q := url.Values{}
	q.Set("client_id", app.ClientID)
	q.Set("state", state)
	if pkce.Challenge != "" {
		q.Set("code_challenge", pkce.Challenge)
	}
	return loginsdk.BuildURL("https://auth.example/authorize", q), nil
}

func (oauthFake) Exchange(ctx context.Context, app loginsdk.App, code string, pkce loginsdk.PKCE) (*loginsdk.AuthConfig, error) {
	if code == "code-fail" {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "提供商拒绝了授权码（client_secret=%s）", app.ClientSecret)
	}
	token := "rt-" + code
	return &loginsdk.AuthConfig{
		Driver: fakeDriverName, CredentialType: "oauth_refresh_token",
		AppMode: loginsdk.AppOwn, AppIdentity: app.ClientID,
		Renewal: "驱动使用自有应用直接续期",
		Fields: map[string]any{
			"refresh_token": token,
			"client_id":     app.ClientID,
			"client_secret": app.ClientSecret,
		},
		Secrets: []string{token},
	}, nil
}

var _ loginsdk.QRAdapter = (*qrFake)(nil)

// qrFake: AppNone scan login whose PollQR walks a scripted status list and
// returns waiting forever once it is exhausted.
type qrFake struct {
	mu     sync.Mutex
	script []loginsdk.QRStatus
	calls  int
	issued int
}

func (a *qrFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakeqr", Name: "模拟扫码盘", Driver: fakeDriverName,
		Method: loginsdk.MethodQR, AppModes: []loginsdk.AppMode{loginsdk.AppNone},
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token"},
	}
}

func (a *qrFake) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppNone {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台不需要配置应用")
	}
	return nil
}

func (a *qrFake) StartQR(ctx context.Context, app loginsdk.App) (*loginsdk.QRCode, error) {
	a.mu.Lock()
	a.issued++
	n := a.issued
	a.mu.Unlock()
	return &loginsdk.QRCode{
		Content:   fmt.Sprintf("https://qr.example/ticket-%d", n),
		ExpiresAt: time.Now().Add(2 * time.Minute),
	}, nil
}

func (a *qrFake) PollQR(ctx context.Context, app loginsdk.App, qr *loginsdk.QRCode) (*loginsdk.QRPoll, error) {
	a.mu.Lock()
	status := loginsdk.QRWaiting
	if a.calls < len(a.script) {
		status = a.script[a.calls]
	}
	a.calls++
	a.mu.Unlock()
	if status == loginsdk.QRConfirmed {
		return &loginsdk.QRPoll{Status: status, Auth: &loginsdk.AuthConfig{
			Driver: fakeDriverName, CredentialType: "qr_token", AppMode: loginsdk.AppNone,
			Renewal: "扫码登录",
			Fields:  map[string]any{"refresh_token": "qr-good-token"},
			Secrets: []string{"qr-good-token"},
		}}, nil
	}
	return &loginsdk.QRPoll{Status: status}, nil
}

var _ loginsdk.FormAdapter = (*formFake)(nil)

// formFake: AppNone account login with a create-only server field.
type formFake struct{}

func (formFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakeform", Name: "模拟账号盘", Driver: fakeDriverName,
		Method: loginsdk.MethodForm, AppModes: []loginsdk.AppMode{loginsdk.AppNone},
		FormFields: []loginsdk.Field{
			{Key: "server", Label: "服务地址", Type: "url", Required: true, CreateOnly: true},
			{Key: "user", Label: "用户名", Type: "text", Required: true},
			{Key: "pass", Label: "密码", Type: "password", Required: true, Secret: true},
		},
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token", "form_user", "form_pass"},
	}
}

func (formFake) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppNone {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台不需要配置应用")
	}
	return nil
}

func (formFake) Submit(ctx context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	user := strings.TrimSpace(in.Values["user"])
	pass := in.Values["pass"]
	if user == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写用户名")
	}
	if pass == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写密码")
	}
	cfg := &loginsdk.AuthConfig{
		Driver: fakeDriverName, CredentialType: "account_password", AppMode: loginsdk.AppNone,
		Renewal: "驱动在令牌失效时用账号密码重新登录",
		Fields: map[string]any{
			"refresh_token": "form-" + user,
			"form_user":     user,
			"form_pass":     pass,
		},
		Secrets: []string{pass},
	}
	if !in.Reauth {
		server := strings.TrimSpace(in.Values["server"])
		if server == "" {
			return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写服务地址")
		}
		cfg.CreateFields = map[string]any{"server": server}
	}
	return cfg, nil
}

var _ loginsdk.FormAdapter = ownFormFake{}

// ownFormFake: a form platform that still needs an own app (client_id,
// client_secret and redirect_uri), for the app/save paths.
type ownFormFake struct{}

func (ownFormFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakeownform", Name: "模拟自有应用表单盘", Driver: fakeDriverName,
		Method: loginsdk.MethodForm, AppModes: []loginsdk.AppMode{loginsdk.AppOwn},
		FormFields: []loginsdk.Field{
			{Key: "user", Label: "用户名", Type: "text", Required: true},
		},
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token"},
	}
}

func (ownFormFake) CheckApp(app loginsdk.App) error {
	if app.ClientID == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "client_id 不能为空")
	}
	if app.ClientSecret == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "client_secret 不能为空")
	}
	if app.RedirectURI == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "redirect_uri 不能为空")
	}
	return nil
}

func (ownFormFake) Submit(ctx context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	user := strings.TrimSpace(in.Values["user"])
	if user == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请填写用户名")
	}
	return &loginsdk.AuthConfig{
		Driver: fakeDriverName, CredentialType: "own_form_token", AppMode: loginsdk.AppOwn,
		AppIdentity: "own-client", Renewal: "驱动使用自有应用直接续期",
		Fields:  map[string]any{"refresh_token": "own-" + user},
		Secrets: []string{"own-" + user},
	}, nil
}

// defaultAdapters is the standard registry content, in registration order.
func defaultAdapters(qrScript ...loginsdk.QRStatus) []loginsdk.Adapter {
	script := qrScript
	if script == nil {
		script = []loginsdk.QRStatus{loginsdk.QRWaiting, loginsdk.QRScanned, loginsdk.QRConfirmed}
	}
	return []loginsdk.Adapter{
		oauthFake{},
		&qrFake{script: script},
		formFake{},
		ownFormFake{},
	}
}

// ---- fake auth ----

var (
	adminUser = &model.User{ID: 1, Username: "admin-tok", Role: model.ADMIN}
	otherUser = &model.User{ID: 2, Username: "other-tok", Role: model.ADMIN}
	// tok-A and tok-B are two tokens of the same user; tok-C is another user.
	testTokens = map[string]*model.User{
		"tok-A": adminUser,
		"tok-B": adminUser,
		"tok-C": otherUser,
	}
)

func fakeAdminAuth(c *gin.Context) {
	user, ok := testTokens[c.GetHeader("Authorization")]
	if !ok {
		common.ErrorStrResp(c, "unknown test token", 401)
		c.Abort()
		return
	}
	common.GinAppendValues(c, conf.UserKey, user)
	c.Next()
}

// ---- engine and request helpers ----

func newTestEngine(t *testing.T, cfg loginsdk.Config, adapters []loginsdk.Adapter, auth ...gin.HandlerFunc) (*server, *gin.Engine) {
	t.Helper()
	reg := loginsdk.NewRegistry()
	for _, a := range adapters {
		if err := reg.Register(a); err != nil {
			t.Fatalf("register adapter: %v", err)
		}
	}
	if len(auth) == 0 {
		auth = []gin.HandlerFunc{fakeAdminAuth}
	}
	gin.SetMode(gin.TestMode)
	e := gin.New()
	g := e.Group("")
	s := newServerWithConfig(reg, cfg)
	s.register(g, auth...)
	return s, e
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// errorCode reads data.error_code of a failure envelope.
func (e envelope) errorCode(t *testing.T) string {
	t.Helper()
	var d struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("decode error data %s: %v", e.Data, err)
	}
	return d.ErrorCode
}

func callJSON(t *testing.T, e *gin.Engine, method, path, token string, body any) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not a JSON envelope: %q: %v", w.Body.String(), err)
	}
	return w, env
}

// callRaw sends a request with an opaque body (nil for none).
func callRaw(e *gin.Engine, method, target, token, body string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func wantOK(t *testing.T, env envelope, what string) {
	t.Helper()
	if env.Code != 200 {
		t.Fatalf("%s: expected code 200, got %d (%s) error_code=%s", what, env.Code, env.Message, env.errorCode(t))
	}
}

func wantErr(t *testing.T, env envelope, httpCode int, ec string, what string) {
	t.Helper()
	if env.Code != httpCode {
		t.Errorf("%s: expected envelope code %d, got %d (%s)", what, httpCode, env.Code, env.Message)
	}
	if got := env.errorCode(t); got != ec {
		t.Errorf("%s: expected error_code %q, got %q", what, ec, got)
	}
}

// into decodes an envelope's data into out.
func into(t *testing.T, env envelope, out any) {
	t.Helper()
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("decode data %s: %v", env.Data, err)
	}
}

// viewOf starts a session and returns its view; fail-fast helper for flows.
func startSession(t *testing.T, e *gin.Engine, token, platform string, reauthID int64) loginsdk.View {
	t.Helper()
	body := map[string]any{"platform": platform}
	if reauthID > 0 {
		body["reauth_storage_id"] = reauthID
	}
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/start", token, body)
	wantOK(t, env, "session/start "+platform)
	var v loginsdk.View
	into(t, env, &v)
	return v
}

func sessionGet(t *testing.T, e *gin.Engine, token, id string) loginsdk.View {
	t.Helper()
	_, env := callJSON(t, e, "GET", "/api/admin/connect/session/get?id="+id, token, nil)
	wantOK(t, env, "session/get "+id)
	var v loginsdk.View
	into(t, env, &v)
	return v
}

// deleteAllApps removes every saved app profile (tests share one database).
func deleteAllApps(t *testing.T) {
	t.Helper()
	for _, a := range []string{"fakeoauth", "fakeqr", "fakeform", "fakeownform"} {
		if err := loginbridge.DeleteApp(a); err != nil {
			t.Fatalf("delete app %s: %v", a, err)
		}
	}
}

// storageCount counts storage rows.
func storageCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.GetDb().Model(&model.Storage{}).Count(&n).Error; err != nil {
		t.Fatalf("count storages: %v", err)
	}
	return n
}

// withBase temporarily switches the site base path.
func withBase(t *testing.T, base string) {
	t.Helper()
	old := conf.URL.Path
	conf.URL.Path = base
	t.Cleanup(func() { conf.URL.Path = old })
}

// assertAbsent fails when needle occurs in any of the haystacks.
func assertAbsent(t *testing.T, what, needle string, haystacks ...string) {
	t.Helper()
	for _, h := range haystacks {
		if needle != "" && strings.Contains(h, needle) {
			t.Errorf("%s must not appear in: %s", what, h)
		}
	}
}
