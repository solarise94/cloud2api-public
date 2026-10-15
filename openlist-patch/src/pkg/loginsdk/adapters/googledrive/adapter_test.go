package googledrive

import (
	"context"
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
	Platform:     "googledrive",
	Mode:         loginsdk.AppOwn,
	ClientID:     "google-client-id-1234567890",
	ClientSecret: "google-client-secret-abcdef",
	RedirectURI:  "http://127.0.0.1:5244/api/connect/oauth/callback",
}

var testPKCE = loginsdk.PKCE{Verifier: "g-verifier-1234567890", Challenge: "g-challenge-1234567890", Method: "S256"}

var onlineApp = loginsdk.App{
	Platform:   "googledrive",
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
		{&tokenURL, srv.URL + "/token"},
		{&aboutURL, srv.URL + "/drive/v3/about"},
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
		{"fragment in redirect uri", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s", RedirectURI: "http://h/cb#f"}},
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
	const rt = "1//0gA1B2C3D4E5F6G7H8I9J0K1L2M3N4"
	auth, err := a.Submit(context.Background(), loginsdk.SubmitInput{
		App:    onlineApp,
		Values: map[string]string{"refresh_token": rt},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc := "https://api.oplist.org"
	wantFields := map[string]any{
		"refresh_token":   rt,
		"use_online_api":  true,
		"api_url_address": svc + "/googleui/renewapi",
		"client_id":       "",
		"client_secret":   "",
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if auth.CreateFields != nil {
		t.Fatalf("CreateFields = %#v, want nil", auth.CreateFields)
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

	if _, err := a.Submit(context.Background(), loginsdk.SubmitInput{App: onlineApp, Values: map[string]string{"refresh_token": "short"}}); err == nil || loginsdk.CodeOf(err) != loginsdk.CodeInvalidInput {
		t.Errorf("short paste: want CodeInvalidInput, got %v", err)
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
	// No callback hash: it would suppress the page's Google consent notice.
	if g.URL != "https://api.oplist.org/" {
		t.Fatalf("guide URL = %q, want the service root without a hash", g.URL)
	}
	if g.Label != "打开 OpenList 授权页" || len(g.Steps) != 3 {
		t.Fatalf("guide = %+v", g)
	}
	joined := strings.Join(g.Steps, "\n")
	for _, want := range []string{"Google Drive OAuth2 (OAuth App)", "使用 OpenList 提供的参数", "刷新令牌"} {
		if !strings.Contains(joined, want) {
			t.Errorf("steps must mention %q: %v", want, g.Steps)
		}
	}
	if a.FormGuide(testApp, false) != nil {
		t.Fatal("own app must not get a guide")
	}
}

func TestAuthorizeURL(t *testing.T) {
	raw, err := New().(loginsdk.OAuthAdapter).AuthorizeURL(testApp, "g-state", testPKCE)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("endpoint = %s", raw)
	}
	want := url.Values{
		"client_id":              {testApp.ClientID},
		"redirect_uri":           {testApp.RedirectURI},
		"response_type":          {"code"},
		"scope":                  {"https://www.googleapis.com/auth/drive"},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {"g-state"},
		"code_challenge":         {testPKCE.Challenge},
		"code_challenge_method":  {"S256"},
	}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Fatalf("authorize query = %v, want %v", u.Query(), want)
	}
}

func TestExchange(t *testing.T) {
	var gotForm url.Values
	var gotAboutQuery url.Values
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			gotForm = r.PostForm
			fmt.Fprint(w, `{"access_token":"g-at","expires_in":3599,"refresh_token":"g-rt","scope":"https://www.googleapis.com/auth/drive","token_type":"Bearer"}`)
		case "/drive/v3/about":
			gotAboutQuery = r.URL.Query()
			if got := r.Header.Get("Authorization"); got != "Bearer g-at" {
				t.Errorf("Authorization = %q", got)
			}
			fmt.Fprint(w, `{"user":{"emailAddress":"me@gmail.com","displayName":"Me"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	auth, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "g-code", testPKCE)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	wantForm := url.Values{
		"code":          {"g-code"},
		"client_id":     {testApp.ClientID},
		"client_secret": {testApp.ClientSecret},
		"redirect_uri":  {testApp.RedirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {testPKCE.Verifier},
	}
	if !reflect.DeepEqual(gotForm, wantForm) {
		t.Fatalf("token form = %v, want %v", gotForm, wantForm)
	}
	if !reflect.DeepEqual(gotAboutQuery, url.Values{"fields": {"user"}}) {
		t.Fatalf("about query = %v", gotAboutQuery)
	}
	wantFields := map[string]any{
		"refresh_token":  "g-rt",
		"use_online_api": false,
		"client_id":      testApp.ClientID,
		"client_secret":  testApp.ClientSecret,
	}
	if !reflect.DeepEqual(auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, wantFields)
	}
	if auth.CreateFields != nil {
		t.Fatalf("CreateFields = %#v, want nil", auth.CreateFields)
	}
	if auth.Account != "me@gmail.com" {
		t.Fatalf("Account = %q", auth.Account)
	}
	if err := loginsdk.ValidateAuth(New(), testApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestExchangeProviderError(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"bad code or secret %s"}`, testApp.ClientSecret)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "g-code-2", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), testApp.ClientSecret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}
	if !strings.Contains(err.Error(), "Google 授权失败") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeMissingRefreshToken(t *testing.T) {
	fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"g-at-2","token_type":"Bearer"}`)
	}))
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "g-code-3", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if !strings.Contains(err.Error(), "移除该应用的授权") {
		t.Fatalf("error message = %s", err)
	}
}

func TestExchangeRejectsPathLikeRefreshToken(t *testing.T) {
	// The GoogleDrive driver would read these as service-account files.
	for _, rt := range []string{"/etc/credentials/sa.json", "./sa.json"} {
		fakeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"access_token":"a","refresh_token":%q}`, rt)
		}))
		_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "g-code-4", testPKCE)
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
			t.Fatalf("refresh_token %q: want CodeProvider, got %v", rt, err)
		}
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
	_, err := New().(loginsdk.OAuthAdapter).Exchange(context.Background(), testApp, "g-code-5", testPKCE)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeNetwork {
		t.Fatalf("want CodeNetwork, got %v", err)
	}
}
