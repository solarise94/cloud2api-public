package onedrive

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

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var testApp = loginsdk.App{
	Platform:     "onedrive",
	Mode:         loginsdk.AppOwn,
	ClientID:     "client-id-1234567890abcdef",
	ClientSecret: "client-secret-abcdefghijklmnop",
	RedirectURI:  "http://127.0.0.1:5244/api/connect/oauth/callback",
}

var testPKCE = loginsdk.PKCE{Verifier: "verifier-1234567890abcdef", Challenge: "challenge-1234567890", Method: "S256"}

var onlineApp = loginsdk.App{
	Platform:   "onedrive",
	Mode:       loginsdk.AppOnlineService,
	ServiceURL: "https://api.oplist.org/",
}

func fakeProvider(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	oldClient := loginsdk.HTTPClient
	loginsdk.HTTPClient = srv.Client()
	for _, pv := range []struct {
		dst *string
		val string
	}{
		{&tokenURL, srv.URL + "/common/oauth2/v2.0/token"},
		{&driveURL, srv.URL + "/v1.0/me/drive"},
	} {
		old := *pv.dst
		*pv.dst = pv.val
		t.Cleanup(func() { *pv.dst = old })
	}
	t.Cleanup(func() {
		loginsdk.HTTPClient = oldClient
		srv.Close()
	})
}

func TestValidateMeta(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := New().Meta()
	if !m.PKCE {
		t.Fatal("Meta.PKCE must be true")
	}
	if m.AppModes[0] != loginsdk.AppOnlineService {
		t.Fatalf("default app mode = %s, want online_service", m.AppModes[0])
	}
	if got := m.MethodFor(loginsdk.AppOnlineService); got != loginsdk.MethodForm {
		t.Fatalf("MethodFor(online_service) = %s, want form", got)
	}
	if got := m.MethodFor(loginsdk.AppOwn); got != loginsdk.MethodOAuth {
		t.Fatalf("MethodFor(own_app) = %s, want oauth", got)
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
	cases := []struct {
		name string
		app  loginsdk.App
	}{
		{"bad mode", loginsdk.App{Mode: loginsdk.AppNone}},
		{"missing service url", loginsdk.App{Mode: loginsdk.AppOnlineService}},
		{"service url with query", loginsdk.App{Mode: loginsdk.AppOnlineService, ServiceURL: "https://s.example/?x=1"}},
		{"missing client id", loginsdk.App{Mode: loginsdk.AppOwn, ClientSecret: "s", RedirectURI: testApp.RedirectURI}},
		{"missing client secret", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", RedirectURI: testApp.RedirectURI}},
		{"missing redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s"}},
		{"oob not allowed", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "oob"}},
		{"relative redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "/cb"}},
		{"userinfo in redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "http://u:p@h/cb"}},
	}
	for _, c := range cases {
		err := a.CheckApp(c.app)
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
			t.Errorf("%s: want CodeAppInvalid, got %v", c.name, err)
		}
	}
}

func TestSubmitOnline(t *testing.T) {
	a := New().(loginsdk.FormAdapter)
	const rt = "0.ATcAe1B2C3D4E5F6G7H8I9J0K1L2M3N4"
	auth, err := a.Submit(context.Background(), loginsdk.SubmitInput{
		App:    onlineApp,
		Values: map[string]string{"refresh_token": "  " + rt + "  "},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc := "https://api.oplist.org"
	wantFields := map[string]any{
		"refresh_token":   rt,
		"use_online_api":  true,
		"api_url_address": svc + "/onedrive/renewapi",
		"client_id":       "",
		"client_secret":   "",
		"redirect_uri":    svc + "/onedrive/callback",
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if !reflect.DeepEqual(auth.CreateFields, map[string]any{"region": "global", "is_sharepoint": false}) {
		t.Fatalf("CreateFields = %#v", auth.CreateFields)
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
	if err := loginsdk.ValidateAuth(New(), onlineApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}

	// Bad paste and wrong mode are rejected.
	if _, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: onlineApp, Values: map[string]string{"refresh_token": "short"}}); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeInvalidInput {
		t.Errorf("short paste: want CodeInvalidInput, got %v", err)
	}
	if _, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: testApp, Values: map[string]string{"refresh_token": rt}}); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
		t.Errorf("own app submit: want CodeAppInvalid, got %v", err)
	}
}

func TestFormGuide(t *testing.T) {
	a := New().(loginsdk.FormGuider)
	g := a.FormGuide(onlineApp, true)
	if g == nil {
		t.Fatal("online app must have a guide")
	}
	svc := "https://api.oplist.org"
	wantHash := base64.StdEncoding.EncodeToString([]byte(`{"driver_txt":"onedrive_pr","server_use":"true"}`))
	if g.URL != svc+"/#"+wantHash {
		t.Fatalf("guide URL = %q", g.URL)
	}
	data, err := base64.StdEncoding.DecodeString(g.URL[strings.Index(g.URL, "#")+1:])
	if err != nil {
		t.Fatalf("hash is not base64: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil || payload["driver_txt"] != "onedrive_pr" {
		t.Fatalf("hash payload = %s (%v)", data, err)
	}
	if g.Label != "打开 OpenList 授权页" || len(g.Steps) != 3 {
		t.Fatalf("guide = %+v", g)
	}
	if !strings.Contains(strings.Join(g.Steps, "\n"), "OneDrive - 个人版本 (OAuth App)") {
		t.Fatalf("steps must mention the onedrive_pr option: %v", g.Steps)
	}
	if a.FormGuide(testApp, false) != nil {
		t.Fatal("own app must not get a guide")
	}
}

func TestAuthorizeURL(t *testing.T) {
	raw, err := New().(loginsdk.OAuthAdapter).AuthorizeURL(testApp, "state-42", testPKCE)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "login.microsoftonline.com" || u.Path != "/common/oauth2/v2.0/authorize" {
		t.Fatalf("endpoint = %s", raw)
	}
	want := url.Values{
		"client_id":             {testApp.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {testApp.RedirectURI},
		"response_mode":         {"query"},
		"scope":                 {"offline_access Files.ReadWrite.All"},
		"state":                 {"state-42"},
		"code_challenge":        {testPKCE.Challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Fatalf("authorize query = %v, want %v", u.Query(), want)
	}
}

func TestExchange(t *testing.T) {
	var gotForm url.Values
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/common/oauth2/v2.0/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			gotForm = r.PostForm
			fmt.Fprint(w, `{"token_type":"Bearer","scope":"offline_access Files.ReadWrite.All","expires_in":3600,"access_token":"ms-at","refresh_token":"ms-rt"}`)
		case "/v1.0/me/drive":
			if got := r.Header.Get("Authorization"); got != "Bearer ms-at" {
				t.Errorf("Authorization = %q", got)
			}
			fmt.Fprint(w, `{"owner":{"user":{"displayName":"张三","email":"zhang@example.com"}}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	auth, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "ms-code", testPKCE)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	wantForm := url.Values{
		"client_id":     {testApp.ClientID},
		"client_secret": {testApp.ClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {"ms-code"},
		"redirect_uri":  {testApp.RedirectURI},
		"code_verifier": {testPKCE.Verifier},
		"scope":         {"offline_access Files.ReadWrite.All"},
	}
	if !reflect.DeepEqual(gotForm, wantForm) {
		t.Fatalf("token form = %v, want %v", gotForm, wantForm)
	}
	wantFields := map[string]any{
		"refresh_token":  "ms-rt",
		"use_online_api": false,
		"client_id":      testApp.ClientID,
		"client_secret":  testApp.ClientSecret,
		"redirect_uri":   testApp.RedirectURI,
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if b, ok := auth.Fields["use_online_api"].(bool); !ok || b {
		t.Fatalf("use_online_api must be bool false, got %#v", auth.Fields["use_online_api"])
	}
	if !reflect.DeepEqual(auth.CreateFields, map[string]any{"region": "global", "is_sharepoint": false}) {
		t.Fatalf("CreateFields = %#v", auth.CreateFields)
	}
	if sp, ok := auth.CreateFields["is_sharepoint"].(bool); !ok || sp {
		t.Fatalf("is_sharepoint must be bool false, got %#v", auth.CreateFields["is_sharepoint"])
	}
	if auth.Account != "张三" {
		t.Fatalf("Account = %q", auth.Account)
	}
	if auth.Renewal == "" {
		t.Fatal("Renewal is empty")
	}
	if err := loginsdk.ValidateAuth(New(), testApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestExchangeAccountFallsBackToEmail(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/common/oauth2/v2.0/token":
			fmt.Fprint(w, `{"access_token":"at2","refresh_token":"rt2"}`)
		case "/v1.0/me/drive":
			fmt.Fprint(w, `{"owner":{"user":{"email":"only@example.com"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	auth, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "c2", testPKCE)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if auth.Account != "only@example.com" {
		t.Fatalf("Account = %q", auth.Account)
	}
}

func TestExchangeProviderError(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"AADSTS70000: code for client %s"}`, testApp.ClientSecret)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "c3", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), testApp.ClientSecret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}
	if !strings.Contains(err.Error(), "Microsoft 授权失败") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeMissingRefreshToken(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"only-at","token_type":"Bearer"}`)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "c4", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if !strings.Contains(err.Error(), "offline_access") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	oldClient := loginsdk.HTTPClient
	loginsdk.HTTPClient = srv.Client()
	oldToken := tokenURL
	tokenURL = srv.URL + "/token"
	t.Cleanup(func() {
		loginsdk.HTTPClient = oldClient
		tokenURL = oldToken
		srv.Close()
	})
	srv.Close()
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "c5", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeNetwork {
		t.Fatalf("want CodeNetwork, got %v", err)
	}
}

func TestCheckReauth(t *testing.T) {
	a := New().(loginsdk.ReauthChecker)
	ok := []map[string]any{
		{},
		{"region": ""},
		{"region": "global"},
		{"region": json.Number("global")},
		{"region": "global", "is_sharepoint": false},
		{"region": "global", "is_sharepoint": "false"},
		{"is_sharepoint": json.Number("0")},
	}
	for _, addition := range ok {
		if err := a.CheckReauth(addition); err != nil {
			t.Errorf("CheckReauth(%v) = %v, want nil", addition, err)
		}
	}
	bad := []map[string]any{
		{"region": "cn"},
		{"region": "us"},
		{"region": "de"},
		{"is_sharepoint": true},
		{"is_sharepoint": "true"},
		{"is_sharepoint": "TRUE"},
		{"is_sharepoint": json.Number("1")},
		{"region": "global", "is_sharepoint": true},
	}
	for _, addition := range bad {
		err := a.CheckReauth(addition)
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeUnsupported {
			t.Errorf("CheckReauth(%v) = %v, want CodeUnsupported", addition, err)
		}
	}
}
