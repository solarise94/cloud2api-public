package loginsdk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type stubAdapter struct {
	fakeAdapterBase
}

func TestValidateRules(t *testing.T) {
	good := oauthMeta("good", true, false)
	if err := Validate(&fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: good}}); err != nil {
		t.Fatalf("valid adapter rejected: %v", err)
	}

	cases := []struct {
		name string
		fix  func(*Meta)
		want string
	}{
		{"bad_id", func(m *Meta) { m.ID = "Bad ID" }, "id"},
		{"missing_name", func(m *Meta) { m.Name = "" }, "name"},
		{"missing_driver", func(m *Meta) { m.Driver = "" }, "driver"},
		{"none_combined", func(m *Meta) { m.AppModes = []AppMode{AppNone, AppOwn} }, "cannot be combined"},
		{"unknown_mode", func(m *Meta) { m.AppModes = []AppMode{"weird"} }, "unknown app mode"},
		{"no_app_modes", func(m *Meta) { m.AppModes = nil }, "at least one app mode"},
		{"bad_root_field", func(m *Meta) { m.RootField = "root" }, "root field"},
		{"reauth_equals_root", func(m *Meta) { m.ReauthFields = []string{m.RootField} }, "root field"},
		{"empty_reauth", func(m *Meta) { m.ReauthFields = nil }, "reauth fields"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := oauthMeta("someid", false, false)
			tc.fix(&meta)
			err := Validate(&fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: meta}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// method/interface mismatch: oauth meta on an adapter without Exchange
	err := Validate(&stubAdapter{fakeAdapterBase{meta: oauthMeta("someid", false, false)}})
	if err == nil || !strings.Contains(err.Error(), "OAuthAdapter") {
		t.Fatalf("err = %v", err)
	}
	// form without fields
	err = Validate(&fakeForm{fakeAdapterBase: fakeAdapterBase{meta: Meta{
		ID: "someid", Name: "n", Driver: "D", Method: MethodForm, AppModes: []AppMode{AppNone},
		RootField: "root_folder_path", RootDefault: "/", RootLabel: "r", ReauthFields: []string{"a"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "form fields") {
		t.Fatalf("err = %v", err)
	}
	// pkce is oauth-only
	qr := qrMeta("someid")
	qr.PKCE = true
	err = Validate(&fakeQR{fakeAdapterBase: fakeAdapterBase{meta: qr}, qrTemplate: &QRCode{}})
	if err == nil || !strings.Contains(err.Error(), "only apply to oauth") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	a := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("dup", false, false)}}
	if err := r.Register(a); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(a); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate register err = %v", err)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unknown adapter must not resolve")
	}
	if got := r.List(); len(got) != 1 || got[0].Meta().ID != "dup" {
		t.Fatalf("list = %v", got)
	}
}

// publicFake adapts the public-app mode: QR by default plus (optionally) the
// PublicAppProvider.
type publicFake struct {
	fakeAdapterBase
	withPublicApp bool
}

func (f *publicFake) StartQR(ctx context.Context, app App) (*QRCode, error) {
	return &QRCode{Content: "qr"}, nil
}

func (f *publicFake) PollQR(ctx context.Context, app App, qr *QRCode) (*QRPoll, error) {
	return &QRPoll{Status: QRWaiting}, nil
}

func (f *publicFake) PublicApp() App {
	if !f.withPublicApp {
		return App{}
	}
	return App{Mode: AppPublic, ClientID: "public-client-id-9876", ClientSecret: "public-client-secret"}
}

func TestValidateModeMethodRules(t *testing.T) {
	dual := func(def Method, modes []AppMode, overrides map[AppMode]Method) *dualFake {
		return &dualFake{fakeAdapterBase: fakeAdapterBase{meta: dualMeta("dual", def, overrides, modes...)}}
	}
	// oauth default + form override: the adapter serves both interfaces.
	if err := Validate(dual(MethodOAuth, []AppMode{AppOwn, AppOnlineService},
		map[AppMode]Method{AppOnlineService: MethodForm})); err != nil {
		t.Fatalf("valid dual adapter rejected: %v", err)
	}

	cases := []struct {
		name string
		a    Adapter
		want string
	}{
		{"override_of_unlisted_mode", dual(MethodForm, []AppMode{AppNone}, map[AppMode]Method{AppOwn: MethodOAuth}), "unlisted mode"},
		{"override_unknown_method", dual(MethodForm, []AppMode{AppNone}, map[AppMode]Method{AppNone: "magic"}), "unknown method"},
		{"oauth_without_interface", &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: func() Meta {
			m := oauthMeta("dual", false, false)
			m.AppModes = []AppMode{AppOwn, AppOnlineService}
			return m
		}()}}, "OAuthAdapter"},
		{"oauth_override_without_interface", &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: func() Meta {
			m := formMeta("dual", AppOnlineService, AppOwn)
			m.ModeMethods = map[AppMode]Method{AppOwn: MethodOAuth}
			return m
		}()}}, "OAuthAdapter"},
		{"unknown_default_method", dual("magic", []AppMode{AppNone}, nil), "unknown method"},
		{"form_without_fields", &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: Meta{
			ID: "nofields", Name: "n", Driver: "D", Method: MethodForm, AppModes: []AppMode{AppNone},
			RootField: "root_folder_path", RootDefault: "/", RootLabel: "r", ReauthFields: []string{"a"},
		}}}, "form fields"},
		{"public_without_provider", &fakeQR{fakeAdapterBase: fakeAdapterBase{meta: func() Meta {
			m := qrMeta("pubfake")
			m.AppModes = []AppMode{AppPublic}
			return m
		}()}, qrTemplate: &QRCode{}}, "PublicAppProvider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidatePublicAppRules(t *testing.T) {
	base := qrMeta("pubfake")
	base.AppModes = []AppMode{AppPublic}
	// PublicApp without credentials is rejected.
	a := &publicFake{fakeAdapterBase: fakeAdapterBase{meta: base}}
	if err := Validate(a); err == nil || !strings.Contains(err.Error(), "client id and secret") {
		t.Fatalf("err = %v, want a PublicApp complaint", err)
	}
	// With credentials (and the right mode) it validates.
	a.withPublicApp = true
	if err := Validate(a); err != nil {
		t.Fatalf("valid public adapter rejected: %v", err)
	}
}

func TestDefaultApp(t *testing.T) {
	// AppOwn has no default.
	own := &fakeOAuth{fakeAdapterBase: fakeAdapterBase{meta: oauthMeta("ownonly", false, false)}}
	if _, ok := DefaultApp(own); ok {
		t.Fatal("own-only adapter must not have a default app")
	}
	// AppNone needs no input.
	none := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("noneform")}}
	app, ok := DefaultApp(none)
	if !ok || app.Mode != AppNone || app.Platform != "noneform" {
		t.Fatalf("none default = %+v ok=%v", app, ok)
	}
	// AppOnlineService gets the default service URL.
	online := qrMeta("onlinefirst")
	online.AppModes = []AppMode{AppOnlineService, AppOwn}
	app, ok = DefaultApp(&fakeQR{fakeAdapterBase: fakeAdapterBase{meta: online}})
	if !ok || app.Mode != AppOnlineService || app.ServiceURL != DefaultServiceURL || app.Platform != "onlinefirst" {
		t.Fatalf("online default = %+v ok=%v", app, ok)
	}
	// AppPublic resolves to the adapter's published app.
	base := qrMeta("pubdef")
	base.AppModes = []AppMode{AppPublic, AppOwn}
	pub := &publicFake{fakeAdapterBase: fakeAdapterBase{meta: base}, withPublicApp: true}
	app, ok = DefaultApp(pub)
	if !ok || app.Mode != AppPublic || app.ClientID != "public-client-id-9876" ||
		app.ClientSecret != "public-client-secret" || app.Platform != "pubdef" {
		t.Fatalf("public default = %+v ok=%v", app, ok)
	}
	if id := app.Identity(); id != MaskID("public-client-id-9876") {
		t.Fatalf("public identity = %q", id)
	}
}

func TestResolveAppRules(t *testing.T) {
	base := qrMeta("pubres")
	base.AppModes = []AppMode{AppPublic, AppOwn}
	pub := &publicFake{fakeAdapterBase: fakeAdapterBase{meta: base}, withPublicApp: true}
	stamp := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	saved := App{Platform: "pubres", Mode: AppPublic, Revision: 7, UpdatedAt: stamp}
	resolved := ResolveApp(pub, saved)
	if resolved.ClientID != "public-client-id-9876" || resolved.ClientSecret != "public-client-secret" {
		t.Fatalf("resolved credentials = %+v", resolved)
	}
	if resolved.Platform != "pubres" || resolved.Mode != AppPublic || resolved.Revision != 7 || !resolved.UpdatedAt.Equal(stamp) {
		t.Fatalf("resolved profile fields lost: %+v", resolved)
	}
	// Non-public modes come back unchanged.
	ownApp := App{Platform: "pubres", Mode: AppOwn, ClientID: "cid", Revision: 2, UpdatedAt: stamp}
	if got := ResolveApp(pub, ownApp); got != ownApp {
		t.Fatalf("own app changed: %+v", got)
	}
}

func TestValidateAuthRules(t *testing.T) {
	adapter := &fakeForm{fakeAdapterBase: fakeAdapterBase{meta: formMeta("testform")}}
	app := App{Platform: "testform", Mode: AppNone}
	base := func() *AuthConfig {
		a := formAuthTemplate()
		a.AppMode = AppNone
		return a
	}

	if err := ValidateAuth(adapter, app, nil); err == nil {
		t.Fatal("nil auth must fail")
	}
	a := base()
	a.Driver = "Wrong"
	if err := ValidateAuth(adapter, app, a); err == nil || !strings.Contains(err.Error(), "驱动") {
		t.Fatalf("err = %v", err)
	}
	a = base()
	a.AppMode = AppOwn
	if err := ValidateAuth(adapter, app, a); err == nil || !strings.Contains(err.Error(), "应用模式") {
		t.Fatalf("err = %v", err)
	}
	a = base()
	a.Fields = nil
	if err := ValidateAuth(adapter, app, a); err == nil || !strings.Contains(err.Error(), "没有任何字段") {
		t.Fatalf("err = %v", err)
	}
	a = base()
	a.Fields["undeclared"] = "x"
	if err := ValidateAuth(adapter, app, a); err == nil || !strings.Contains(err.Error(), "未声明") {
		t.Fatalf("err = %v", err)
	}
	a = base()
	a.CreateFields = map[string]any{"password": "x"}
	if err := ValidateAuth(adapter, app, a); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("err = %v", err)
	}
	a = base()
	a.CreateFields = map[string]any{"region": "global"}
	if err := ValidateAuth(adapter, app, a); err != nil {
		t.Fatalf("valid auth rejected: %v", err)
	}
}

func TestCodeOf(t *testing.T) {
	if CodeOf(nil) != CodeInternal {
		t.Fatal("nil error must map to internal")
	}
	if CodeOf(errors.New("plain")) != CodeInternal {
		t.Fatal("plain error must map to internal")
	}
	if CodeOf(Wrap(CodeNetwork, "x", errors.New("y"))) != CodeNetwork {
		t.Fatal("wrapped code lost")
	}
	var e *Error
	if !errors.As(Errorf(CodeConflict, "m"), &e) || e.Message != "m" {
		t.Fatal("Errorf message lost")
	}
}
