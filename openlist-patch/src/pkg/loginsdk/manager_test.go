package loginsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Distinctive secret values used by the fakes; the no-secret checks look for
// exactly these strings in marshaled views.
const (
	testClientSecret = "cs-CLIENT-SECRET-9f8e7d6c"
	testAuthCode     = "AC-authcode-4b8d2f77"
	testToken        = "tok-REFRESH-TOKEN-71abcd"
	testFormPassword = "pw-Form-Secret-889900"
	testOOBCode      = "oob-4242-authcode"
	testClientID     = "client-id-1234567890"
)

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeAdapterBase struct {
	meta        Meta
	checkAppErr error
}

func (f *fakeAdapterBase) Meta() Meta         { return f.meta }
func (f *fakeAdapterBase) CheckApp(App) error { return f.checkAppErr }

type fakeOAuth struct {
	fakeAdapterBase
	mu            sync.Mutex
	authorizeErr  error
	lastAuthState string
	lastAuthPKCE  PKCE
	exchangeCalls int
	lastCode      string
	lastExPKCE    PKCE
	exchangeErr   error
	auth          *AuthConfig
	lastAuth      *AuthConfig // the config instance actually staged by the manager
	entered       chan struct{}
	block         chan struct{}
}

func (f *fakeOAuth) AuthorizeURL(app App, state string, pkce PKCE) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authorizeErr != nil {
		return "", f.authorizeErr
	}
	f.lastAuthState, f.lastAuthPKCE = state, pkce
	return "https://provider.example.com/authorize?state=" + state, nil
}

func (f *fakeOAuth) Exchange(ctx context.Context, app App, code string, pkce PKCE) (*AuthConfig, error) {
	f.mu.Lock()
	f.exchangeCalls++
	f.lastCode, f.lastExPKCE = code, pkce
	entered, block, err, auth := f.entered, f.block, f.exchangeErr, f.auth
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	out := cloneAuth(auth)
	f.mu.Lock()
	f.lastAuth = out
	f.mu.Unlock()
	return out, nil
}

func (f *fakeOAuth) stagedAuth() *AuthConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuth
}

func (f *fakeOAuth) state() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuthState
}

func (f *fakeOAuth) authorizePKCE() PKCE {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAuthPKCE
}

func (f *fakeOAuth) exchanges() (calls int, code string, pkce PKCE) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchangeCalls, f.lastCode, f.lastExPKCE
}

type qrStep struct {
	poll *QRPoll
	err  error
}

type fakeQR struct {
	fakeAdapterBase
	mu         sync.Mutex
	startCalls int
	startErr   error
	qrTemplate *QRCode
	steps      []qrStep
	stepIdx    int
	pollCalls  int
	inPoll     int
	maxInPoll  int
}

func (f *fakeQR) StartQR(ctx context.Context, app App) (*QRCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return nil, f.startErr
	}
	qr := *f.qrTemplate
	return &qr, nil
}

func (f *fakeQR) PollQR(ctx context.Context, app App, qr *QRCode) (*QRPoll, error) {
	f.mu.Lock()
	f.pollCalls++
	f.inPoll++
	if f.inPoll > f.maxInPoll {
		f.maxInPoll = f.inPoll
	}
	f.mu.Unlock()
	time.Sleep(5 * time.Millisecond) // real time: overlap window for -race
	f.mu.Lock()
	i := f.stepIdx
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	step := f.steps[i]
	if f.stepIdx < len(f.steps)-1 {
		f.stepIdx++
	}
	f.inPoll--
	f.mu.Unlock()
	if step.poll != nil {
		p := *step.poll
		return &p, nil
	}
	return nil, step.err
}

func (f *fakeQR) stats() (starts, polls, maxInPoll int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCalls, f.pollCalls, f.maxInPoll
}

type fakeForm struct {
	fakeAdapterBase
	mu          sync.Mutex
	submitCalls int
	lastApp     App
	lastValues  map[string]string
	lastReauth  bool
	submitErr   error
	auth        *AuthConfig
	entered     chan struct{}
	block       chan struct{}
}

func (f *fakeForm) Submit(ctx context.Context, in SubmitInput) (*AuthConfig, error) {
	f.mu.Lock()
	f.submitCalls++
	f.lastApp = in.App
	f.lastValues = copyValues(in.Values)
	f.lastReauth = in.Reauth
	entered, block, err, auth := f.entered, f.block, f.submitErr, f.auth
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	return cloneAuth(auth), nil
}

func (f *fakeForm) lastSubmit() (App, map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastApp, f.lastValues, f.lastReauth
}

func cloneAuth(a *AuthConfig) *AuthConfig {
	if a == nil {
		return nil
	}
	b := *a
	b.Fields = copyAnyMap(a.Fields)
	b.CreateFields = copyAnyMap(a.CreateFields)
	b.Secrets = append([]string(nil), a.Secrets...)
	return &b
}

func copyAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyValues(vs map[string]string) map[string]string {
	out := make(map[string]string, len(vs))
	for k, v := range vs {
		out[k] = v
	}
	return out
}

func oauthMeta(id string, pkce, oob bool) Meta {
	return Meta{
		ID: id, Name: "测试平台" + id, Driver: "TestOAuthDriver", Method: MethodOAuth,
		AppModes: []AppMode{AppOwn}, Scopes: []string{"basic"}, PKCE: pkce, AllowOOB: oob,
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "根目录路径", RootHelp: "根目录",
		ReauthFields: []string{"refresh_token"},
	}
}

func qrMeta(id string) Meta {
	return Meta{
		ID: id, Name: "测试扫码" + id, Driver: "TestQRDriver", Method: MethodQR,
		AppModes:  []AppMode{AppNone},
		RootField: "root_folder_id", RootDefault: "0", RootLabel: "根目录ID",
		ReauthFields: []string{"access_token"},
	}
}

func formMeta(id string, modes ...AppMode) Meta {
	if len(modes) == 0 {
		modes = []AppMode{AppNone}
	}
	return Meta{
		ID: id, Name: "测试表单" + id, Driver: "TestFormDriver", Method: MethodForm,
		AppModes:   modes,
		FormFields: formTestFields(),
		RootField:  "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"username", "password"},
	}
}

func formTestFields() []Field {
	return []Field{
		{Key: "username", Label: "用户名", Type: "text", Required: true},
		{Key: "password", Label: "密码", Type: "password", Required: true, Secret: true},
		{Key: "server", Label: "服务器地址", Type: "url", CreateOnly: true},
	}
}

func oauthAuthTemplate() *AuthConfig {
	return &AuthConfig{
		Driver: "TestOAuthDriver", CredentialType: "oauth_refresh_token", AppMode: AppOwn,
		AppIdentity: testClientID, Renewal: "驱动使用自有应用直接续期", Account: "someone@example.com",
		Fields:  map[string]any{"refresh_token": testToken},
		Secrets: []string{testToken},
	}
}

func qrAuthTemplate() *AuthConfig {
	return &AuthConfig{
		Driver: "TestQRDriver", CredentialType: "qr_access_token", AppMode: AppNone,
		Renewal: "驱动使用扫码令牌续期",
		Fields:  map[string]any{"access_token": testToken},
		Secrets: []string{testToken},
	}
}

func formAuthTemplate() *AuthConfig {
	return &AuthConfig{
		Driver: "TestFormDriver", CredentialType: "account_password", AppMode: AppNone,
		Renewal: "驱动使用账号密码重新登录",
		Fields:  map[string]any{"username": "someone", "password": testFormPassword},
		Secrets: []string{testFormPassword},
	}
}

func oauthApp(platform string) App {
	return App{
		Platform: platform, Mode: AppOwn,
		ClientID: testClientID, ClientSecret: testClientSecret,
		RedirectURI: "https://app.example.com/cb",
	}
}

var testOwner = Owner{UserID: 1, Username: "admin", TokenFP: "fp-browser-1"}

func newReg(t *testing.T, as ...Adapter) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, a := range as {
		if err := r.Register(a); err != nil {
			t.Fatalf("register adapter: %v", err)
		}
	}
	return r
}

func testManager(reg *Registry, clock *fakeClock, mod func(*Config)) *Manager {
	cfg := Config{
		PendingTTL: 10 * time.Minute, AuthorizedTTL: 15 * time.Minute, ResultTTL: 30 * time.Minute,
		MaxPerOwner: 2, MaxSessions: 8, MinPollEvery: 2 * time.Second, MaxQRRefresh: 2,
		CallTimeout: 5 * time.Second, SaveTimeout: 5 * time.Second, Now: clock.Now,
	}
	if mod != nil {
		mod(&cfg)
	}
	return NewManager(reg, cfg)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got nil", code)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if e.Code != code {
		t.Fatalf("want error code %s, got %s (%s)", code, e.Code, e.Message)
	}
}

func startOAuthSession(t *testing.T, m *Manager, a *fakeOAuth, owner Owner) View {
	t.Helper()
	v, err := m.Start(context.Background(), StartInput{Owner: owner, Platform: a.meta.ID, App: oauthApp(a.meta.ID)})
	if err != nil {
		t.Fatalf("start oauth session: %v", err)
	}
	return v
}

// --- Start -------------------------------------------------------------

func TestStartValidation(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", true, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "nosuch"}); err == nil {
		t.Fatal("unknown platform must fail")
	} else {
		wantCode(t, err, CodeNotFound)
	}

	// app platform mismatch
	bad := oauthApp("testoauth")
	bad.Platform = "other"
	if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: bad}); err == nil {
		t.Fatal("platform mismatch must fail")
	} else {
		wantCode(t, err, CodeAppInvalid)
	}

	// unsupported app mode
	bad = oauthApp("testoauth")
	bad.Mode = AppOnlineService
	_, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: bad})
	wantCode(t, err, CodeAppInvalid)

	// CheckApp *Error is returned as-is
	a.checkAppErr = Errorf(CodeAppInvalid, "缺少 client_id")
	_, err = m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	wantCode(t, err, CodeAppInvalid)
	if !strings.Contains(err.Error(), "缺少 client_id") {
		t.Fatalf("CheckApp message lost: %v", err)
	}

	// CheckApp non-*Error is wrapped as app_invalid
	a.checkAppErr = errors.New("boom")
	_, err = m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	wantCode(t, err, CodeAppInvalid)
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("wrapped message lost: %v", err)
	}
}

func TestStartAppNoneIgnoresApp(t *testing.T) {
	clock := newFakeClock()
	f := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")}, auth: formAuthTemplate()}
	m := testManager(newReg(t, f), clock, nil)

	v, err := m.Start(context.Background(), StartInput{
		Owner:    testOwner,
		Platform: "testform",
		App:      App{Platform: "whatever", Mode: AppOwn, ClientSecret: "junk"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if v.App.Mode != AppNone {
		t.Fatalf("app mode = %s, want none", v.App.Mode)
	}
	if _, err := m.Submit(context.Background(), testOwner, v.ID, map[string]string{"username": "u", "password": "p"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	app, _, _ := f.lastSubmit()
	if app.Mode != AppNone || app.Platform != "testform" || app.ClientSecret != "" {
		t.Fatalf("adapter app = %+v, want the canonical none app", app)
	}
}

func TestStartPerOwnerLimit(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")}); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	_, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	wantCode(t, err, CodeLimit)
	if !strings.Contains(err.Error(), "进行中的登录会话过多") {
		t.Fatalf("unexpected limit message: %v", err)
	}

	// another owner is unaffected
	other := testOwner
	other.UserID, other.TokenFP = 7, "fp-7"
	if _, err := m.Start(ctx, StartInput{Owner: other, Platform: "testoauth", App: oauthApp("testoauth")}); err != nil {
		t.Fatalf("other owner start: %v", err)
	}
}

func TestStartOverLimitMakesNoProviderCall(t *testing.T) {
	clock := newFakeClock()
	o := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	qr := &fakeQR{fakeAdapterBase: fakeAdapterBase{meta: qrMeta("testqr")}, qrTemplate: &QRCode{Content: "qr-1"}}
	m := testManager(newReg(t, o, qr), clock, nil)
	for i := 0; i < 2; i++ {
		startOAuthSession(t, m, o, testOwner)
	}
	_, err := m.Start(context.Background(), StartInput{Owner: testOwner, Platform: "testqr"})
	wantCode(t, err, CodeLimit)
	qr.mu.Lock()
	calls := qr.startCalls
	qr.mu.Unlock()
	if calls != 0 {
		t.Fatalf("an over-limit start requested %d QR codes from the provider", calls)
	}
}

func TestStartMapFullDropsFinishedOldestFirst(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, func(c *Config) {
		c.MaxPerOwner, c.MaxSessions = 10, 2
	})
	ctx := context.Background()

	s1, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	if err != nil {
		t.Fatalf("start 1: %v", err)
	}
	if _, err := m.Cancel(testOwner, s1.ID); err != nil {
		t.Fatalf("cancel 1: %v", err)
	}
	if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")}); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	// map is full (cancelled s1 + live s2); s3 must evict the finished s1
	s3, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	if err != nil {
		t.Fatalf("start 3 should evict finished session: %v", err)
	}
	if _, err := m.Get(ctx, testOwner, s1.ID); err == nil {
		t.Fatal("evicted session must be gone")
	}
	// map full of live sessions: hard limit
	_, err = m.Start(ctx, StartInput{Owner: testOwner, Platform: "testoauth", App: oauthApp("testoauth")})
	wantCode(t, err, CodeLimit)
	if _, err := m.Get(ctx, testOwner, s3.ID); err != nil {
		t.Fatalf("s3 must still exist: %v", err)
	}
}

func TestStartOOB(t *testing.T) {
	clock := newFakeClock()
	plain := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("plain", false, false)}, auth: oauthAuthTemplate()}
	oob := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("oobplat", false, true)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, plain, oob), clock, nil)
	ctx := context.Background()

	app := oauthApp("plain")
	app.RedirectURI = "oob"
	_, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "plain", App: app})
	wantCode(t, err, CodeAppInvalid)

	app = oauthApp("oobplat")
	app.RedirectURI = "oob"
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "oobplat", App: app})
	if err != nil {
		t.Fatalf("start oob: %v", err)
	}
	if !v.OAuth.OOB || v.OAuth.RedirectURI != "oob" {
		t.Fatalf("oob view = %+v", v.OAuth)
	}
}

// --- OAuth callback and submit ------------------------------------------

func TestOAuthAuthorizeURLGetsStateAndPKCE(t *testing.T) {
	clock := newFakeClock()
	pkce := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("withpkce", true, false)}, auth: oauthAuthTemplate()}
	plain := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("nopkce", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, pkce, plain), clock, nil)
	ctx := context.Background()

	if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "withpkce", App: oauthApp("withpkce")}); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := pkce.authorizePKCE()
	if got.Verifier == "" || got.Challenge == "" || got.Method != "S256" {
		t.Fatalf("pkce adapter must receive a PKCE pair, got %+v", got)
	}
	if pkce.state() == "" {
		t.Fatal("adapter must receive the session state")
	}

	if _, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "nopkce", App: oauthApp("nopkce")}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := plain.authorizePKCE(); got != (PKCE{}) {
		t.Fatalf("non-pkce adapter must receive the zero PKCE, got %+v", got)
	}
}

func TestOAuthCallbackFlow(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", true, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	v := startOAuthSession(t, m, a, testOwner)
	state := a.state()

	// unknown state
	_, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {"nosuchstate"}})
	wantCode(t, err, CodeStateMismatch)

	// happy path
	out, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {state}})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if out.Phase != PhaseAuthorized || out.SessionID != v.ID || out.PlatformName != a.meta.Name {
		t.Fatalf("outcome = %+v", out)
	}
	if out.Message != "授权完成，请回到登录配置页面继续" {
		t.Fatalf("outcome message = %q", out.Message)
	}
	calls, code, exPKCE := a.exchanges()
	if calls != 1 || code != testAuthCode {
		t.Fatalf("exchange calls = %d code = %q", calls, code)
	}
	if exPKCE.Verifier != a.authorizePKCE().Verifier {
		t.Fatal("exchange must receive the same PKCE verifier")
	}

	gv, err := m.Get(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gv.Phase != PhaseAuthorized || gv.Auth == nil {
		t.Fatalf("view after callback = %+v", gv)
	}
	if len(gv.Auth.Fields) != 1 || gv.Auth.Fields[0] != "refresh_token" {
		t.Fatalf("auth summary fields = %v", gv.Auth.Fields)
	}
	if gv.Auth.AppIdentity != MaskID(testClientID) {
		t.Fatalf("auth identity = %q, want masked client id", gv.Auth.AppIdentity)
	}

	// the state is single-use even after success
	_, err = m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {state}})
	wantCode(t, err, CodeStateMismatch)
	if calls, _, _ := a.exchanges(); calls != 1 {
		t.Fatalf("exchange called %d times, want 1", calls)
	}
}

func TestCallbackUsedDuringExchange(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{
		fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)},
		auth:            oauthAuthTemplate(),
		entered:         make(chan struct{}, 1),
		block:           make(chan struct{}),
	}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	startOAuthSession(t, m, a, testOwner)
	state := a.state()

	done := make(chan CallbackOutcome, 1)
	go func() {
		out, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {state}})
		if err != nil {
			t.Errorf("callback: %v", err)
		}
		done <- out
	}()
	select {
	case <-a.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exchange did not start")
	}

	// while the exchange is in flight the state is used but still indexed
	_, err := m.Callback(ctx, url.Values{"code": {"x"}, "state": {state}})
	wantCode(t, err, CodeStateMismatch)
	if !strings.Contains(err.Error(), "已处理过") {
		t.Fatalf("unexpected message: %v", err)
	}

	close(a.block)
	out := <-done
	if out.Phase != PhaseAuthorized {
		t.Fatalf("outcome phase = %s", out.Phase)
	}
	if calls, _, _ := a.exchanges(); calls != 1 {
		t.Fatalf("exchange called %d times, want 1", calls)
	}
}

func TestOAuthCallbackDeniedAndFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("access_denied", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v := startOAuthSession(t, m, a, testOwner)
		out, err := m.Callback(ctx, url.Values{"error": {"access_denied"}, "state": {a.state()}})
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		if out.Phase != PhaseCancelled {
			t.Fatalf("phase = %s", out.Phase)
		}
		gv, _ := m.Get(ctx, testOwner, v.ID)
		if gv.Phase != PhaseCancelled || gv.ErrorCode != CodeCancelled {
			t.Fatalf("view = %s/%s", gv.Phase, gv.ErrorCode)
		}
	})

	t.Run("provider_error", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{
			fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)},
			auth:            oauthAuthTemplate(),
			exchangeErr:     Errorf(CodeProvider, "兑换失败 code=%s secret=%s", testAuthCode, testClientSecret),
		}
		m := testManager(newReg(t, a), clock, nil)
		v := startOAuthSession(t, m, a, testOwner)
		out, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {a.state()}})
		if err != nil {
			t.Fatalf("provider failure must still be a successful callback: %v", err)
		}
		if out.Phase != PhaseFailed {
			t.Fatalf("phase = %s", out.Phase)
		}
		gv, _ := m.Get(ctx, testOwner, v.ID)
		if gv.Phase != PhaseFailed || gv.ErrorCode != CodeProvider {
			t.Fatalf("view = %s/%s", gv.Phase, gv.ErrorCode)
		}
		data, _ := json.Marshal(gv)
		if strings.Contains(string(data), testAuthCode) || strings.Contains(string(data), testClientSecret) {
			t.Fatalf("secret leaked into view: %s", data)
		}
	})

	t.Run("provider_error_params", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v := startOAuthSession(t, m, a, testOwner)
		out, err := m.Callback(ctx, url.Values{
			"error": {"server_error"}, "error_description": {"数据库爆炸 " + testClientSecret}, "state": {a.state()},
		})
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		if out.Phase != PhaseFailed || !strings.Contains(out.Message, "server_error") {
			t.Fatalf("outcome = %+v", out)
		}
		if strings.Contains(out.Message, testClientSecret) {
			t.Fatalf("secret in message: %s", out.Message)
		}
		gv, _ := m.Get(ctx, testOwner, v.ID)
		if !strings.Contains(gv.Error, "授权失败：server_error") {
			t.Fatalf("view error = %q", gv.Error)
		}
	})
}

func TestOAuthSubmitCallbackURL(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", true, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v := startOAuthSession(t, m, a, testOwner)

	foreign := "https://app.example.com/cb?code=" + testAuthCode + "&state=foreign-state-value"
	_, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"callback_url": foreign})
	wantCode(t, err, CodeStateMismatch)
	gv, err := m.Get(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gv.Phase != PhasePending {
		t.Fatalf("session changed to %s on a foreign state", gv.Phase)
	}
	if calls, _, _ := a.exchanges(); calls != 0 {
		t.Fatal("foreign state must not reach Exchange")
	}

	ours := "https://app.example.com/cb?code=" + testAuthCode + "&state=" + url.QueryEscape(a.state())
	gv, err = m.Submit(ctx, testOwner, v.ID, map[string]string{"callback_url": ours})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if gv.Phase != PhaseAuthorized {
		t.Fatalf("phase = %s", gv.Phase)
	}
	if calls, code, _ := a.exchanges(); calls != 1 || code != testAuthCode {
		t.Fatalf("exchange calls = %d code = %q", calls, code)
	}
}

func TestOAuthSubmitOOBCode(t *testing.T) {
	clock := newFakeClock()
	plain := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("plain", false, false)}, auth: oauthAuthTemplate()}
	oob := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("oobplat", false, true)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, plain, oob), clock, nil)
	ctx := context.Background()

	// code submit only works for oob apps
	v := startOAuthSession(t, m, plain, testOwner)
	_, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"code": testOOBCode})
	wantCode(t, err, CodeInvalidInput)

	app := oauthApp("oobplat")
	app.RedirectURI = "oob"
	v2, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "oobplat", App: app})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	gv, err := m.Submit(ctx, testOwner, v2.ID, map[string]string{"code": testOOBCode})
	if err != nil {
		t.Fatalf("submit oob code: %v", err)
	}
	if gv.Phase != PhaseAuthorized {
		t.Fatalf("phase = %s", gv.Phase)
	}
	if calls, code, _ := oob.exchanges(); calls != 1 || code != testOOBCode {
		t.Fatalf("exchange calls = %d code = %q", calls, code)
	}

	// qrcode sessions take no submit at all
	qr := &fakeQR{fakeAdapterBase: fakeAdapterBase{meta: qrMeta("testqr")}, qrTemplate: &QRCode{Content: "qr-1"}}
	m2 := testManager(newReg(t, qr), clock, nil)
	qv, err := m2.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start qr: %v", err)
	}
	_, err = m2.Submit(ctx, testOwner, qv.ID, map[string]string{"code": "x"})
	wantCode(t, err, CodeInvalidInput)
}

// --- owner isolation ----------------------------------------------------

func TestOwnerIsolation(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v := startOAuthSession(t, m, a, testOwner)

	otherToken := testOwner
	otherToken.TokenFP = "fp-permanent-token"
	otherUser := testOwner
	otherUser.UserID = 2

	for name, who := range map[string]Owner{"other token": otherToken, "other user": otherUser} {
		if _, err := m.Get(ctx, who, v.ID); err == nil {
			t.Fatalf("%s: get must fail", name)
		} else {
			wantCode(t, err, CodeNotFound)
		}
		if _, err := m.Submit(ctx, who, v.ID, nil); err == nil {
			t.Fatalf("%s: submit must fail", name)
		}
		noFn := func(context.Context, SaveRequest) SaveResult {
			t.Errorf("%s: save fn must not run", name)
			return SaveResult{}
		}
		if _, err := m.Save(ctx, who, v.ID, noFn); err == nil {
			t.Fatalf("%s: save must fail", name)
		}
		if _, err := m.Cancel(who, v.ID); err == nil {
			t.Fatalf("%s: cancel must fail", name)
		}
	}

	// the real owner still sees the session
	if _, err := m.Get(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("owner get: %v", err)
	}
}

// --- QR ------------------------------------------------------------------

func qrTestAdapter(id string, steps ...qrStep) *fakeQR {
	return &fakeQR{
		fakeAdapterBase: fakeAdapterBase{meta: qrMeta(id)},
		qrTemplate:      &QRCode{Content: "qr-code-1", PollEvery: time.Second},
		steps:           steps,
	}
}

func TestQRPollFlow(t *testing.T) {
	clock := newFakeClock()
	a := qrTestAdapter("testqr",
		qrStep{poll: &QRPoll{Status: QRWaiting}},
		qrStep{poll: &QRPoll{Status: QRScanned, Message: "已扫码"}},
		qrStep{poll: &QRPoll{Status: QRConfirmed, Auth: qrAuthTemplate()}},
	)
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if v.QR.Status != QRWaiting || v.QR.Content != "qr-code-1" {
		t.Fatalf("initial qr = %+v", v.QR)
	}
	if _, polls, _ := a.stats(); polls != 0 {
		t.Fatal("start must not poll")
	}
	// before the interval elapses nothing polls
	if _, err := m.Get(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, polls, _ := a.stats(); polls != 0 {
		t.Fatalf("polled %d times before the interval", polls)
	}

	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRWaiting {
		t.Fatalf("status = %s", gv.QR.Status)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRScanned || gv.QR.Message != "已扫码" {
		t.Fatalf("status = %s msg = %q", gv.QR.Status, gv.QR.Message)
	}
	clock.Add(2 * time.Second)
	gv, err := m.Get(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gv.QR.Status != QRConfirmed {
		t.Fatalf("status = %s", gv.QR.Status)
	}
	if gv.Phase != PhaseAuthorized || gv.Auth == nil {
		t.Fatalf("phase = %s auth = %+v", gv.Phase, gv.Auth)
	}
	if gv.QR.CanRefresh {
		t.Fatal("an authorized session cannot refresh its qr")
	}
	if _, polls, _ := a.stats(); polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
}

func TestQRPollThrottledAndNeverConcurrent(t *testing.T) {
	clock := newFakeClock()
	a := qrTestAdapter("testqr", qrStep{poll: &QRPoll{Status: QRWaiting}})
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	hammer := func() {
		var wg sync.WaitGroup
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.Get(ctx, testOwner, v.ID); err != nil {
					t.Errorf("get: %v", err)
				}
			}()
		}
		wg.Wait()
	}

	clock.Add(2 * time.Second)
	hammer()
	if _, polls, maxIn := a.stats(); polls != 1 || maxIn != 1 {
		t.Fatalf("first hammer: polls = %d max concurrent = %d", polls, maxIn)
	}
	clock.Add(2 * time.Second)
	hammer()
	if _, polls, maxIn := a.stats(); polls != 2 || maxIn != 1 {
		t.Fatalf("second hammer: polls = %d max concurrent = %d", polls, maxIn)
	}
}

func TestQRNetworkErrorThenRecovery(t *testing.T) {
	clock := newFakeClock()
	a := qrTestAdapter("testqr",
		qrStep{err: Wrap(CodeNetwork, "无法连接 provider.example.com", errors.New("dial tcp: timeout"))},
		qrStep{poll: &QRPoll{Status: QRWaiting}},
		qrStep{poll: &QRPoll{Status: QRConfirmed, Auth: qrAuthTemplate(), Message: "令牌 " + testToken}},
	)
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	clock.Add(2 * time.Second)
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.QR.Status != QRNetworkError || gv.Phase != PhasePending {
		t.Fatalf("after network error: %s/%s", gv.QR.Status, gv.Phase)
	}
	if !strings.Contains(gv.QR.Message, "无法连接") {
		t.Fatalf("network message = %q", gv.QR.Message)
	}

	clock.Add(2 * time.Second)
	if gv, _ = m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRWaiting || gv.QR.Message != "" {
		t.Fatalf("recovery = %s %q, want waiting without the stale network message", gv.QR.Status, gv.QR.Message)
	}
	clock.Add(2 * time.Second)
	gv, err = m.Get(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gv.Phase != PhaseAuthorized {
		t.Fatalf("phase = %s", gv.Phase)
	}
	if strings.Contains(gv.QR.Message, testToken) {
		t.Fatalf("secret in confirm message: %q", gv.QR.Message)
	}
}

func TestQRExpiredAndRefreshLimit(t *testing.T) {
	clock := newFakeClock()
	a := qrTestAdapter("testqr", qrStep{poll: &QRPoll{Status: QRExpired}})
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	clock.Add(2 * time.Second)
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.QR.Status != QRExpired || gv.Phase != PhasePending {
		t.Fatalf("after expiry poll: %s/%s", gv.QR.Status, gv.Phase)
	}
	if !gv.QR.CanRefresh {
		t.Fatal("expired qr must allow refresh")
	}

	// expired code is not polled again
	clock.Add(2 * time.Second)
	m.Get(ctx, testOwner, v.ID)
	if _, polls, _ := a.stats(); polls != 1 {
		t.Fatalf("polls = %d, want 1 (expired code must not be polled again)", polls)
	}

	// refresh replaces the code; MaxQRRefresh is 2 in the test config
	a.mu.Lock()
	a.qrTemplate = &QRCode{Content: "qr-code-2", PollEvery: time.Second}
	a.mu.Unlock()
	gv, err = m.RefreshQR(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	if gv.QR.Status != QRWaiting || gv.QR.Content != "qr-code-2" {
		t.Fatalf("refreshed qr = %+v", gv.QR)
	}
	if _, err := m.RefreshQR(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	_, err = m.RefreshQR(ctx, testOwner, v.ID)
	wantCode(t, err, CodeLimit)
	if starts, _, _ := a.stats(); starts != 3 {
		t.Fatalf("start calls = %d, want 3", starts)
	}
}

func TestQRCancelled(t *testing.T) {
	clock := newFakeClock()
	a := qrTestAdapter("testqr", qrStep{poll: &QRPoll{Status: QRCancelled}})
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	clock.Add(2 * time.Second)
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.Phase != PhaseCancelled || gv.ErrorCode != CodeCancelled {
		t.Fatalf("view = %s/%s", gv.Phase, gv.ErrorCode)
	}
}

func TestQRPastExpiryWithoutPolling(t *testing.T) {
	clock := newFakeClock()
	a := &fakeQR{
		fakeAdapterBase: fakeAdapterBase{meta: qrMeta("testqr")},
		qrTemplate:      &QRCode{Content: "qr-code-1", ExpiresAt: clock.Now().Add(time.Minute)},
		steps:           []qrStep{{poll: &QRPoll{Status: QRWaiting}}},
	}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	clock.Add(2 * time.Minute) // qr expired, session (10m pending) still alive
	gv, err := m.Get(ctx, testOwner, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gv.QR.Status != QRExpired || gv.Phase != PhasePending || !gv.QR.CanRefresh {
		t.Fatalf("view = %s/%s can_refresh=%v", gv.QR.Status, gv.Phase, gv.QR.CanRefresh)
	}
	if _, polls, _ := a.stats(); polls != 0 {
		t.Fatalf("polled %d times on an expired code", polls)
	}
}

// --- form ----------------------------------------------------------------

func TestFormSubmitFlow(t *testing.T) {
	clock := newFakeClock()
	f := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")}, auth: formAuthTemplate()}
	m := testManager(newReg(t, f), clock, nil)
	ctx := context.Background()

	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testform"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(v.Form) != 3 {
		t.Fatalf("create form fields = %d, want 3", len(v.Form))
	}

	// a rejected submission keeps the session pending
	f.mu.Lock()
	f.submitErr = Errorf(CodeInvalidInput, "密码错误")
	f.mu.Unlock()
	_, err = m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": "wrong"})
	wantCode(t, err, CodeInvalidInput)
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.Phase != PhasePending {
		t.Fatalf("phase = %s after a rejected submit", gv.Phase)
	}

	f.mu.Lock()
	f.submitErr = nil
	f.mu.Unlock()
	values := map[string]string{"username": "someone", "password": testFormPassword, "server": "https://files.example.com"}
	gv, err = m.Submit(ctx, testOwner, v.ID, values)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if gv.Phase != PhaseAuthorized || gv.Auth == nil {
		t.Fatalf("view = %s auth=%+v", gv.Phase, gv.Auth)
	}
	app, gotValues, reauth := f.lastSubmit()
	if app.Mode != AppNone {
		t.Fatalf("app = %+v", app)
	}
	if reauth {
		t.Fatal("create submit must pass Reauth=false")
	}
	if gotValues["server"] != values["server"] || gotValues["password"] != testFormPassword {
		t.Fatalf("values = %v", gotValues)
	}

	// a second submit on an authorized session conflicts
	_, err = m.Submit(ctx, testOwner, v.ID, values)
	wantCode(t, err, CodeConflict)
}

func TestFormReauthHidesCreateOnly(t *testing.T) {
	clock := newFakeClock()
	f := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")}, auth: formAuthTemplate()}
	m := testManager(newReg(t, f), clock, nil)
	ctx := context.Background()

	target := &Target{StorageID: 12, MountPath: "/baidu", Driver: "TestFormDriver", Status: "work"}
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testform", Target: target})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if v.Mode != "reauth" || v.Target == nil || v.Target.StorageID != 12 {
		t.Fatalf("view mode/target = %s/%+v", v.Mode, v.Target)
	}
	for _, field := range v.Form {
		if field.CreateOnly {
			t.Fatalf("reauth form must hide create-only field %q", field.Key)
		}
	}
	if len(v.Form) != 2 {
		t.Fatalf("reauth form fields = %d, want 2", len(v.Form))
	}

	if _, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": testFormPassword}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, reauth := f.lastSubmit(); !reauth {
		t.Fatal("reauth submit must pass Reauth=true")
	}
}

func TestFormSubmitConflictWhileInFlight(t *testing.T) {
	clock := newFakeClock()
	f := &fakeForm{
		fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")},
		auth:            formAuthTemplate(),
		entered:         make(chan struct{}, 1),
		block:           make(chan struct{}),
	}
	m := testManager(newReg(t, f), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testform"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": "pw"})
		done <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("submit did not start")
	}
	_, err = m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": "pw"})
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), "正在处理上一次提交") {
		t.Fatalf("message: %v", err)
	}

	close(f.block)
	if err := <-done; err != nil {
		t.Fatalf("first submit: %v", err)
	}
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.Phase != PhaseAuthorized {
		t.Fatalf("phase = %s", gv.Phase)
	}
}

// --- ValidateAuth failures ------------------------------------------------

func TestValidateAuthFailuresInManager(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *AuthConfig
		want string
	}{
		{"wrong_driver", &AuthConfig{
			Driver: "OtherDriver", AppMode: AppNone, CredentialType: "account_password",
			Fields: map[string]any{"password": "x"},
		}, "驱动"},
		{"undeclared_field", &AuthConfig{
			Driver: "TestFormDriver", AppMode: AppNone, CredentialType: "account_password",
			Fields: map[string]any{"password": "x", "not_declared": "y"},
		}, "未声明"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			f := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")}, auth: tc.auth}
			m := testManager(newReg(t, f), clock, nil)
			ctx := context.Background()
			v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "testform"})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if _, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": "pw"}); err == nil {
				t.Fatal("submit must fail")
			}
			gv, _ := m.Get(ctx, testOwner, v.ID)
			if gv.Phase != PhaseFailed {
				t.Fatalf("phase = %s", gv.Phase)
			}
			if !strings.Contains(gv.Error, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", gv.Error, tc.want)
			}
		})
	}

	t.Run("app_mode_mismatch", func(t *testing.T) {
		clock := newFakeClock()
		f := &fakeForm{
			fakeAdapterBase: fakeAdapterBase{meta: formMeta("ownform", AppOwn)},
			auth: &AuthConfig{
				Driver: "TestFormDriver", AppMode: AppNone, CredentialType: "account_password",
				Fields: map[string]any{"username": "u"},
			},
		}
		m := testManager(newReg(t, f), clock, nil)
		ctx := context.Background()
		v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "ownform", App: oauthApp("ownform")})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := m.Submit(ctx, testOwner, v.ID, map[string]string{"username": "someone", "password": "pw"}); err == nil {
			t.Fatal("submit must fail")
		}
		gv, _ := m.Get(ctx, testOwner, v.ID)
		if gv.Phase != PhaseFailed || !strings.Contains(gv.Error, "应用模式") {
			t.Fatalf("view = %s %q", gv.Phase, gv.Error)
		}
	})
}

// --- save -----------------------------------------------------------------

func authorizedOAuth(t *testing.T, m *Manager, a *fakeOAuth) (View, *AuthConfig) {
	t.Helper()
	v := startOAuthSession(t, m, a, testOwner)
	if _, err := m.Callback(context.Background(), url.Values{"code": {testAuthCode}, "state": {a.state()}}); err != nil {
		t.Fatalf("callback: %v", err)
	}
	return v, a.stagedAuth()
}

func TestSaveRunsOnceForConcurrentCalls(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, _ := authorizedOAuth(t, m, a)

	var calls atomic.Int32
	var mu sync.Mutex
	var staged *AuthConfig
	var gotReq SaveRequest
	fn := func(ctx context.Context, req SaveRequest) SaveResult {
		calls.Add(1)
		mu.Lock()
		staged, gotReq = req.Auth, req
		mu.Unlock()
		time.Sleep(30 * time.Millisecond) // widen the overlap window for -race
		return SaveResult{OK: true, Mode: "create", StorageID: 13, MountPath: "/baidu", Status: "work", Message: "已创建存储 /baidu"}
	}

	var wg sync.WaitGroup
	views := make([]View, 8)
	for i := range views {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gv, err := m.Save(ctx, testOwner, v.ID, fn)
			if err != nil {
				t.Errorf("save %d: %v", i, err)
				return
			}
			views[i] = gv
		}(i)
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("fn called %d times, want exactly 1", calls.Load())
	}
	if gotReq.SessionID != v.ID || gotReq.Adapter == nil || gotReq.Target != nil {
		t.Fatalf("save request = %+v", gotReq)
	}
	wantApp := oauthApp("testoauth")
	if gotReq.App != wantApp {
		t.Fatalf("save request app = %+v, want the full app snapshot %+v", gotReq.App, wantApp)
	}
	mu.Lock()
	st := staged
	mu.Unlock()
	if st == nil {
		t.Fatal("fn never ran")
	}
	for i, gv := range views {
		if gv.Phase != PhaseSaved || gv.Result == nil || gv.Result.StorageID != 13 {
			t.Fatalf("view %d = %s %+v", i, gv.Phase, gv.Result)
		}
		if gv.Result.Mode != "create" {
			t.Fatalf("result mode = %q", gv.Result.Mode)
		}
	}

	// the staged AuthConfig the fake produced is wiped after the save
	if st.Fields != nil || st.Secrets != nil {
		t.Fatalf("credentials not wiped: %+v", st)
	}
	// idempotent: a later save returns the same result without running fn again
	again, err := m.Save(ctx, testOwner, v.ID, func(context.Context, SaveRequest) SaveResult {
		t.Error("fn must not run again")
		return SaveResult{}
	})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if again.Phase != PhaseSaved || again.Result.StorageID != 13 {
		t.Fatalf("second save view = %s %+v", again.Phase, again.Result)
	}
	// the auth summary survives the wipe
	if again.Auth == nil || len(again.Auth.Fields) != 1 {
		t.Fatalf("auth summary gone: %+v", again.Auth)
	}
}

func TestSaveRetryableKeepsCredentials(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, _ := authorizedOAuth(t, m, a)

	var seen []*AuthConfig
	gv, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
		seen = append(seen, req.Auth)
		return SaveResult{OK: false, Retryable: true, Message: "挂载路径已被占用"}
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if gv.Phase != PhaseAuthorized || gv.Result == nil || !gv.Result.Retryable {
		t.Fatalf("view = %s %+v", gv.Phase, gv.Result)
	}
	if len(seen[0].Fields) == 0 {
		t.Fatal("retryable save must keep the credentials")
	}

	// correct the input and save again
	gv, err = m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
		seen = append(seen, req.Auth)
		return SaveResult{OK: true, StorageID: 20, MountPath: "/baidu2", Message: "已创建"}
	})
	if err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if gv.Phase != PhaseSaved {
		t.Fatalf("phase = %s", gv.Phase)
	}
	if len(seen) != 2 || seen[0] != seen[1] {
		t.Fatal("the same staged config must be handed to the second save")
	}
	if seen[1].Fields != nil {
		t.Fatal("credentials must be wiped after the final save")
	}
	if gv.Result.Message != "已创建" {
		t.Fatalf("result message = %q", gv.Result.Message)
	}
}

func TestSavePanicAndSecretScrub(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, _ := authorizedOAuth(t, m, a)

	gv, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
		panic("写入失败，令牌为 " + testToken)
	})
	if err != nil {
		t.Fatalf("a panicking save must not escape: %v", err)
	}
	if gv.Phase != PhaseSaveFailed || gv.Result == nil {
		t.Fatalf("view = %s %+v", gv.Phase, gv.Result)
	}
	if gv.Result.Message != "保存时发生内部错误" {
		t.Fatalf("message = %q", gv.Result.Message)
	}
	if strings.Contains(gv.Result.Error, testToken) {
		t.Fatalf("panic text not scrubbed: %q", gv.Result.Error)
	}
	if gv.Auth == nil {
		t.Fatal("auth summary must survive")
	}
}

func TestSaveContextDetachedFromCaller(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	v, _ := authorizedOAuth(t, m, a)

	ctx, cancel := context.WithCancel(context.Background())
	var fnCancelled bool
	gv, err := m.Save(ctx, testOwner, v.ID, func(fctx context.Context, req SaveRequest) SaveResult {
		cancel()
		time.Sleep(50 * time.Millisecond)
		select {
		case <-fctx.Done():
			fnCancelled = true
		default:
		}
		return SaveResult{OK: true, StorageID: 9, Message: "ok"}
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if fnCancelled {
		t.Fatal("cancelling the caller context must not cancel the save")
	}
	if gv.Phase != PhaseSaved {
		t.Fatalf("phase = %s", gv.Phase)
	}
}

func TestSaveBeforeAuthorizedAndPhases(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	pending := startOAuthSession(t, m, a, testOwner)
	_, err := m.Save(ctx, testOwner, pending.ID, nil)
	wantCode(t, err, CodeInvalidInput)
	if !strings.Contains(err.Error(), "尚未完成授权") {
		t.Fatalf("message: %v", err)
	}

	cancelled := startOAuthSession(t, m, a, testOwner)
	if _, err := m.Cancel(testOwner, cancelled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err = m.Save(ctx, testOwner, cancelled.ID, nil)
	wantCode(t, err, CodeCancelled)

	failed := startOAuthSession(t, m, a, testOwner)
	a.mu.Lock()
	a.exchangeErr = Errorf(CodeProvider, "nope")
	state := a.lastAuthState
	a.mu.Unlock()
	if _, err := m.Callback(ctx, url.Values{"code": {"c"}, "state": {state}}); err != nil {
		t.Fatalf("callback: %v", err)
	}
	_, err = m.Save(ctx, testOwner, failed.ID, nil)
	wantCode(t, err, CodeConflict)
}

func TestCancelWhileSaving(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, _ := authorizedOAuth(t, m, a)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan View, 1)
	go func() {
		gv, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
			close(entered)
			<-release
			return SaveResult{OK: true, StorageID: 3, Message: "ok"}
		})
		if err != nil {
			t.Errorf("save: %v", err)
		}
		done <- gv
	}()
	<-entered

	_, err := m.Cancel(testOwner, v.ID)
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), "正在保存") {
		t.Fatalf("message: %v", err)
	}

	close(release)
	gv := <-done
	if gv.Phase != PhaseSaved {
		t.Fatalf("phase = %s", gv.Phase)
	}
}

func TestCancelPendingAndAuthorized(t *testing.T) {
	clock := newFakeClock()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	pending := startOAuthSession(t, m, a, testOwner)
	gv, err := m.Cancel(testOwner, pending.ID)
	if err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if gv.Phase != PhaseCancelled {
		t.Fatalf("phase = %s", gv.Phase)
	}
	// the state is gone: a late callback cannot complete it
	_, err = m.Callback(ctx, url.Values{"code": {"c"}, "state": {a.state()}})
	wantCode(t, err, CodeStateMismatch)
	// cancelling again returns the same view unchanged
	gv2, err := m.Cancel(testOwner, pending.ID)
	if err != nil || gv2.Phase != PhaseCancelled {
		t.Fatalf("re-cancel = %s %v", gv2.Phase, err)
	}
}

// --- expiry ----------------------------------------------------------------

func TestExpiry(t *testing.T) {
	ctx := context.Background()

	t.Run("pending_expires_and_state_dropped", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v := startOAuthSession(t, m, a, testOwner)
		state := a.state()

		clock.Add(10*time.Minute + time.Second)
		gv, err := m.Get(ctx, testOwner, v.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if gv.Phase != PhaseExpired || gv.ErrorCode != CodeExpired {
			t.Fatalf("view = %s/%s", gv.Phase, gv.ErrorCode)
		}
		_, err = m.Callback(ctx, url.Values{"code": {"c"}, "state": {state}})
		wantCode(t, err, CodeStateMismatch)
	})

	t.Run("authorized_expires_and_wipes", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v, auth := authorizedOAuth(t, m, a)

		clock.Add(15*time.Minute + time.Second)
		gv, err := m.Get(ctx, testOwner, v.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if gv.Phase != PhaseExpired {
			t.Fatalf("phase = %s", gv.Phase)
		}
		if auth.Fields != nil || auth.Secrets != nil {
			t.Fatalf("credentials not wiped on expiry: %+v", auth)
		}
		if gv.Auth == nil {
			t.Fatal("auth summary must stay readable")
		}
	})

	t.Run("saving_never_expires", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v, _ := authorizedOAuth(t, m, a)

		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan View, 1)
		go func() {
			gv, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
				close(entered)
				<-release
				return SaveResult{OK: true, StorageID: 5, Message: "ok"}
			})
			if err != nil {
				t.Errorf("save: %v", err)
			}
			done <- gv
		}()
		<-entered
		clock.Add(2 * time.Hour)
		gv, err := m.Get(ctx, testOwner, v.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if gv.Phase != PhaseSaving {
			t.Fatalf("a saving session must not expire, got %s", gv.Phase)
		}
		close(release)
		if gv := <-done; gv.Phase != PhaseSaved {
			t.Fatalf("save result phase = %s", gv.Phase)
		}
	})

	t.Run("finished_deleted_after_result_ttl", func(t *testing.T) {
		clock := newFakeClock()
		a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("testoauth", false, false)}, auth: oauthAuthTemplate()}
		m := testManager(newReg(t, a), clock, nil)
		v, _ := authorizedOAuth(t, m, a)
		if _, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
			return SaveResult{OK: true, Message: "ok"}
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
		clock.Add(30*time.Minute + time.Second)
		_, err := m.Get(ctx, testOwner, v.ID)
		wantCode(t, err, CodeNotFound)
	})
}

// --- per-mode method dispatch ----------------------------------------------

// dualFake implements OAuthAdapter and FormAdapter (and FormGuider) so one
// adapter can serve a different method per app mode.
type dualFake struct {
	fakeAdapterBase
	mu          sync.Mutex
	oauthStarts int
	formSubmits int
	guideReauth []bool
}

func (f *dualFake) AuthorizeURL(app App, state string, _ PKCE) (string, error) {
	f.mu.Lock()
	f.oauthStarts++
	f.mu.Unlock()
	return "https://dual.example.com/authorize?state=" + state, nil
}

func (f *dualFake) Exchange(ctx context.Context, app App, code string, _ PKCE) (*AuthConfig, error) {
	token := "dual-" + code
	return &AuthConfig{
		Driver: "DualDriver", CredentialType: "oauth_refresh_token", AppMode: app.Mode,
		AppIdentity: app.ClientID, Renewal: "r",
		Fields: map[string]any{"refresh_token": token}, Secrets: []string{token},
	}, nil
}

func (f *dualFake) Submit(_ context.Context, in SubmitInput) (*AuthConfig, error) {
	f.mu.Lock()
	f.formSubmits++
	f.mu.Unlock()
	token := "pasted-token-value-1234"
	return &AuthConfig{
		Driver: "DualDriver", CredentialType: "oauth_refresh_token", AppMode: in.App.Mode,
		AppIdentity: in.App.ServiceURL, Renewal: "r",
		Fields: map[string]any{"refresh_token": token}, Secrets: []string{token},
	}, nil
}

func (f *dualFake) FormGuide(app App, reauth bool) *Guide {
	f.mu.Lock()
	f.guideReauth = append(f.guideReauth, reauth)
	f.mu.Unlock()
	return &Guide{URL: "https://dual.example.com/token", Label: "打开授权页", Steps: []string{"第一步", "第二步"}}
}

func dualMeta(id string, def Method, overrides map[AppMode]Method, modes ...AppMode) Meta {
	return Meta{
		ID: id, Name: "双模式" + id, Driver: "DualDriver", Method: def,
		AppModes: modes, ModeMethods: overrides,
		FormFields: []Field{{Key: "refresh_token", Label: "刷新令牌", Type: "password", Required: true, Secret: true}},
		FormLabel:  "粘贴令牌",
		RootField:  "root_folder_path", RootDefault: "/", RootLabel: "根目录路径",
		ReauthFields: []string{"refresh_token"},
	}
}

func TestMethodForDispatch(t *testing.T) {
	clock := newFakeClock()
	// oauth default + form override.
	oauthDef := &dualFake{fakeAdapterBase: fakeAdapterBase{meta: dualMeta("dualoauth", MethodOAuth,
		map[AppMode]Method{AppOnlineService: MethodForm}, AppOwn, AppOnlineService)}}
	// form default + oauth override.
	formDef := &dualFake{fakeAdapterBase: fakeAdapterBase{meta: dualMeta("dualform", MethodForm,
		map[AppMode]Method{AppOwn: MethodOAuth}, AppOnlineService, AppOwn)}}
	m := testManager(newReg(t, oauthDef, formDef), clock, func(c *Config) { c.MaxPerOwner = 8 })
	ctx := context.Background()

	// Own app on the oauth-default adapter: an oauth session.
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "dualoauth", App: oauthApp("dualoauth")})
	if err != nil {
		t.Fatalf("start own: %v", err)
	}
	if v.Method != MethodOAuth || v.OAuth == nil || v.Form != nil || v.Guide != nil {
		t.Fatalf("own-app view = %s oauth=%v form=%d guide=%v", v.Method, v.OAuth, len(v.Form), v.Guide)
	}
	if _, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {stateOfDual(t, v)}}); err != nil {
		t.Fatalf("callback: %v", err)
	}
	gv, _ := m.Get(ctx, testOwner, v.ID)
	if gv.Phase != PhaseAuthorized {
		t.Fatalf("own-app phase = %s", gv.Phase)
	}

	// Online service on the oauth-default adapter: a form session with label
	// and guide.
	onl := App{Platform: "dualoauth", Mode: AppOnlineService, ServiceURL: "https://svc.example"}
	v2, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "dualoauth", App: onl})
	if err != nil {
		t.Fatalf("start online: %v", err)
	}
	if v2.Method != MethodForm || v2.Form == nil || v2.OAuth != nil {
		t.Fatalf("online view = %s form=%d oauth=%v", v2.Method, len(v2.Form), v2.OAuth)
	}
	if v2.FormLabel != "粘贴令牌" {
		t.Fatalf("form_label = %q", v2.FormLabel)
	}
	if v2.Guide == nil || v2.Guide.Label != "打开授权页" || len(v2.Guide.Steps) != 2 {
		t.Fatalf("guide = %+v", v2.Guide)
	}
	if _, err := m.Submit(ctx, testOwner, v2.ID, map[string]string{"refresh_token": "x"}); err != nil {
		t.Fatalf("submit form: %v", err)
	}

	// The same adapter the other way around: online default is a form
	// session, own app is an oauth session.
	onlForm := App{Platform: "dualform", Mode: AppOnlineService, ServiceURL: "https://svc.example"}
	v3, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "dualform", App: onlForm})
	if err != nil {
		t.Fatalf("start online default: %v", err)
	}
	if v3.Method != MethodForm || v3.Form == nil {
		t.Fatalf("form-default view = %s form=%d", v3.Method, len(v3.Form))
	}
	v4, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "dualform", App: oauthApp("dualform")})
	if err != nil {
		t.Fatalf("start own override: %v", err)
	}
	if v4.Method != MethodOAuth || v4.OAuth == nil {
		t.Fatalf("oauth-override view = %s oauth=%v", v4.Method, v4.OAuth)
	}

	// The guide sees the reauth flag.
	target := &Target{StorageID: 21, MountPath: "/dualform", Driver: "DualDriver"}
	v5, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "dualform", App: onlForm, Target: target})
	if err != nil {
		t.Fatalf("start reauth: %v", err)
	}
	if v5.Guide == nil {
		t.Fatal("reauth form session lost its guide")
	}
	formDef.mu.Lock()
	last := len(formDef.guideReauth) - 1
	gotReauth := formDef.guideReauth[last]
	formDef.mu.Unlock()
	if !gotReauth {
		t.Fatal("FormGuide must receive reauth=true for a re-authorization session")
	}
}

func stateOfDual(t *testing.T, v View) string {
	t.Helper()
	u, err := url.Parse(v.OAuth.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	return u.Query().Get("state")
}

func TestQRInitialMessage(t *testing.T) {
	clock := newFakeClock()
	hint := "初始提示：也可以输入代码 AB12"
	a := &fakeQR{
		fakeAdapterBase: fakeAdapterBase{meta: qrMeta("msgqr")},
		qrTemplate:      &QRCode{Content: "qr-msg", Message: hint, PollEvery: time.Second},
		steps: []qrStep{
			{poll: &QRPoll{Status: QRWaiting}}, // no message: the hint stays
			{err: Wrap(CodeNetwork, "无法连接 provider.example.com", errors.New("dial tcp: timeout"))},
			{poll: &QRPoll{Status: QRWaiting}},                 // recovered: the hint is back
			{poll: &QRPoll{Status: QRScanned, Message: "已扫码"}}, // replaces the hint
		},
	}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()

	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "msgqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if v.QR.Message != hint {
		t.Fatalf("initial qr message = %q, want the hint", v.QR.Message)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Message != hint {
		t.Fatalf("empty waiting poll must keep the hint, got %q", gv.QR.Message)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRNetworkError || !strings.Contains(gv.QR.Message, "无法连接") {
		t.Fatalf("network error view = %s %q", gv.QR.Status, gv.QR.Message)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRWaiting || gv.QR.Message != hint {
		t.Fatalf("after recovery = %s %q, want waiting with the hint", gv.QR.Status, gv.QR.Message)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Message != "已扫码" {
		t.Fatalf("message poll must replace the hint, got %q", gv.QR.Message)
	}
	// A refreshed code restarts from its own hint.
	if _, err := m.RefreshQR(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Message != hint {
		t.Fatalf("refreshed qr message = %q, want the hint again", gv.QR.Message)
	}
}

// An expired code's hint (a user code to type elsewhere) must not linger,
// whether the provider or the session clock expires it.
func TestQRExpiryClearsHint(t *testing.T) {
	clock := newFakeClock()
	hint := "也可以输入代码 AB12"
	a := &fakeQR{
		fakeAdapterBase: fakeAdapterBase{meta: qrMeta("msgqr")},
		qrTemplate:      &QRCode{Content: "qr-msg", Message: hint, PollEvery: time.Second},
		steps:           []qrStep{{poll: &QRPoll{Status: QRExpired}}},
	}
	m := testManager(newReg(t, a), clock, nil)
	ctx := context.Background()
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "msgqr"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	clock.Add(2 * time.Second)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRExpired || gv.QR.Message != "" {
		t.Fatalf("provider expiry = %s %q, want expired without the hint", gv.QR.Status, gv.QR.Message)
	}

	a.mu.Lock()
	a.qrTemplate = &QRCode{Content: "qr-msg-2", Message: hint, ExpiresAt: clock.Now().Add(time.Minute)}
	a.mu.Unlock()
	if _, err := m.RefreshQR(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	clock.Add(2 * time.Minute)
	if gv, _ := m.Get(ctx, testOwner, v.ID); gv.QR.Status != QRExpired || gv.QR.Message != "" {
		t.Fatalf("clock expiry = %s %q, want expired without the hint", gv.QR.Status, gv.QR.Message)
	}
}

// --- no secrets anywhere ----------------------------------------------------

func TestNoSecretsInViewsAndLogs(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var logs []string
	clock := newFakeClock()
	m := testManager(newReg(t), clock, func(c *Config) {
		c.MaxPerOwner = 10
		c.Logf = func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}
	})

	forbidden := []string{testClientSecret, testAuthCode, testToken, testFormPassword, testOOBCode}

	oauthA := &fakeOAuth{
		fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("secoauth", true, false)},
		auth:            oauthAuthTemplate(),
		exchangeErr:     nil,
	}
	if err := m.reg.Register(oauthA); err != nil {
		t.Fatal(err)
	}
	qrA := qrTestAdapter("secqr",
		qrStep{poll: &QRPoll{Status: QRWaiting}},
		qrStep{poll: &QRPoll{Status: QRScanned}},
		qrStep{poll: &QRPoll{Status: QRConfirmed, Auth: qrAuthTemplate(), Message: "拿到令牌 " + testToken}},
	)
	if err := m.reg.Register(qrA); err != nil {
		t.Fatal(err)
	}
	formA := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("secform")}, auth: formAuthTemplate()}
	if err := m.reg.Register(formA); err != nil {
		t.Fatal(err)
	}

	var views []View

	// oauth flow
	v, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "secoauth", App: oauthApp("secoauth")})
	if err != nil {
		t.Fatalf("start oauth: %v", err)
	}
	views = append(views, v)
	verifier := oauthA.authorizePKCE().Verifier
	forbidden = append(forbidden, verifier)
	if _, err := m.Callback(ctx, url.Values{"code": {testAuthCode}, "state": {oauthA.state()}}); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if gv, err := m.Get(ctx, testOwner, v.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else {
		views = append(views, gv)
	}
	if gv, err := m.Save(ctx, testOwner, v.ID, func(ctx context.Context, req SaveRequest) SaveResult {
		return SaveResult{OK: true, StorageID: 1, Message: "已保存"}
	}); err != nil {
		t.Fatalf("save: %v", err)
	} else {
		views = append(views, gv)
	}

	// qr flow
	qv, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "secqr"})
	if err != nil {
		t.Fatalf("start qr: %v", err)
	}
	views = append(views, qv)
	for i := 0; i < 3; i++ {
		clock.Add(2 * time.Second)
		gv, err := m.Get(ctx, testOwner, qv.ID)
		if err != nil {
			t.Fatalf("qr get: %v", err)
		}
		views = append(views, gv)
	}

	// form flow
	fv, err := m.Start(ctx, StartInput{Owner: testOwner, Platform: "secform"})
	if err != nil {
		t.Fatalf("start form: %v", err)
	}
	views = append(views, fv)
	if gv, err := m.Submit(ctx, testOwner, fv.ID, map[string]string{
		"username": "someone", "password": testFormPassword, "server": "https://s.example.com",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	} else {
		views = append(views, gv)
	}

	for i, vv := range views {
		data, err := json.Marshal(vv)
		if err != nil {
			t.Fatalf("marshal view %d: %v", i, err)
		}
		for _, secret := range forbidden {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatalf("view %d leaks %q: %s", i, secret, data)
			}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("lifecycle logs missing")
	}
	for _, line := range logs {
		for _, secret := range forbidden {
			if secret != "" && strings.Contains(line, secret) {
				t.Fatalf("log line leaks a secret: %s", line)
			}
		}
		for _, id := range []string{v.ID, qv.ID, fv.ID} {
			if strings.Contains(line, id) {
				t.Fatalf("log line carries the full session id: %s", line)
			}
		}
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, v.ID[:8]) {
		t.Fatalf("logs must carry the truncated session id:\n%s", joined)
	}
	for _, event := range []string{"started", "authorized", "saved"} {
		if !strings.Contains(joined, event) {
			t.Fatalf("missing %q in logs:\n%s", event, joined)
		}
	}
}
