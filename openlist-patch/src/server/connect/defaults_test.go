package connect

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

const (
	publicClientID     = "public-client-id-0123456789"
	publicClientSecret = "public-client-secret-abcdefgh"
)

var _ loginsdk.QRAdapter = (*publicFake)(nil)

// publicFake: a platform whose default app is the adapter's public app (scan
// login), like baidu.
type publicFake struct{}

func (publicFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakepublic", Name: "模拟公开应用盘", Driver: fakeDriverName,
		Method: loginsdk.MethodQR, AppModes: []loginsdk.AppMode{loginsdk.AppPublic},
		PublicAppLabel: "公开应用说明", RootField: "root_folder_path", RootDefault: "/",
		RootLabel: "根目录路径", ReauthFields: []string{"refresh_token", "client_id", "client_secret"},
	}
}

func (publicFake) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppPublic {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台仅支持公共应用")
	}
	return nil
}

func (publicFake) PublicApp() loginsdk.App {
	return loginsdk.App{
		Mode:         loginsdk.AppPublic,
		ClientID:     publicClientID,
		ClientSecret: publicClientSecret,
		RedirectURI:  "oob",
	}
}

func (publicFake) StartQR(ctx context.Context, app loginsdk.App) (*loginsdk.QRCode, error) {
	return &loginsdk.QRCode{
		Content:   "https://qr.example/device",
		Message:   "也可以输入代码 KLMN90",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}, nil
}

func (publicFake) PollQR(ctx context.Context, app loginsdk.App, qr *loginsdk.QRCode) (*loginsdk.QRPoll, error) {
	return &loginsdk.QRPoll{Status: loginsdk.QRWaiting}, nil
}

var _ loginsdk.FormAdapter = (*onlineFormFake)(nil)

// onlineFormFake: a platform whose default app is the online service (paste
// login), like OneDrive and Google Drive.
type onlineFormFake struct{}

func (onlineFormFake) Meta() loginsdk.Meta {
	return loginsdk.Meta{
		ID: "fakeonline", Name: "模拟在线表单盘", Driver: fakeDriverName,
		Method: loginsdk.MethodForm, AppModes: []loginsdk.AppMode{loginsdk.AppOnlineService},
		FormLabel: "粘贴令牌",
		FormFields: []loginsdk.Field{
			{Key: "refresh_token", Label: "刷新令牌", Type: "password", Required: true, Secret: true},
		},
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token", "use_online_api"},
	}
}

func (onlineFormFake) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppOnlineService {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台仅支持在线授权服务")
	}
	if app.ServiceURL == "" {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写在线授权服务地址")
	}
	return nil
}

func (onlineFormFake) Submit(_ context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	rt := strings.TrimSpace(in.Values["refresh_token"])
	if len(rt) < 16 {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "刷新令牌太短")
	}
	return &loginsdk.AuthConfig{
		Driver: fakeDriverName, CredentialType: "oauth_refresh_token", AppMode: loginsdk.AppOnlineService,
		AppIdentity: strings.TrimRight(in.App.ServiceURL, "/"), Renewal: "驱动通过在线授权服务续期",
		Fields:  map[string]any{"refresh_token": rt, "use_online_api": true},
		Secrets: []string{rt},
	}, nil
}

func (onlineFormFake) FormGuide(app loginsdk.App, reauth bool) *loginsdk.Guide {
	return &loginsdk.Guide{
		URL:   strings.TrimRight(app.ServiceURL, "/") + "/",
		Label: "打开 OpenList 授权页",
		Steps: []string{"在打开的页面获取刷新令牌"},
	}
}

// deleteExtraApps removes the app profiles of the adapters defined here
// (tests share one database).
func deleteExtraApps(t *testing.T) {
	t.Helper()
	for _, p := range []string{"fakepublic", "fakeonline"} {
		if err := loginbridge.DeleteApp(p); err != nil {
			t.Fatalf("delete app %s: %v", p, err)
		}
	}
}

func TestDefaultAppPlatformPayload(t *testing.T) {
	deleteAllApps(t)
	deleteExtraApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, []loginsdk.Adapter{publicFake{}, onlineFormFake{}, oauthFake{}})

	_, env := callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env, "platforms")
	var data struct {
		Platforms []struct {
			ID             string         `json:"id"`
			Method         string         `json:"method"`
			ActiveMethod   string         `json:"active_method"`
			FormLabel      string         `json:"form_label"`
			PublicAppLabel string         `json:"public_app_label"`
			App            map[string]any `json:"app"`
		} `json:"platforms"`
	}
	into(t, env, &data)
	if len(data.Platforms) != 3 {
		t.Fatalf("expected 3 platforms, got %d", len(data.Platforms))
	}

	// Default public app: configured, marked default, no client_id, masked
	// identity, and the platform's active method is the scan.
	pub := data.Platforms[0]
	if pub.App["default"] != true || pub.App["configured"] != true ||
		pub.App["mode"] != "public_app" || pub.App["problem"] != "" {
		t.Fatalf("public default app = %v", pub.App)
	}
	if _, has := pub.App["client_id"]; has {
		t.Error("public app payload must omit client_id")
	}
	if idt, _ := pub.App["identity"].(string); !strings.HasPrefix(idt, "publ") || strings.Contains(idt, publicClientID) {
		t.Errorf("identity = %q, want the masked client id", idt)
	}
	if pub.ActiveMethod != "qrcode" || pub.PublicAppLabel != "公开应用说明" {
		t.Errorf("public platform payload wrong: %+v", pub)
	}

	// Default online service: the default service URL is filled in.
	onl := data.Platforms[1]
	if onl.App["default"] != true || onl.App["configured"] != true ||
		onl.App["mode"] != "online_service" || onl.App["service_url"] != loginsdk.DefaultServiceURL {
		t.Fatalf("online default app = %v", onl.App)
	}
	if onl.ActiveMethod != "form" || onl.FormLabel != "粘贴令牌" {
		t.Errorf("online platform payload wrong: %+v", onl)
	}

	// Own-only platform: no default, active_method falls back to Meta.Method.
	own := data.Platforms[2]
	if _, has := own.App["default"]; has || own.App["configured"] != false {
		t.Fatalf("own-only app = %v", own.App)
	}
	if own.ActiveMethod != "oauth" {
		t.Errorf("own-only active_method = %q", own.ActiveMethod)
	}
}

func TestSessionStartNoSavedProfile(t *testing.T) {
	deleteAllApps(t)
	deleteExtraApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, []loginsdk.Adapter{publicFake{}, onlineFormFake{}})

	// The public-app default starts a scan session without any saved profile.
	v := startSession(t, e, "tok-A", "fakepublic", 0)
	if v.Method != loginsdk.MethodQR || v.QR == nil {
		t.Fatalf("public default start = %s qr=%v", v.Method, v.QR)
	}
	if v.App.Mode != loginsdk.AppPublic {
		t.Errorf("app mode = %s", v.App.Mode)
	}
	if v.QR.Message != "也可以输入代码 KLMN90" {
		t.Errorf("qr message = %q", v.QR.Message)
	}

	// The online-service default starts a paste session with a guide.
	v = startSession(t, e, "tok-A", "fakeonline", 0)
	if v.Method != loginsdk.MethodForm || len(v.Form) != 1 || v.FormLabel != "粘贴令牌" {
		t.Fatalf("online default start = %s form=%d label=%q", v.Method, len(v.Form), v.FormLabel)
	}
	if v.Guide == nil || v.Guide.URL != loginsdk.DefaultServiceURL+"/" || len(v.Guide.Steps) != 1 {
		t.Fatalf("guide = %+v", v.Guide)
	}

	// Submitting works against the default app and stages names-only creds.
	const rt = "default-app-refresh-token-1234567890"
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{"refresh_token": rt}})
	wantOK(t, env, "submit")
	var got loginsdk.View
	into(t, env, &got)
	if got.Phase != loginsdk.PhaseAuthorized || got.Auth == nil {
		t.Fatalf("phase = %s auth = %v", got.Phase, got.Auth)
	}
	assertAbsent(t, "refresh token", rt, env.Message, string(env.Data))
}

func TestPublicAppSave(t *testing.T) {
	deleteAllApps(t)
	deleteExtraApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, []loginsdk.Adapter{publicFake{}})

	// Saving public_app ignores whatever client fields came along.
	_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", map[string]any{
		"platform": "fakepublic", "mode": "public_app",
		"client_id": "should-be-ignored-000000", "client_secret": "also-ignored-0000",
	})
	wantOK(t, env, "app/save")
	var saved map[string]any
	into(t, env, &saved)
	if saved["mode"] != "public_app" || saved["configured"] != true {
		t.Fatalf("saved public app = %v", saved)
	}
	if _, has := saved["default"]; has {
		t.Error("a saved public app is not the default")
	}
	if _, has := saved["client_id"]; has {
		t.Error("saved public app payload must omit client_id")
	}
	stored, found, err := loginbridge.GetApp("fakepublic")
	if err != nil || !found {
		t.Fatalf("GetApp: found=%v err=%v", found, err)
	}
	if stored.ClientID != "" || stored.ClientSecret != "" || stored.Mode != loginsdk.AppPublic {
		t.Fatalf("stored profile must keep the mode only: %+v", stored)
	}

	// The saved profile still logs in (ResolveApp re-attaches credentials).
	v := startSession(t, e, "tok-A", "fakepublic", 0)
	if v.App.Mode != loginsdk.AppPublic || v.QR == nil {
		t.Fatalf("session over the saved public app: %+v", v)
	}

	// Deleting the profile returns to the default.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/delete", "tok-A",
		map[string]any{"platform": "fakepublic"})
	wantOK(t, env, "app/delete")
	into(t, env, &saved)
	if saved["default"] != true || saved["configured"] != true || saved["mode"] != "public_app" {
		t.Fatalf("deleted public app = %v", saved)
	}
}

func TestMountSuggest(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	suggest := func(platform string) envelope {
		_, env := callJSON(t, e, "GET", "/api/admin/connect/mount/suggest?platform="+platform, "tok-A", nil)
		return env
	}

	// Free name.
	env := suggest("fakeform")
	wantOK(t, env, "free")
	var data struct {
		MountPath string `json:"mount_path"`
	}
	into(t, env, &data)
	if data.MountPath != "/模拟账号盘" {
		t.Fatalf("free mount = %q", data.MountPath)
	}

	// Taken once → -2.
	st := targetFixture(t, "/模拟账号盘", op.WORK, `{"refresh_token":"t"}`)
	t.Cleanup(func() { db.GetDb().Delete(st) })
	env = suggest("fakeform")
	wantOK(t, env, "taken")
	into(t, env, &data)
	if data.MountPath != "/模拟账号盘-2" {
		t.Fatalf("taken mount = %q, want /模拟账号盘-2", data.MountPath)
	}

	// Unknown platform.
	env = suggest("nope")
	wantErr(t, env, 404, loginsdk.CodeNotFound, "unknown platform")

	// Missing platform parameter.
	_, env2 := callJSON(t, e, "GET", "/api/admin/connect/mount/suggest", "tok-A", nil)
	wantErr(t, env2, 400, loginsdk.CodeInvalidInput, "missing platform")

	// Every candidate taken → "".
	for i := 1; i <= 99; i++ {
		mount := "/模拟扫码盘"
		if i > 1 {
			mount = fmt.Sprintf("/模拟扫码盘-%d", i)
		}
		targetFixture(t, mount, op.WORK, `{"refresh_token":"t"}`)
	}
	t.Cleanup(func() {
		db.GetDb().Where("mount_path LIKE ?", "/模拟扫码盘%").Delete(&model.Storage{})
	})
	env = suggest("fakeqr")
	wantOK(t, env, "all taken")
	into(t, env, &data)
	if data.MountPath != "" {
		t.Fatalf("all-taken mount = %q, want empty", data.MountPath)
	}
}
