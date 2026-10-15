package aliyunopen

import (
	"context"
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

var ownApp = loginsdk.App{
	Platform:     "aliyun",
	Mode:         loginsdk.AppOwn,
	ClientID:     "cid-1234567890abcdef",
	ClientSecret: "cs-1234567890abcdef",
}

// setUp points apiBase and the HTTP client at fake servers. The returned
// service server is only used in online-service tests.
func setUp(t *testing.T, api http.HandlerFunc, service http.HandlerFunc) (apiSrv, serviceSrv *httptest.Server) {
	t.Helper()
	apiSrv = httptest.NewServer(http.HandlerFunc(api))
	oldClient := loginsdk.HTTPClient
	loginsdk.HTTPClient = apiSrv.Client()
	oldBase := apiBase
	apiBase = apiSrv.URL
	t.Cleanup(func() {
		loginsdk.HTTPClient = oldClient
		apiBase = oldBase
		if serviceSrv != nil {
			serviceSrv.Close()
		}
		apiSrv.Close()
	})
	if service != nil {
		serviceSrv = httptest.NewServer(http.HandlerFunc(service))
	}
	return
}

func onlineApp(t *testing.T, srv *httptest.Server) loginsdk.App {
	t.Helper()
	return loginsdk.App{
		Platform:   "aliyun",
		Mode:       loginsdk.AppOnlineService,
		ServiceURL: srv.URL + "/",
	}
}

func TestValidateMeta(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if ms := New().Meta().AppModes; ms[0] != loginsdk.AppOnlineService {
		t.Fatalf("default app mode = %s, want online_service", ms[0])
	}
}

func TestCheckApp(t *testing.T) {
	a := New()
	if err := a.CheckApp(ownApp); err != nil {
		t.Fatalf("own app: %v", err)
	}
	if err := a.CheckApp(onlineApp(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))); err != nil {
		t.Fatalf("online service: %v", err)
	}
	cases := []struct {
		name string
		app  loginsdk.App
	}{
		{"bad mode", loginsdk.App{Mode: loginsdk.AppNone}},
		{"missing service url", loginsdk.App{Mode: loginsdk.AppOnlineService}},
		{"service url with query", loginsdk.App{Mode: loginsdk.AppOnlineService, ServiceURL: "https://s.example.org/?x=1"}},
		{"relative service url", loginsdk.App{Mode: loginsdk.AppOnlineService, ServiceURL: "/svc"}},
		{"missing client id", loginsdk.App{Mode: loginsdk.AppOwn, ClientSecret: "s"}},
		{"missing client secret", loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c"}},
	}
	for _, c := range cases {
		err := a.CheckApp(c.app)
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
			t.Errorf("%s: want CodeAppInvalid, got %v", c.name, err)
		}
	}
}

func TestStartQROwnApp(t *testing.T) {
	var gotBody map[string]any
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/authorize/qrcode" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		fmt.Fprint(w, `{"qrCodeUrl":"https://openapi.alipan.com/oauth/qrcode/sid-1","sid":"sid-1"}`)
	}, nil)
	qr, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), ownApp)
	if err != nil {
		t.Fatalf("StartQR: %v", err)
	}
	want := map[string]any{
		"client_id":     ownApp.ClientID,
		"client_secret": ownApp.ClientSecret,
		"scopes":        []any{"user:base", "file:all:read", "file:all:write"},
		"width":         float64(300),
		"height":        float64(300),
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("request body = %#v, want %#v", gotBody, want)
	}
	if qr.ImageURL != "https://openapi.alipan.com/oauth/qrcode/sid-1" {
		t.Fatalf("ImageURL = %q", qr.ImageURL)
	}
	if qr.Private["sid"] != "sid-1" {
		t.Fatalf("Private = %v", qr.Private)
	}
	if d := time.Until(qr.ExpiresAt); d < 4*time.Minute || d > 6*time.Minute {
		t.Fatalf("ExpiresAt = %v", qr.ExpiresAt)
	}
	if qr.PollEvery != 2*time.Second {
		t.Fatalf("PollEvery = %v", qr.PollEvery)
	}
}

func TestStartQROwnAppError(t *testing.T) {
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"code":"InvalidParameter","message":"client_secret %s wrong"}`, ownApp.ClientSecret)
	}, nil)
	_, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), ownApp)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), ownApp.ClientSecret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}
}

func TestStartQROnlineService(t *testing.T) {
	var gotQuery url.Values
	_, svc := setUp(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alicloud/requests" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query()
		fmt.Fprint(w, `{"text":"https://openapi.alipan.com/oauth/qrcode/svc-sid","sid":"svc-sid"}`)
	})
	qr, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), onlineApp(t, svc))
	if err != nil {
		t.Fatalf("StartQR: %v", err)
	}
	if !reflect.DeepEqual(gotQuery, url.Values{"driver_txt": {"alicloud_qr"}, "server_use": {"true"}}) {
		t.Fatalf("requests query = %v", gotQuery)
	}
	if qr.Private["sid"] != "svc-sid" || qr.ImageURL == "" {
		t.Fatalf("qr = %+v", qr)
	}
}

func TestStartQROnlineServiceFailure(t *testing.T) {
	_, svc := setUp(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"text":"upstream busy"}`)
	})
	_, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), onlineApp(t, svc))
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if !strings.Contains(err.Error(), "upstream busy") {
		t.Fatalf("error message = %s", err)
	}
}

func TestStartQRBadImageURL(t *testing.T) {
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"qrCodeUrl":"http://insecure.example.com/qr.png","sid":"s"}`)
	}, nil)
	_, err := New().(loginsdk.QRAdapter).StartQR(context.Background(), ownApp)
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("http image URL must be rejected, got %v", err)
	}
}

func poll(t *testing.T, app loginsdk.App, sid, status string) *loginsdk.QRPoll {
	t.Helper()
	poll, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), app, &loginsdk.QRCode{
		Private: map[string]string{"sid": sid},
	})
	if err != nil {
		t.Fatalf("PollQR: %v", err)
	}
	return poll
}

func TestPollQRStatusMapping(t *testing.T) {
	for _, c := range []struct {
		status string
		want   loginsdk.QRStatus
	}{
		{"WaitLogin", loginsdk.QRWaiting},
		{"ScanSuccess", loginsdk.QRScanned},
		{"QRCodeExpired", loginsdk.QRExpired},
		{"SomethingNew", loginsdk.QRWaiting},
	} {
		setUp(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"status":%q,"authCode":"ac"}`, c.status)
		}, nil)
		p := poll(t, ownApp, "sid-x", c.status)
		if p.Status != c.want {
			t.Fatalf("status %q mapped to %s, want %s", c.status, p.Status, c.want)
		}
		if c.status == "SomethingNew" && p.Message != "SomethingNew" {
			t.Fatalf("unknown status message = %q, want raw status", p.Message)
		}
	}
}

func TestPollQRLoginSuccessOwnApp(t *testing.T) {
	var gotBody map[string]any
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/qrcode/sid-ok/status":
			fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"auth-code-1"}`)
		case "/oauth/access_token":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			fmt.Fprint(w, `{"access_token":"at-own","refresh_token":"rt-own","token_type":"Bearer"}`)
		case "/oauth/users/info":
			if got := r.Header.Get("Authorization"); got != "Bearer at-own" {
				t.Errorf("Authorization = %q", got)
			}
			fmt.Fprint(w, `{"nick_name":"阿里昵称","name":"fallback"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}, nil)
	p := poll(t, ownApp, "sid-ok", "LoginSuccess")
	if p.Status != loginsdk.QRConfirmed || p.Auth == nil {
		t.Fatalf("poll = %+v", p)
	}
	wantBody := map[string]any{
		"client_id":     ownApp.ClientID,
		"client_secret": ownApp.ClientSecret,
		"grant_type":    "authorization_code",
		"code":          "auth-code-1",
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Fatalf("access_token body = %#v, want %#v", gotBody, wantBody)
	}
	wantFields := map[string]any{
		"refresh_token":  "rt-own",
		"AccessToken":    "at-own",
		"use_online_api": false,
		"client_id":      ownApp.ClientID,
		"client_secret":  ownApp.ClientSecret,
		"alipan_type":    "default",
	}
	if !reflect.DeepEqual(p.Auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", p.Auth.Fields, wantFields)
	}
	if p.Auth.Account != "阿里昵称" {
		t.Fatalf("Account = %q", p.Auth.Account)
	}
	if p.Auth.AppIdentity != ownApp.ClientID || p.Auth.AppMode != loginsdk.AppOwn {
		t.Fatalf("identity = %q mode = %s", p.Auth.AppIdentity, p.Auth.AppMode)
	}
	if err := loginsdk.ValidateAuth(New(), ownApp, p.Auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestPollQRLoginSuccessOnlineService(t *testing.T) {
	var gotQuery url.Values
	var gotCookie string
	_, svc := setUp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/qrcode/svc-2/status":
			fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"ignored"}`)
		case "/oauth/users/info":
			fmt.Fprint(w, `{"name":"在线昵称"}`)
		default:
			t.Errorf("unexpected api request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alicloud/callback" {
			t.Errorf("unexpected service request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query()
		gotCookie = r.Header.Get("Cookie")
		fmt.Fprint(w, `{"access_token":"at-svc","refresh_token":"rt-svc","expires_in":1296000}`)
	})
	app := onlineApp(t, svc)
	p := poll(t, app, "svc-2", "LoginSuccess")
	if p.Status != loginsdk.QRConfirmed || p.Auth == nil {
		t.Fatalf("poll = %+v", p)
	}
	if !reflect.DeepEqual(gotQuery, url.Values{"code": {"svc-2"}}) {
		t.Fatalf("callback query = %v", gotQuery)
	}
	if gotCookie != "driver_txt=alicloud_qr; server_use=true" {
		t.Fatalf("callback Cookie = %q", gotCookie)
	}
	wantFields := map[string]any{
		"refresh_token":   "rt-svc",
		"AccessToken":     "at-svc",
		"use_online_api":  true,
		"api_url_address": strings.TrimRight(app.ServiceURL, "/") + "/alicloud/renewapi",
		"alipan_type":     "default",
		"client_id":       "",
		"client_secret":   "",
	}
	if !reflect.DeepEqual(p.Auth.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", p.Auth.Fields, wantFields)
	}
	if b, ok := p.Auth.Fields["use_online_api"].(bool); !ok || !b {
		t.Fatalf("use_online_api must be bool true, got %#v", p.Auth.Fields["use_online_api"])
	}
	if p.Auth.Account != "在线昵称" {
		t.Fatalf("Account = %q", p.Auth.Account)
	}
	if p.Auth.AppIdentity != strings.TrimRight(app.ServiceURL, "/") {
		t.Fatalf("AppIdentity = %q", p.Auth.AppIdentity)
	}
	if !strings.Contains(p.Auth.Renewal, "在线授权服务") || !strings.Contains(p.Auth.Renewal, strings.TrimRight(app.ServiceURL, "/")) {
		t.Fatalf("Renewal = %q", p.Auth.Renewal)
	}
	if err := loginsdk.ValidateAuth(New(), app, p.Auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestPollQROnlineServiceLoginFailedRace(t *testing.T) {
	// The status endpoint already said LoginSuccess but the service's own
	// re-check lags: 401 "Login failed:WaitLogin" must read as scanned.
	_, svc := setUp(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"x"}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"text":"Login failed:WaitLogin"}`)
	})
	p := poll(t, onlineApp(t, svc), "svc-3", "LoginSuccess")
	if p.Status != loginsdk.QRScanned {
		t.Fatalf("status = %s, want scanned", p.Status)
	}
	if p.Auth != nil {
		t.Fatalf("Auth must stay nil while waiting, got %+v", p.Auth)
	}
}

func TestPollQROnlineServiceOtherFailure(t *testing.T) {
	_, svc := setUp(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"x"}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"text":"token exchange failed rt-svc leaked"}`)
	})
	_, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), onlineApp(t, svc), &loginsdk.QRCode{
		Private: map[string]string{"sid": "svc-4"},
	})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
}

func TestPollQRNetworkError(t *testing.T) {
	apiSrv, _ := setUp(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"WaitLogin"}`)
	}, nil)
	apiSrv.Close()
	_, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), ownApp, &loginsdk.QRCode{
		Private: map[string]string{"sid": "sid-net"},
	})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeNetwork {
		t.Fatalf("want CodeNetwork, got %v", err)
	}
}

func TestPollQRMissingToken(t *testing.T) {
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/qrcode/sid-m/status":
			fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"ac-2"}`)
		case "/oauth/access_token":
			fmt.Fprint(w, `{"access_token":"only-at"}`)
		default:
			http.NotFound(w, r)
		}
	}, nil)
	_, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), ownApp, &loginsdk.QRCode{
		Private: map[string]string{"sid": "sid-m"},
	})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
}

func TestPollQRProviderErrorBody(t *testing.T) {
	setUp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/qrcode/sid-e/status":
			fmt.Fprint(w, `{"status":"LoginSuccess","authCode":"ac-3"}`)
		case "/oauth/access_token":
			fmt.Fprintf(w, `{"code":"InvalidClientSecret","message":"bad secret %s"}`, ownApp.ClientSecret)
		default:
			http.NotFound(w, r)
		}
	}, nil)
	_, err := New().(loginsdk.QRAdapter).PollQR(context.Background(), ownApp, &loginsdk.QRCode{
		Private: map[string]string{"sid": "sid-e"},
	})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeProvider {
		t.Fatalf("want CodeProvider, got %v", err)
	}
	if strings.Contains(err.Error(), ownApp.ClientSecret) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error not scrubbed: %s", err)
	}
}
