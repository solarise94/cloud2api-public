package connect

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

type platformsData struct {
	Module map[string]any `json:"module"`
	// moduleStr reads the string fields of Module (map[string]any is used so
	// the boolean handoff capability flag decodes alongside them).
	CallbackPath         string `json:"callback_path"`
	SuggestedRedirectURI string `json:"suggested_redirect_uri"`
	Platforms            []struct {
		ID     string         `json:"id"`
		Name   string         `json:"name"`
		Driver string         `json:"driver"`
		Method string         `json:"method"`
		App    map[string]any `json:"app"`
	} `json:"platforms"`
}

// moduleStr returns data.Module[key] as a string ("" when absent).
func (d platformsData) moduleStr(key string) string {
	s, _ := d.Module[key].(string)
	return s
}

// moduleBool returns data.Module[key] as a bool (false when absent).
func (d platformsData) moduleBool(key string) bool {
	b, _ := d.Module[key].(bool)
	return b
}

func TestPlatformsShapes(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	oldSite := conf.Conf.SiteURL
	conf.Conf.SiteURL = ""
	t.Cleanup(func() { conf.Conf.SiteURL = oldSite })

	w, env := callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env, "platforms")
	var data platformsData
	into(t, env, &data)

	// Registry order.
	want := []string{"fakeoauth", "fakeqr", "fakeform", "fakeownform"}
	if len(data.Platforms) != len(want) {
		t.Fatalf("expected %d platforms, got %d", len(want), len(data.Platforms))
	}
	for i, id := range want {
		if data.Platforms[i].ID != id {
			t.Errorf("platform %d: expected %s, got %s", i, id, data.Platforms[i].ID)
		}
		if data.Platforms[i].Driver != fakeDriverName {
			t.Errorf("platform %s: wrong driver %s", id, data.Platforms[i].Driver)
		}
	}

	// Module fields.
	if data.moduleStr("name") != "connect" {
		t.Errorf("module.name = %q", data.moduleStr("name"))
	}
	for _, k := range []string{"version", "upstream_version", "upstream_commit", "web_version"} {
		if _, ok := data.Module[k]; !ok {
			t.Errorf("module.%s missing", k)
		}
	}
	if data.moduleStr("version") != Version || data.moduleStr("upstream_version") != conf.Version {
		t.Errorf("module version fields wrong: %+v", data.Module)
	}
	// The handoff entry is an explicit capability flag, not a version.
	if !data.moduleBool("handoff") {
		t.Error("module.handoff = false, want true")
	}

	// Base-path aware callback path.
	if data.CallbackPath != "/api/connect/oauth/callback" {
		t.Errorf("callback_path = %q", data.CallbackPath)
	}
	if data.SuggestedRedirectURI != "" {
		t.Errorf("suggested_redirect_uri = %q, want empty without site_url", data.SuggestedRedirectURI)
	}

	// AppNone shape: exactly the three keys.
	for _, i := range []int{1, 2} { // fakeqr, fakeform
		app := data.Platforms[i].App
		if len(app) != 3 || app["required"] != false || app["configured"] != true || app["mode"] != "none" {
			t.Errorf("platform %s: AppNone app shape wrong: %v", data.Platforms[i].ID, app)
		}
	}

	// Unconfigured own-app shape.
	app := data.Platforms[0].App
	if app["required"] != true || app["configured"] != false || app["mode"] != "" || app["problem"] != "" {
		t.Errorf("unconfigured app shape wrong: %v", app)
	}

	// Save an app and look at the configured shape.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", map[string]any{
		"platform": "fakeoauth", "mode": "own_app",
		"client_id": oauthClientID, "client_secret": oauthClientSecret,
		"redirect_uri": oauthRedirectURI,
	})
	wantOK(t, env, "app/save")
	var saved map[string]any
	into(t, env, &saved)
	if saved["configured"] != true || saved["mode"] != "own_app" || saved["client_id"] != oauthClientID {
		t.Errorf("saved app payload wrong: %v", saved)
	}
	if saved["client_secret_set"] != true {
		t.Errorf("client_secret_set = %v, want true", saved["client_secret_set"])
	}
	if rev, ok := saved["revision"].(float64); !ok || rev != 1 {
		t.Errorf("revision = %v, want 1", saved["revision"])
	}
	if _, has := saved["updated_at"]; !has {
		t.Error("saved app payload missing updated_at")
	}

	// The client secret must never appear in any raw body.
	w2, env2 := callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env2, "platforms 2")
	assertAbsent(t, "client secret", oauthClientSecret, w.Body.String(), w2.Body.String())
	var data2 platformsData
	into(t, env2, &data2)
	app = data2.Platforms[0].App
	if app["configured"] != true || app["client_secret_set"] != true {
		t.Errorf("configured app payload wrong: %v", app)
	}
	if idt, _ := app["identity"].(string); !strings.HasPrefix(idt, "fake") || strings.Contains(idt, oauthClientID) {
		t.Errorf("identity %q must be a masked client id", idt)
	}

	// A saved-but-unusable profile reports its problem.
	row := loginbridge.ConnectApp{
		Platform: "fakeownform", Mode: "own_app",
		ClientID: "incomplete-client-id", UpdatedAt: time.Now(), Revision: 2,
	}
	if err := db.GetDb().Create(&row).Error; err != nil {
		t.Fatalf("seed broken app row: %v", err)
	}
	_, env3 := callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env3, "platforms 3")
	var data3 platformsData
	into(t, env3, &data3)
	app = data3.Platforms[3].App
	if app["configured"] != false || app["problem"] == "" {
		t.Errorf("broken app payload wrong: %v", app)
	}
}

func TestPlatformsBaseAndSiteURL(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	withBase(t, "/sub")

	oldSite := conf.Conf.SiteURL
	t.Cleanup(func() { conf.Conf.SiteURL = oldSite })
	conf.Conf.SiteURL = "https://example.com"
	_, env := callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env, "platforms")
	var data platformsData
	into(t, env, &data)
	if data.CallbackPath != "/sub/api/connect/oauth/callback" {
		t.Errorf("callback_path = %q", data.CallbackPath)
	}
	if data.SuggestedRedirectURI != "https://example.com/api/connect/oauth/callback" {
		t.Errorf("suggested_redirect_uri = %q", data.SuggestedRedirectURI)
	}

	conf.Conf.SiteURL = "example.com"
	_, env = callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env, "platforms 2")
	into(t, env, &data)
	if data.SuggestedRedirectURI != "" {
		t.Errorf("suggested_redirect_uri = %q, want empty without scheme", data.SuggestedRedirectURI)
	}

	withBase(t, "/")
	_, env = callJSON(t, e, "GET", "/api/admin/connect/platforms", "tok-A", nil)
	wantOK(t, env, "platforms 3")
	into(t, env, &data)
	if data.CallbackPath != "/api/connect/oauth/callback" {
		t.Errorf("callback_path with base '/' = %q", data.CallbackPath)
	}
}

func TestAppSaveValidation(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	// Unknown platform.
	_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A",
		map[string]any{"platform": "nope", "mode": "own_app"})
	wantErr(t, env, 404, loginsdk.CodeNotFound, "unknown platform")

	// AppNone platform needs no app.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A",
		map[string]any{"platform": "fakeform", "mode": "none"})
	wantErr(t, env, 400, loginsdk.CodeUnsupported, "appnone platform")

	// Mode the platform does not support.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A",
		map[string]any{"platform": "fakeoauth", "mode": "none"})
	wantErr(t, env, 400, loginsdk.CodeAppInvalid, "bad mode")

	// CheckApp rejection writes nothing.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A",
		map[string]any{"platform": "fakeownform", "mode": "own_app", "client_id": "only-id"})
	wantErr(t, env, 400, loginsdk.CodeAppInvalid, "checkapp rejection")
	if _, found, _ := loginbridge.GetApp("fakeownform"); found {
		t.Error("rejected app was written")
	}

	// Empty platform.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A",
		map[string]any{"platform": "", "mode": "own_app"})
	wantErr(t, env, 400, loginsdk.CodeInvalidInput, "empty platform")

	// Malformed body.
	w := callRaw(e, "POST", "/api/admin/connect/app/save", "tok-A", "{not json")
	var env2 envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env2); err != nil {
		t.Fatalf("malformed body response: %v", err)
	}
	wantErr(t, env2, 400, loginsdk.CodeInvalidInput, "malformed body")

	// A body over 64 KiB is rejected.
	big := strings.Repeat("x", 70*1024)
	w = callRaw(e, "POST", "/api/admin/connect/app/save", "tok-A", `{"client_secret":"`+big+`"}`)
	if err := json.Unmarshal(w.Body.Bytes(), &env2); err != nil {
		t.Fatalf("oversized body response: %v", err)
	}
	wantErr(t, env2, 400, loginsdk.CodeInvalidInput, "oversized body")
}

func TestAppSaveKeepsStoredSecret(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	save := func(extra map[string]any) envelope {
		body := map[string]any{
			"platform": "fakeoauth", "mode": "own_app",
			"client_id": oauthClientID, "redirect_uri": oauthRedirectURI,
		}
		for k, v := range extra {
			body[k] = v
		}
		_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", body)
		return env
	}

	wantOK(t, save(map[string]any{"client_secret": oauthClientSecret}), "first save")

	// Same client id + mode with an empty secret: the stored secret is kept.
	env := save(map[string]any{"client_secret": ""})
	wantOK(t, env, "second save")
	var app map[string]any
	into(t, env, &app)
	if app["client_secret_set"] != true {
		t.Errorf("client_secret_set = %v, want true", app["client_secret_set"])
	}
	if rev, _ := app["revision"].(float64); rev != 2 {
		t.Errorf("revision = %v, want 2", app["revision"])
	}
	stored, found, err := loginbridge.GetApp("fakeoauth")
	if err != nil || !found {
		t.Fatalf("GetApp: found=%v err=%v", found, err)
	}
	if stored.ClientSecret != oauthClientSecret {
		t.Error("stored client secret was not preserved")
	}

	// A different client id with an empty secret cannot inherit: CheckApp
	// rejects and nothing is overwritten.
	env = save(map[string]any{"client_id": "another-client-id-9876", "client_secret": ""})
	wantErr(t, env, 400, loginsdk.CodeAppInvalid, "other client id empty secret")
	stored, _, _ = loginbridge.GetApp("fakeoauth")
	if stored.ClientID != oauthClientID || stored.ClientSecret != oauthClientSecret {
		t.Error("rejected save overwrote the stored app")
	}
}

func TestAppDelete(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", map[string]any{
		"platform": "fakeoauth", "mode": "own_app",
		"client_id": oauthClientID, "client_secret": oauthClientSecret,
		"redirect_uri": oauthRedirectURI,
	})
	wantOK(t, env, "save")

	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/delete", "tok-A",
		map[string]any{"platform": "fakeoauth"})
	wantOK(t, env, "delete")
	var app map[string]any
	into(t, env, &app)
	if app["required"] != true || app["configured"] != false || app["mode"] != "" || app["problem"] != "" {
		t.Errorf("deleted app payload wrong: %v", app)
	}
	if _, found, _ := loginbridge.GetApp("fakeoauth"); found {
		t.Error("app still present after delete")
	}

	// Unknown platform.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/app/delete", "tok-A",
		map[string]any{"platform": "nope"})
	wantErr(t, env, 404, loginsdk.CodeNotFound, "delete unknown platform")
}

// targetFixture inserts a storage row directly, bypassing op, so the stored
// status text is fully controlled.
func targetFixture(t *testing.T, mount, status, addition string) *model.Storage {
	t.Helper()
	st := &model.Storage{
		MountPath: mount, Driver: fakeDriverName, Status: status,
		Addition: addition, Modified: time.Now(),
	}
	if err := db.GetDb().Create(st).Error; err != nil {
		t.Fatalf("insert storage: %v", err)
	}
	return st
}

func TestTarget(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	// Invalid ids.
	for _, id := range []string{"", "abc", "0", "-3", "1.5"} {
		target := "/api/admin/connect/target"
		if id != "" {
			target += "?id=" + id
		}
		_, env := callJSON(t, e, "GET", target, "tok-A", nil)
		wantErr(t, env, 400, loginsdk.CodeInvalidInput, "target id "+id)
	}

	// Unknown id.
	_, env := callJSON(t, e, "GET", "/api/admin/connect/target?id=99991", "tok-A", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "target unknown")

	// A healthy storage: every adapter of the driver is listed.
	st := targetFixture(t, "/tgt-healthy", op.WORK,
		`{"refresh_token":"good-token","root_folder_path":"/","client_id":"","client_secret":"","form_user":"","form_pass":""}`)
	_, env = callJSON(t, e, "GET", fmt.Sprintf("/api/admin/connect/target?id=%d", st.ID), "tok-A", nil)
	wantOK(t, env, "target healthy")
	var data struct {
		StorageID uint      `json:"storage_id"`
		MountPath string    `json:"mount_path"`
		Driver    string    `json:"driver"`
		Status    string    `json:"status"`
		Disabled  bool      `json:"disabled"`
		Modified  time.Time `json:"modified"`
		Platforms []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Problem string `json:"problem"`
		} `json:"platforms"`
		EditPath string `json:"edit_path"`
	}
	into(t, env, &data)
	if data.StorageID != st.ID || data.MountPath != "/tgt-healthy" || data.Driver != fakeDriverName {
		t.Errorf("target identity wrong: %+v", data)
	}
	if data.EditPath != "/@manage/storages/edit/"+fmt.Sprint(st.ID) {
		t.Errorf("edit_path = %q", data.EditPath)
	}
	if len(data.Platforms) != 4 {
		t.Fatalf("expected 4 platforms, got %+v", data.Platforms)
	}
	for _, p := range data.Platforms {
		want := ""
		switch p.ID {
		case "fakeoauth", "fakeownform":
			want = "尚未配置应用"
		}
		if p.Problem != want {
			t.Errorf("platform %s problem = %q, want %q", p.ID, p.Problem, want)
		}
	}

	// A disabled storage: AppNone adapters report CheckTarget's reason.
	st2 := targetFixture(t, "/tgt-disabled", op.WORK,
		`{"refresh_token":"good-token","root_folder_path":"/"}`)
	db.GetDb().Model(st2).Update("disabled", true)
	_, env = callJSON(t, e, "GET", fmt.Sprintf("/api/admin/connect/target?id=%d", st2.ID), "tok-A", nil)
	wantOK(t, env, "target disabled")
	into(t, env, &data)
	var qrProblem string
	for _, p := range data.Platforms {
		if p.ID == "fakeqr" {
			qrProblem = p.Problem
		}
	}
	if qrProblem != "存储已停用，请先在 OpenList 中启用后再重新授权" {
		t.Errorf("disabled problem = %q", qrProblem)
	}

	// A storage whose status echoes its own refresh token: scrubbed.
	secret := "scrub-me-1234-token"
	st3 := targetFixture(t, "/tgt-scrub", "failed init: token "+secret+" rejected",
		`{"refresh_token":"`+secret+`","root_folder_path":"/"}`)
	w, env := callJSON(t, e, "GET", fmt.Sprintf("/api/admin/connect/target?id=%d", st3.ID), "tok-A", nil)
	wantOK(t, env, "target scrub")
	into(t, env, &data)
	if data.Status == "" || strings.Contains(data.Status, secret) {
		t.Errorf("status %q leaks the refresh token", data.Status)
	}
	assertAbsent(t, "refresh token", secret, w.Body.String())

	// edit_path carries the base path.
	withBase(t, "/sub")
	_, env = callJSON(t, e, "GET", fmt.Sprintf("/api/admin/connect/target?id=%d", st.ID), "tok-A", nil)
	wantOK(t, env, "target base")
	into(t, env, &data)
	if data.EditPath != "/sub/@manage/storages/edit/"+fmt.Sprint(st.ID) {
		t.Errorf("edit_path with base = %q", data.EditPath)
	}
}

func TestSessionStartValidation(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	// Unknown platform.
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/start", "tok-A",
		map[string]any{"platform": "nope"})
	wantErr(t, env, 404, loginsdk.CodeNotFound, "start unknown platform")

	// Own-app platform without a saved app.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/start", "tok-A",
		map[string]any{"platform": "fakeoauth"})
	wantErr(t, env, 400, loginsdk.CodeAppMissing, "app missing")
	if !strings.Contains(env.Message, "模拟OAuth盘") {
		t.Errorf("app_missing message should name the platform: %q", env.Message)
	}

	// Malformed body.
	w := callRaw(e, "POST", "/api/admin/connect/session/start", "tok-A", "[1,")
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("malformed start body: %v", err)
	}
	wantErr(t, env, 400, loginsdk.CodeInvalidInput, "start malformed body")

	// Unknown reauth target.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/start", "tok-A",
		map[string]any{"platform": "fakeqr", "reauth_storage_id": 99991})
	wantErr(t, env, 404, loginsdk.CodeNotFound, "start unknown reauth target")

	// Disabled reauth target: CheckTarget rejects.
	st := targetFixture(t, "/start-disabled", op.WORK, `{"refresh_token":"good-token","root_folder_path":"/"}`)
	db.GetDb().Model(st).Update("disabled", true)
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/start", "tok-A",
		map[string]any{"platform": "fakeqr", "reauth_storage_id": int64(st.ID)})
	wantErr(t, env, 409, loginsdk.CodeConflict, "start disabled reauth target")
}

func TestSessionViews(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())

	// An OAuth session with a configured app.
	_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", map[string]any{
		"platform": "fakeoauth", "mode": "own_app",
		"client_id": oauthClientID, "client_secret": oauthClientSecret,
		"redirect_uri": oauthRedirectURI,
	})
	wantOK(t, env, "save app")

	v := startSession(t, e, "tok-A", "fakeoauth", 0)
	if v.Phase != loginsdk.PhasePending || v.Mode != "create" || v.Method != loginsdk.MethodOAuth {
		t.Errorf("oauth view wrong: %+v", v)
	}
	if v.OAuth == nil || !strings.Contains(v.OAuth.AuthorizeURL, "state=") ||
		!strings.Contains(v.OAuth.AuthorizeURL, "code_challenge=") {
		t.Errorf("oauth view missing PKCE authorize url: %+v", v.OAuth)
	}
	if v.OAuth.RedirectURI != oauthRedirectURI {
		t.Errorf("redirect_uri = %q", v.OAuth.RedirectURI)
	}
	if v.App.Mode != loginsdk.AppOwn {
		t.Errorf("app mode = %q", v.App.Mode)
	}

	// A form session (no app needed) shows its create-only field.
	v = startSession(t, e, "tok-A", "fakeform", 0)
	if len(v.Form) != 3 || !v.Form[0].CreateOnly || v.Form[0].Key != "server" {
		t.Errorf("create form fields wrong: %+v", v.Form)
	}

	// Reauth hides create-only fields and pins the target.
	st := targetFixture(t, "/views-reauth", op.WORK, `{"refresh_token":"good-token","root_folder_path":"/"}`)
	v = startSession(t, e, "tok-A", "fakeform", int64(st.ID))
	if v.Mode != "reauth" || v.Target == nil || v.Target.StorageID != st.ID {
		t.Errorf("reauth view wrong: mode=%s target=%+v", v.Mode, v.Target)
	}
	for _, f := range v.Form {
		if f.CreateOnly {
			t.Errorf("reauth form must not contain create-only field %s", f.Key)
		}
	}

	// A form login that fails validation keeps the session pending.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{"user": " ", "pass": "p"}})
	wantErr(t, env, 400, loginsdk.CodeInvalidInput, "empty form user")
	if got := sessionGet(t, e, "tok-A", v.ID); got.Phase != loginsdk.PhasePending {
		t.Errorf("phase after failed submit = %s", got.Phase)
	}

	// A completed CREATE form login stages names-only credentials.
	create := startSession(t, e, "tok-A", "fakeform", 0)
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": create.ID, "values": map[string]string{
			"user": "alice", "pass": "pw-secret-9876", "server": "https://s.example",
		}})
	wantOK(t, env, "form submit")
	var got loginsdk.View
	into(t, env, &got)
	if got.Phase != loginsdk.PhaseAuthorized || got.Auth == nil {
		t.Fatalf("form authorized view wrong: %+v", got)
	}
	wantFields := []string{"form_pass", "form_user", "refresh_token"}
	if strings.Join(got.Auth.Fields, ",") != strings.Join(wantFields, ",") {
		t.Errorf("auth.fields = %v, want %v (values must never appear)", got.Auth.Fields, wantFields)
	}
	if strings.Join(got.Auth.CreateFields, ",") != "server" {
		t.Errorf("auth.create_fields = %v", got.Auth.CreateFields)
	}
	assertAbsent(t, "form password", "pw-secret-9876", env.Message)

	// A second submit on an authorized session conflicts.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": create.ID, "values": map[string]string{"user": "alice", "pass": "x"}})
	wantErr(t, env, 409, loginsdk.CodeConflict, "double submit")

	// Unknown session id.
	_, env = callJSON(t, e, "GET", "/api/admin/connect/session/get?id=zzz", "tok-A", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "unknown session")
}
