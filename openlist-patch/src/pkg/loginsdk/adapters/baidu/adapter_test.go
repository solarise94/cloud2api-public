package baidu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var testApp = loginsdk.App{
	Platform:     "baidu",
	Mode:         loginsdk.AppOwn,
	ClientID:     "appkey-1234567890abcdef",
	ClientSecret: "secretkey-abcdefghijklmnop",
	RedirectURI:  "http://127.0.0.1:5244/api/connect/oauth/callback",
}

var onlineApp = loginsdk.App{
	Platform:   "baidu",
	Mode:       loginsdk.AppOnlineService,
	ServiceURL: "https://api.oplist.org/",
}

// pubApp is the public app as the connect layer resolves it for a session.
var pubApp = loginsdk.App{
	Platform:     "baidu",
	Mode:         loginsdk.AppPublic,
	ClientID:     publicApp.ClientID,
	ClientSecret: publicApp.ClientSecret,
	RedirectURI:  publicApp.RedirectURI,
}

// fakeProvider points the adapter's endpoints and HTTP client at a test
// server and restores everything afterwards.
func fakeProvider(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	oldClient := loginsdk.HTTPClient
	loginsdk.HTTPClient = srv.Client()
	setVar(t, &tokenURL, srv.URL+"/oauth/2.0/token")
	setVar(t, &deviceCodeURL, srv.URL+"/oauth/2.0/device/code")
	setVar(t, &uinfoURL, srv.URL+"/rest/2.0/xpan/nas")
	t.Cleanup(func() {
		loginsdk.HTTPClient = oldClient
		srv.Close()
	})
	return srv
}

func setVar(t *testing.T, dst *string, v string) {
	t.Helper()
	old := *dst
	*dst = v
	t.Cleanup(func() { *dst = old })
}

func TestValidateMeta(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := New().Meta()
	if m.AppModes[0] != loginsdk.AppPublic {
		t.Fatalf("default app mode = %s, want public_app", m.AppModes[0])
	}
	for mode, want := range map[loginsdk.AppMode]loginsdk.Method{
		loginsdk.AppPublic:        loginsdk.MethodQR,
		loginsdk.AppOnlineService: loginsdk.MethodForm,
		loginsdk.AppOwn:           loginsdk.MethodOAuth,
	} {
		if got := m.MethodFor(mode); got != want {
			t.Errorf("MethodFor(%s) = %s, want %s", mode, got, want)
		}
	}
	if m.FormLabel != "粘贴令牌" || m.PublicAppLabel == "" {
		t.Errorf("labels = %q / %q", m.FormLabel, m.PublicAppLabel)
	}
}

func TestPublicApp(t *testing.T) {
	app := New().(loginsdk.PublicAppProvider).PublicApp()
	if app.Mode != loginsdk.AppPublic || app.ClientID == "" || app.ClientSecret == "" || app.RedirectURI != "oob" {
		t.Fatalf("PublicApp = %+v", app)
	}
	def, ok := loginsdk.DefaultApp(New())
	if !ok || def.ClientID != app.ClientID || def.ClientSecret != app.ClientSecret || def.Platform != "baidu" {
		t.Fatalf("DefaultApp = %+v ok=%v", def, ok)
	}
	resolved := loginsdk.ResolveApp(New(), loginsdk.App{Platform: "baidu", Mode: loginsdk.AppPublic, Revision: 4})
	if resolved.ClientID != app.ClientID || resolved.ClientSecret != app.ClientSecret || resolved.Revision != 4 {
		t.Fatalf("ResolveApp = %+v", resolved)
	}
}

func TestCheckApp(t *testing.T) {
	a := New()
	if err := a.CheckApp(testApp); err != nil {
		t.Fatalf("valid own app: %v", err)
	}
	if err := a.CheckApp(onlineApp); err != nil {
		t.Fatalf("valid online service: %v", err)
	}
	pub := loginsdk.App{Mode: loginsdk.AppPublic}
	if err := a.CheckApp(pub); err != nil {
		t.Fatalf("public app needs no fields: %v", err)
	}
	cases := []struct {
		name string
		app  loginsdk.App
	}{
		{"bad mode", loginsdk.App{Mode: loginsdk.AppNone}},
		{"missing client id", loginsdk.App{Mode: loginsdk.AppOwn, ClientSecret: "s", RedirectURI: testApp.RedirectURI}},
		{"missing client secret", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", RedirectURI: testApp.RedirectURI}},
		{"missing redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s"}},
		{"relative redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "/callback"}},
		{"redirect uri with fragment", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "http://h/cb#x"}},
		{"missing service url", loginsdk.App{Mode: loginsdk.AppOnlineService}},
		{"service url with query", loginsdk.App{Mode: loginsdk.AppOnlineService, ServiceURL: "https://s.example/?x=1"}},
		{"relative service url", loginsdk.App{Mode: loginsdk.AppOnlineService, ServiceURL: "/svc"}},
	}
	for _, c := range cases {
		err := a.CheckApp(c.app)
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
			t.Errorf("%s: want CodeAppInvalid, got %v", c.name, err)
		}
	}
	oob := testApp
	oob.RedirectURI = "oob"
	if err := a.CheckApp(oob); err != nil {
		t.Errorf("oob redirect uri: %v", err)
	}
}

func TestAuthorizeURL(t *testing.T) {
	raw, err := New().(loginsdk.OAuthAdapter).AuthorizeURL(testApp, "state-123", loginsdk.PKCE{})
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Scheme != "https" || u.Host != "openapi.baidu.com" || u.Path != "/oauth/2.0/authorize" {
		t.Fatalf("unexpected authorize endpoint: %s", raw)
	}
	want := url.Values{
		"response_type": {"code"},
		"client_id":     {testApp.ClientID},
		"redirect_uri":  {testApp.RedirectURI},
		"scope":         {"basic,netdisk"},
		"state":         {"state-123"},
		"display":       {"page"},
	}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Fatalf("authorize query = %v, want %v", u.Query(), want)
	}
}

// deviceCodeHandler answers one device-code request and records its query.
func deviceCodeHandler(t *testing.T, gotQuery *url.Values, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/2.0/device/code" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		*gotQuery = r.URL.Query()
		fmt.Fprint(w, body)
	})
}

func TestStartQRDeviceCode(t *testing.T) {
	var gotQuery url.Values
	fakeProvider(t, deviceCodeHandler(t, &gotQuery,
		`{"device_code":"dc-123456","user_code":"AB12CD","verification_url":"https://openapi.baidu.com/device",`+
			`"qrcode_url":"https://openapi.baidu.com/device/qrcode/ab12/AB12CD","expires_in":300,"interval":5}`))
	qr, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp)
	if err != nil {
		t.Fatalf("StartQR: %v", err)
	}
	want := url.Values{
		"response_type": {"device_code"},
		"client_id":     {pubApp.ClientID},
		"scope":         {"basic,netdisk"},
	}
	if !reflect.DeepEqual(gotQuery, want) {
		t.Fatalf("device code query = %v, want %v", gotQuery, want)
	}
	if qr.ImageURL != "https://openapi.baidu.com/device/qrcode/ab12/AB12CD" {
		t.Fatalf("ImageURL = %q", qr.ImageURL)
	}
	if qr.Private["device_code"] != "dc-123456" {
		t.Fatalf("Private = %v", qr.Private)
	}
	if !strings.Contains(qr.Message, "https://openapi.baidu.com/device") || !strings.Contains(qr.Message, "AB12CD") {
		t.Fatalf("Message = %q, want the user-code hint", qr.Message)
	}
	if d := time.Until(qr.ExpiresAt); d < 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("ExpiresAt = %v (want ~300s)", d)
	}
	if qr.PollEvery != 5*time.Second {
		t.Fatalf("PollEvery = %v, want 5s", qr.PollEvery)
	}
}

func TestStartQRDeviceCodeDefaults(t *testing.T) {
	// No expires_in/interval: defaults 300 s and 5 s.
	var q url.Values
	fakeProvider(t, deviceCodeHandler(t, &q,
		`{"device_code":"dc","user_code":"UC","qrcode_url":"https://openapi.baidu.com/device/qrcode/x/UC"}`))
	qr, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp)
	if err != nil {
		t.Fatalf("StartQR: %v", err)
	}
	if d := time.Until(qr.ExpiresAt); d < 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("default ExpiresAt = %v (want 300s)", d)
	}
	if qr.PollEvery != 5*time.Second {
		t.Fatalf("default PollEvery = %v", qr.PollEvery)
	}

	// expires_in is capped at 15 minutes; a smaller interval still polls at 5 s.
	fakeProvider(t, deviceCodeHandler(t, &q,
		`{"device_code":"dc","user_code":"UC","qrcode_url":"https://qr.example/x.png","expires_in":99999,"interval":1}`))
	qr, err = New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp)
	if err != nil {
		t.Fatalf("StartQR capped: %v", err)
	}
	if d := time.Until(qr.ExpiresAt); d > 15*time.Minute {
		t.Fatalf("ExpiresAt = %v, want the 15-minute cap", d)
	}
	if qr.PollEvery != 5*time.Second {
		t.Fatalf("PollEvery = %v, want the 5s floor", qr.PollEvery)
	}
}

func TestStartQRErrors(t *testing.T) {
	var q url.Values
	// A provider error in the body, echoed with the client secret.
	fakeProvider(t, deviceCodeHandler(t, &q,
		fmt.Sprintf(`{"error":"invalid_client","error_description":"client %s unknown"}`, pubApp.ClientSecret)))
	_, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), pubApp.ClientSecret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}

	// Non-https QR image and missing fields.
	fakeProvider(t, deviceCodeHandler(t, &q, `{"device_code":"dc","user_code":"UC","qrcode_url":"http://qr.example/x.png"}`))
	if _, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("http image URL must be rejected, got %v", err)
	}
	fakeProvider(t, deviceCodeHandler(t, &q, `{"device_code":"dc","qrcode_url":"https://qr.example/x.png"}`))
	if _, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), pubApp); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("missing user code must be rejected, got %v", err)
	}

	// Only the public app scans.
	if _, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), testApp); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
		t.Fatalf("own app must not scan, got %v", err)
	}
}

// deviceTokenHandler answers the device-token poll and records its query.
func deviceTokenHandler(t *testing.T, gotQuery *url.Values, body string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/2.0/token" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		*gotQuery = r.URL.Query()
		if status != 0 {
			w.WriteHeader(status)
		}
		fmt.Fprint(w, body)
	})
}

func TestPollQRDeviceMappings(t *testing.T) {
	a := New().(loginsdk.QRAdapter)
	cases := []struct {
		name string
		body string
	}{
		{"pending", `{"error":"authorization_pending"}`},
		{"slow_down", `{"error":"slow_down"}`},
		{"expired_token", `{"error":"expired_token"}`},
		{"invalid_grant", `{"error":"invalid_grant","error_description":"invalid code , expired or revoked"}`},
		{"access_denied", `{"error":"access_denied"}`},
	}
	for _, c := range cases {
		var q url.Values
		// Baidu reports these with any HTTP status; 400 here proves the body wins.
		fakeProvider(t, deviceTokenHandler(t, &q, c.body, http.StatusBadRequest))
		poll, err := a.PollQR(context.Background(), pubApp, &loginsdk.QRCode{
			Private: map[string]string{"device_code": "dc-poll"},
		})
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := map[string]loginsdk.QRStatus{
			"pending":       loginsdk.QRWaiting,
			"slow_down":     loginsdk.QRWaiting,
			"expired_token": loginsdk.QRExpired,
			"invalid_grant": loginsdk.QRExpired,
			"access_denied": loginsdk.QRCancelled,
		}[c.name]
		if poll.Status != want || poll.Auth != nil {
			t.Errorf("%s: poll = %+v, want status %s without auth", c.name, poll, want)
		}
		if !reflect.DeepEqual(q, url.Values{
			"grant_type":    {"device_token"},
			"code":          {"dc-poll"},
			"client_id":     {pubApp.ClientID},
			"client_secret": {pubApp.ClientSecret},
		}) {
			t.Errorf("%s: device token query = %v", c.name, q)
		}
	}

	// Any other error is a scrubbed provider failure.
	var q url.Values
	fakeProvider(t, deviceTokenHandler(t, &q,
		fmt.Sprintf(`{"error":"server_error","error_description":"boom %s"}`, pubApp.ClientSecret), 0))
	_, err := a.PollQR(context.Background(), pubApp, &loginsdk.QRCode{Private: map[string]string{"device_code": "dc"}})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), pubApp.ClientSecret) {
		t.Fatalf("error leaks the public client secret: %s", err)
	}
}

func TestPollQRConfirmed(t *testing.T) {
	var q url.Values
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/2.0/token":
			q = r.URL.Query()
			fmt.Fprint(w, `{"access_token":"at-public","refresh_token":"rt-public-123456","expires_in":7776000,"scope":"basic,netdisk"}`)
		case "/rest/2.0/xpan/nas":
			fmt.Fprint(w, `{"errno":0,"netdisk_name":"公开应用昵称"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	poll, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), pubApp, &loginsdk.QRCode{
		Private: map[string]string{"device_code": "dc-ok"},
	})
	if err != nil {
		t.Fatalf("PollQR: %v", err)
	}
	if !reflect.DeepEqual(q, url.Values{
		"grant_type":    {"device_token"},
		"code":          {"dc-ok"},
		"client_id":     {pubApp.ClientID},
		"client_secret": {pubApp.ClientSecret},
	}) {
		t.Fatalf("device token query = %v", q)
	}
	if poll.Status != loginsdk.QRConfirmed || poll.Auth == nil {
		t.Fatalf("poll = %+v", poll)
	}
	auth := poll.Auth
	wantFields := map[string]any{
		"refresh_token":  "rt-public-123456",
		"AccessToken":    "at-public",
		"use_online_api": false,
		"client_id":      pubApp.ClientID,
		"client_secret":  pubApp.ClientSecret,
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if auth.Driver != "BaiduNetdisk" || auth.CredentialType != "oauth_refresh_token" ||
		auth.AppMode != loginsdk.AppPublic || auth.AppIdentity != pubApp.ClientID {
		t.Fatalf("auth header fields wrong: %+v", auth)
	}
	if auth.Account != "公开应用昵称" {
		t.Fatalf("Account = %q", auth.Account)
	}
	if !strings.Contains(auth.Renewal, "公开应用") || !strings.Contains(auth.Renewal, "use_online_api=false") {
		t.Fatalf("Renewal = %q", auth.Renewal)
	}
	wantSecrets := map[string]bool{"rt-public-123456": false, "at-public": false, pubApp.ClientSecret: false}
	for _, s := range auth.Secrets {
		wantSecrets[s] = true
	}
	for s, seen := range wantSecrets {
		if !seen {
			t.Errorf("missing secret %q", s)
		}
	}
	if err := loginsdk.ValidateAuth(New(), pubApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestPollQRSuccessMissingToken(t *testing.T) {
	fakeProvider(t, deviceTokenHandler(t, &url.Values{}, `{"access_token":"only-at"}`, 0))
	_, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), pubApp, &loginsdk.QRCode{
		Private: map[string]string{"device_code": "dc-m"},
	})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
}

func TestSubmitOnlinePaste(t *testing.T) {
	a := New().(loginsdk.FormAdapter)
	const rt = "122.a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6"
	app := onlineApp
	auth, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: app, Values: map[string]string{"refresh_token": "  " + rt + "  "}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc := "https://api.oplist.org"
	wantFields := map[string]any{
		"refresh_token":   rt,
		"AccessToken":     "",
		"use_online_api":  true,
		"api_url_address": svc + "/baiduyun/renewapi",
		"client_id":       "",
		"client_secret":   "",
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if b, ok := auth.Fields["use_online_api"].(bool); !ok || !b {
		t.Fatalf("use_online_api must be bool true, got %#v", auth.Fields["use_online_api"])
	}
	if auth.AppMode != loginsdk.AppOnlineService || auth.AppIdentity != svc {
		t.Fatalf("mode/identity = %s/%s", auth.AppMode, auth.AppIdentity)
	}
	if !strings.Contains(auth.Renewal, svc) || !strings.Contains(auth.Renewal, "use_online_api=true") {
		t.Fatalf("Renewal = %q", auth.Renewal)
	}
	if len(auth.Secrets) != 1 || auth.Secrets[0] != rt {
		t.Fatalf("Secrets = %v", auth.Secrets)
	}
	if err := loginsdk.ValidateAuth(New(), app, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}

	// The api.oplist.org callback hash and a JSON object both decode.
	hash := "https://api.oplist.org/#" + base64.StdEncoding.EncodeToString(
		[]byte(`{"driver_txt":"baiduyun_go","refresh_token":"`+rt+`"}`))
	if auth, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: app, Values: map[string]string{"refresh_token": hash}}); err != nil || auth.Fields["refresh_token"] != rt {
		t.Fatalf("hash paste: %v %+v", err, auth)
	}
	if auth, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: app, Values: map[string]string{"refresh_token": `{"refresh_token":"` + rt + `"}`}}); err != nil || auth.Fields["refresh_token"] != rt {
		t.Fatalf("json paste: %v %+v", err, auth)
	}

	// Bad pastes and wrong modes are rejected.
	for name, values := range map[string]map[string]string{
		"empty":   {"refresh_token": " "},
		"garbage": {"refresh_token": "https://x.example/#not-a-token"},
	} {
		if _, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: app, Values: values}); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeInvalidInput {
			t.Errorf("%s paste: want CodeInvalidInput, got %v", name, err)
		}
	}
	if _, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: testApp, Values: map[string]string{"refresh_token": rt}}); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
		t.Errorf("own app submit: want CodeAppInvalid, got %v", err)
	}
}

func TestFormGuide(t *testing.T) {
	a := New().(loginsdk.FormGuider)
	g := a.FormGuide(onlineApp, false)
	if g == nil {
		t.Fatal("online app must have a guide")
	}
	wantHash := base64.StdEncoding.EncodeToString([]byte(`{"driver_txt":"baiduyun_go","server_use":"true"}`))
	if g.URL != "https://api.oplist.org/#"+wantHash {
		t.Fatalf("guide URL = %q", g.URL)
	}
	if g.Label != "打开 OpenList 授权页" {
		t.Fatalf("label = %q", g.Label)
	}
	if len(g.Steps) != 3 {
		t.Fatalf("steps = %v", g.Steps)
	}
	var probe map[string]string
	data, _ := base64.StdEncoding.DecodeString(g.URL[strings.Index(g.URL, "#")+1:])
	if err := json.Unmarshal(data, &probe); err != nil || probe["driver_txt"] != "baiduyun_go" {
		t.Fatalf("hash payload = %s (%v)", data, err)
	}
	if a.FormGuide(testApp, true) != nil {
		t.Fatal("non-online apps must not get a guide")
	}
}

func TestExchange(t *testing.T) {
	var gotQuery url.Values
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/2.0/token":
			gotQuery = r.URL.Query()
			fmt.Fprint(w, `{"access_token":"at-fresh-token","refresh_token":"rt-long-token","expires_in":7776000}`)
		case "/rest/2.0/xpan/nas":
			if r.URL.Query().Get("method") != "uinfo" {
				t.Errorf("uinfo method param = %q", r.URL.Query().Get("method"))
			}
			if r.URL.Query().Get("access_token") != "at-fresh-token" {
				t.Errorf("uinfo access_token param = %q", r.URL.Query().Get("access_token"))
			}
			fmt.Fprint(w, `{"errno":0,"netdisk_name":"网盘昵称","baidu_name":"百度昵称"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))

	auth, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-xyz", loginsdk.PKCE{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	wantQuery := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-xyz"},
		"client_id":     {testApp.ClientID},
		"client_secret": {testApp.ClientSecret},
		"redirect_uri":  {testApp.RedirectURI},
	}
	if !reflect.DeepEqual(gotQuery, wantQuery) {
		t.Fatalf("token query = %v, want %v", gotQuery, wantQuery)
	}
	wantFields := map[string]any{
		"refresh_token":  "rt-long-token",
		"AccessToken":    "at-fresh-token",
		"use_online_api": false,
		"client_id":      testApp.ClientID,
		"client_secret":  testApp.ClientSecret,
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if b, ok := auth.Fields["use_online_api"].(bool); !ok || b {
		t.Fatalf("use_online_api must be bool false, got %#v", auth.Fields["use_online_api"])
	}
	if auth.Account != "网盘昵称" {
		t.Fatalf("Account = %q", auth.Account)
	}
	if auth.Driver != "BaiduNetdisk" || auth.CredentialType != "oauth_refresh_token" ||
		auth.AppMode != loginsdk.AppOwn || auth.AppIdentity != testApp.ClientID {
		t.Fatalf("auth header fields wrong: %+v", auth)
	}
	if auth.Renewal == "" || strings.Contains(auth.Renewal, "在线") {
		t.Fatalf("Renewal = %q", auth.Renewal)
	}
	wantSecrets := map[string]bool{"rt-long-token": false, "at-fresh-token": false, testApp.ClientSecret: false, "code-xyz": false}
	for _, s := range auth.Secrets {
		if _, ok := wantSecrets[s]; !ok {
			t.Errorf("unexpected secret %q", s)
		}
		wantSecrets[s] = true
	}
	for s, seen := range wantSecrets {
		if !seen {
			t.Errorf("missing secret %q", s)
		}
	}
	if err := loginsdk.ValidateAuth(New(), testApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestExchangeAccountLookupIgnored(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/2.0/token":
			fmt.Fprint(w, `{"access_token":"at2","refresh_token":"rt2"}`)
		case "/rest/2.0/xpan/nas":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	auth, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-2", loginsdk.PKCE{})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if auth.Account != "" {
		t.Fatalf("Account = %q, want empty on uinfo failure", auth.Account)
	}
	if auth.Fields["refresh_token"] != "rt2" {
		t.Fatalf("refresh_token = %v", auth.Fields["refresh_token"])
	}
}

func TestExchangeProviderError(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The provider echoes the client secret in its error text.
		fmt.Fprintf(w, `{"error":"invalid_request","error_description":"client_secret %s is wrong"}`, testApp.ClientSecret)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-3", loginsdk.PKCE{})
	if err == nil {
		t.Fatal("want error")
	}
	if loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("code = %s, want provider", loginsdk.CodeOf(err))
	}
	if strings.Contains(err.Error(), testApp.ClientSecret) {
		t.Fatalf("error leaks the client secret: %s", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}
	if !strings.Contains(err.Error(), "百度授权失败") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeErrorOnHTTP200(t *testing.T) {
	// Baidu reports failures in the body with any HTTP status.
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"expired_token","error_description":"authorization code expired"}`)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-4", loginsdk.PKCE{})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if !strings.Contains(err.Error(), "authorization code expired") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeMissingRefreshToken(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"only-access"}`)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-5", loginsdk.PKCE{})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
}

func TestExchangeNetworkError(t *testing.T) {
	srv := fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "code-6", loginsdk.PKCE{})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeNetwork {
		t.Fatalf("want CodeNetwork, got %v", err)
	}
}

// The fake provider above answers JSON; ensure a token response decodes through
// the SDK's TokenResponse (guards against tag drift).
func TestTokenResponseDecoding(t *testing.T) {
	var tok loginsdk.TokenResponse
	if err := json.Unmarshal([]byte(`{"access_token":"a","refresh_token":"r","error":"e","error_description":"d"}`), &tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "a" || tok.RefreshToken != "r" || tok.Error != "e" || tok.ErrorDescription != "d" {
		t.Fatalf("decoded = %+v", tok)
	}
}
