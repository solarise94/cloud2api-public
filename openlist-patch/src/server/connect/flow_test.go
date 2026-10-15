package connect

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/gin-gonic/gin"
)

// saveOAuthApp configures the fakeoauth app on engine e.
func saveOAuthApp(t *testing.T, e *gin.Engine) {
	t.Helper()
	_, env := callJSON(t, e, "POST", "/api/admin/connect/app/save", "tok-A", map[string]any{
		"platform": "fakeoauth", "mode": "own_app",
		"client_id": oauthClientID, "client_secret": oauthClientSecret,
		"redirect_uri": oauthRedirectURI,
	})
	wantOK(t, env, "save oauth app")
}

// authorizeOAuth drives an oauth session to authorized via the public
// callback and returns the callback response.
func authorizeOAuth(t *testing.T, e *gin.Engine, v loginsdk.View, code string) string {
	t.Helper()
	u, err := url.Parse(v.OAuth.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("authorize url carries no state")
	}
	return callRaw(e, "GET", "/api/connect/oauth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), "", "").Body.String()
}

func TestCreateFlowOverHTTP(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	saveOAuthApp(t, e)

	// start → callback with a good code → get → save.
	v := startSession(t, e, "tok-A", "fakeoauth", 0)
	page := authorizeOAuth(t, e, v, "GOODCODE")
	if !strings.Contains(page, "授权完成") {
		t.Errorf("callback page misses 授权完成: %s", page)
	}
	if !strings.Contains(page, "/@connect/#/s/"+v.ID) {
		t.Errorf("callback page misses the session link: %s", page)
	}
	u2, _ := url.Parse(v.OAuth.AuthorizeURL)
	for _, needle := range []string{"GOODCODE", oauthClientSecret, "rt-GOODCODE", u2.Query().Get("state")} {
		assertAbsent(t, "callback secret", needle, page)
	}

	got := sessionGet(t, e, "tok-A", v.ID)
	if got.Phase != loginsdk.PhaseAuthorized || got.Auth == nil {
		t.Fatalf("phase after callback = %s", got.Phase)
	}
	if strings.Join(got.Auth.Fields, ",") != "client_id,client_secret,refresh_token" {
		t.Errorf("auth.fields = %v", got.Auth.Fields)
	}

	before := storageCount(t)
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/save", "tok-A", map[string]any{
		"id": v.ID, "mount_path": "/created/oauth-ok", "root": "/x", "remark": "flow",
	})
	wantOK(t, env, "save")
	var saved loginsdk.View
	into(t, env, &saved)
	if saved.Result == nil || !saved.Result.OK || saved.Result.StorageID == 0 {
		t.Fatalf("save result wrong: %+v", saved.Result)
	}
	if saved.Phase != loginsdk.PhaseSaved {
		t.Errorf("phase after save = %s", saved.Phase)
	}
	row, err := db.GetStorageById(saved.Result.StorageID)
	if err != nil {
		t.Fatalf("saved storage missing: %v", err)
	}
	if !strings.Contains(row.Addition, "rt-GOODCODE") {
		t.Errorf("credential not in addition: %s", row.Addition)
	}
	if storageCount(t) != before+1 {
		t.Errorf("storage count %d, want %d", storageCount(t), before+1)
	}

	// Repeating the save returns the same result and creates no second row.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/save", "tok-A", map[string]any{
		"id": v.ID, "mount_path": "/created/oauth-ok2", "root": "/", "remark": "",
	})
	wantOK(t, env, "save again")
	into(t, env, &saved)
	if saved.Result == nil || !saved.Result.OK || saved.Result.StorageID != row.ID {
		t.Fatalf("repeat save result wrong: %+v", saved.Result)
	}
	if storageCount(t) != before+1 {
		t.Errorf("repeat save created a second row: %d", storageCount(t))
	}

	// A failing driver init reports the cleanup and leaves no row.
	v2 := startSession(t, e, "tok-A", "fakeoauth", 0)
	page = authorizeOAuth(t, e, v2, "Q-bad-code")
	if !strings.Contains(page, "授权完成") {
		t.Errorf("exchange of a bad token still authorizes: %s", page)
	}
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/save", "tok-A", map[string]any{
		"id": v2.ID, "mount_path": "/created/oauth-fail", "root": "/", "remark": "",
	})
	wantOK(t, env, "save failing")
	var failed loginsdk.View
	into(t, env, &failed)
	if failed.Result == nil || failed.Result.OK || failed.Result.Cleanup == nil ||
		!failed.Result.Cleanup.Deleted || failed.Result.Cleanup.StorageID == 0 {
		t.Fatalf("failed save result wrong: %+v", failed.Result)
	}
	if failed.Phase != loginsdk.PhaseSaveFailed {
		t.Errorf("phase after failed save = %s", failed.Phase)
	}
	if storageCount(t) != before+1 {
		t.Errorf("failed save left a row behind: %d", storageCount(t))
	}
	// The driver error echoes the refresh token; it must be scrubbed.
	assertAbsent(t, "scrubbed token", "Q-bad-code", env.Message, string(env.Data))
}

func TestCallbackPages(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	saveOAuthApp(t, e)

	// Unknown state → 400 page linking to the page root.
	w := callRaw(e, "GET", "/api/connect/oauth/callback?code=x&state=unknown-state", "", "")
	if w.Code != 400 {
		t.Errorf("unknown state status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/@connect/") || strings.Contains(w.Body.String(), "#/s/") {
		t.Errorf("400 page link wrong: %s", w.Body.String())
	}
	assertAbsent(t, "query echo", "unknown-state", w.Body.String())
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("missing Referrer-Policy")
	}
	if !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("missing callback CSP")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing no-store")
	}

	// Provider-side cancel.
	v := startSession(t, e, "tok-A", "fakeoauth", 0)
	u, _ := url.Parse(v.OAuth.AuthorizeURL)
	state := u.Query().Get("state")
	w = callRaw(e, "GET", "/api/connect/oauth/callback?error=access_denied&state="+url.QueryEscape(state), "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "授权已取消") {
		t.Errorf("cancelled page wrong: %d %s", w.Code, w.Body.String())
	}
	assertAbsent(t, "state echo", state, w.Body.String())

	// Exchange failure: the message echoes the client secret and must be
	// scrubbed; the session fails.
	v2 := startSession(t, e, "tok-A", "fakeoauth", 0)
	u, _ = url.Parse(v2.OAuth.AuthorizeURL)
	state = u.Query().Get("state")
	w = callRaw(e, "GET", "/api/connect/oauth/callback?code=code-fail&state="+url.QueryEscape(state), "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "授权失败") {
		t.Errorf("failed page wrong: %d %s", w.Code, w.Body.String())
	}
	assertAbsent(t, "client secret", oauthClientSecret, w.Body.String())
	got := sessionGet(t, e, "tok-A", v2.ID)
	if got.Phase != loginsdk.PhaseFailed || got.ErrorCode != loginsdk.CodeProvider {
		t.Errorf("phase after failed exchange = %s/%s", got.Phase, got.ErrorCode)
	}

	// A state can be used only once.
	v3 := startSession(t, e, "tok-A", "fakeoauth", 0)
	u, _ = url.Parse(v3.OAuth.AuthorizeURL)
	state = u.Query().Get("state")
	if w := callRaw(e, "GET", "/api/connect/oauth/callback?code=FIRSTCODE&state="+url.QueryEscape(state), "", ""); w.Code != 200 {
		t.Errorf("first use status = %d", w.Code)
	}
	w = callRaw(e, "GET", "/api/connect/oauth/callback?code=SECONDCODE&state="+url.QueryEscape(state), "", "")
	if w.Code != 400 {
		t.Errorf("reused state status = %d", w.Code)
	}
	assertAbsent(t, "reused query", "SECONDCODE", w.Body.String())
	assertAbsent(t, "reused state", state, w.Body.String())
	got = sessionGet(t, e, "tok-A", v3.ID)
	if got.Phase != loginsdk.PhaseAuthorized {
		t.Errorf("phase after state reuse = %s", got.Phase)
	}
}

func TestReauthFlowOverHTTP(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	saveOAuthApp(t, e)

	// A live storage to re-authorize.
	st := &model.Storage{
		MountPath: "/reauth/flow", Driver: fakeDriverName,
		Addition: `{"refresh_token":"good-old-token","root_folder_path":"/old-root","client_id":"old-cid","client_secret":"old-secret"}`,
	}
	id, err := op.CreateStorage(context.Background(), *st)
	if err != nil {
		t.Fatalf("create base storage: %v", err)
	}

	v := startSession(t, e, "tok-A", "fakeoauth", int64(id))
	if v.Mode != "reauth" || v.Target == nil || v.Target.StorageID != id {
		t.Fatalf("reauth start wrong: %+v", v.Target)
	}
	if page := authorizeOAuth(t, e, v, "NEWCODE"); !strings.Contains(page, "授权完成") {
		t.Errorf("reauth callback: %s", page)
	}

	// mount_path/root/remark are ignored on reauth.
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/save", "tok-A", map[string]any{
		"id": v.ID, "mount_path": "/totally-different", "root": "/ignored", "remark": "ignored",
	})
	wantOK(t, env, "reauth save")
	var saved loginsdk.View
	into(t, env, &saved)
	if saved.Result == nil || !saved.Result.OK || saved.Result.StorageID != id {
		t.Fatalf("reauth result wrong: %+v", saved.Result)
	}

	row, err := db.GetStorageById(id)
	if err != nil {
		t.Fatalf("storage gone: %v", err)
	}
	if row.MountPath != "/reauth/flow" || row.Driver != fakeDriverName {
		t.Errorf("reauth changed identity: %s %s", row.MountPath, row.Driver)
	}
	addition, err := loginbridge.DecodeAddition(row.Addition)
	if err != nil {
		t.Fatalf("decode addition: %v", err)
	}
	if addition["refresh_token"] != "rt-NEWCODE" {
		t.Errorf("refresh_token = %v, want the new credential", addition["refresh_token"])
	}
	if addition["client_id"] != oauthClientID {
		t.Errorf("client_id = %v, want the new app credential", addition["client_id"])
	}
	if addition["root_folder_path"] != "/old-root" {
		t.Errorf("root_folder_path = %v, must be preserved", addition["root_folder_path"])
	}
}

func TestOwnerIsolation(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	saveOAuthApp(t, e)

	v := startSession(t, e, "tok-A", "fakeoauth", 0)

	// The same user with another token: not found.
	_, env := callJSON(t, e, "GET", "/api/admin/connect/session/get?id="+v.ID, "tok-B", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "other token of same user")

	// Another user: also not found, indistinguishable.
	_, env = callJSON(t, e, "GET", "/api/admin/connect/session/get?id="+v.ID, "tok-C", nil)
	wantErr(t, env, 404, loginsdk.CodeNotFound, "other user")

	// The owning token still sees it, and can save it.
	_ = sessionGet(t, e, "tok-A", v.ID)
}

func TestErrorEnvelopeMapping(t *testing.T) {
	deleteAllApps(t)

	// limit: an owner with one live session slot.
	_, e := newTestEngine(t, loginsdk.Config{MaxPerOwner: 1}, defaultAdapters())
	v := startSession(t, e, "tok-A", "fakeqr", 0)
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/start", "tok-A",
		map[string]any{"platform": "fakeqr"})
	wantErr(t, env, 429, loginsdk.CodeLimit, "limit")

	// cancelled: cancelling then submitting.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/cancel", "tok-A",
		map[string]any{"id": v.ID})
	wantOK(t, env, "cancel")
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{}})
	wantErr(t, env, 410, loginsdk.CodeCancelled, "submit after cancel")

	// state_mismatch via a pasted callback url with a foreign state.
	_, e = newTestEngine(t, loginsdk.Config{}, defaultAdapters())
	saveOAuthApp(t, e)
	v = startSession(t, e, "tok-A", "fakeoauth", 0)
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{
			"callback_url": "https://cb.example/api/connect/oauth/callback?code=X&state=not-my-state",
		}})
	wantErr(t, env, 400, loginsdk.CodeStateMismatch, "foreign state")

	// provider: a pasted callback url whose exchange fails.
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{
			"callback_url": "https://cb.example/api/connect/oauth/callback?code=code-fail&state=" + stateOf(t, v),
		}})
	wantErr(t, env, 502, loginsdk.CodeProvider, "provider rejection")
	assertAbsent(t, "client secret", oauthClientSecret, env.Message)
}

func TestExpiredSessionEnvelope(t *testing.T) {
	deleteAllApps(t)
	_, e := newTestEngine(t, loginsdk.Config{PendingTTL: 30 * time.Millisecond}, defaultAdapters())

	v := startSession(t, e, "tok-A", "fakeqr", 0)
	time.Sleep(60 * time.Millisecond)
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/submit", "tok-A",
		map[string]any{"id": v.ID, "values": map[string]string{}})
	wantErr(t, env, 410, loginsdk.CodeExpired, "submit after expiry")
}

func TestQRSessionOverHTTP(t *testing.T) {
	deleteAllApps(t)
	// Fast polling so the scripted statuses advance without real waiting.
	_, e := newTestEngine(t, loginsdk.Config{MinPollEvery: 10 * time.Millisecond}, defaultAdapters(
		loginsdk.QRScanned, loginsdk.QRConfirmed,
	))

	v := startSession(t, e, "tok-A", "fakeqr", 0)
	if v.QR == nil || v.QR.Status != loginsdk.QRWaiting || !strings.Contains(v.QR.Content, "ticket-1") {
		t.Fatalf("initial qr view wrong: %+v", v.QR)
	}

	poll := func() loginsdk.QRStatus {
		got := sessionGet(t, e, "tok-A", v.ID)
		if got.QR == nil {
			t.Fatalf("qr view lost: %+v", got)
		}
		return got.QR.Status
	}
	waitStatus := func(want loginsdk.QRStatus) {
		t.Helper()
		for i := 0; i < 50; i++ {
			if poll() == want {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("qr never reached %s", want)
	}
	waitStatus(loginsdk.QRScanned)
	waitStatus(loginsdk.QRConfirmed)

	got := sessionGet(t, e, "tok-A", v.ID)
	if got.Phase != loginsdk.PhaseAuthorized {
		t.Fatalf("phase after confirm = %s", got.Phase)
	}
	if got.Auth == nil || strings.Join(got.Auth.Fields, ",") != "refresh_token" {
		t.Errorf("auth fields wrong: %+v", got.Auth)
	}

	// Refresh issues a new ticket on a pending qr session.
	v2 := startSession(t, e, "tok-A", "fakeqr", 0)
	if v2.QR == nil || !strings.Contains(v2.QR.Content, "ticket-2") {
		t.Fatalf("second qr session start wrong: %+v", v2.QR)
	}
	_, env := callJSON(t, e, "POST", "/api/admin/connect/session/refresh", "tok-A",
		map[string]any{"id": v2.ID})
	wantOK(t, env, "refresh")
	var refreshed loginsdk.View
	into(t, env, &refreshed)
	if refreshed.QR == nil || refreshed.QR.Content == v2.QR.Content ||
		!strings.Contains(refreshed.QR.Content, "ticket-") {
		t.Errorf("refreshed qr wrong: %+v (was %s)", refreshed.QR, v2.QR.Content)
	}

	// Refresh on a non-qr session is invalid.
	v3 := startSession(t, e, "tok-A", "fakeform", 0)
	_, env = callJSON(t, e, "POST", "/api/admin/connect/session/refresh", "tok-A",
		map[string]any{"id": v3.ID})
	wantErr(t, env, 400, loginsdk.CodeInvalidInput, "refresh non-qr")
}

// stateOf extracts the oauth state of a pending session view.
func stateOf(t *testing.T, v loginsdk.View) string {
	t.Helper()
	u, err := url.Parse(v.OAuth.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	return u.Query().Get("state")
}
