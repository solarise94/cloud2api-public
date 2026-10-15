package example

import (
	"context"
	"reflect"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var noneApp = loginsdk.App{Platform: "example-openlist", Mode: loginsdk.AppNone}

func TestValidateMeta(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCheckApp(t *testing.T) {
	a := New()
	if err := a.CheckApp(noneApp); err != nil {
		t.Fatalf("mode none: %v", err)
	}
	err := a.CheckApp(loginsdk.App{Mode: loginsdk.AppOwn, ClientID: "c", ClientSecret: "s"})
	if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
		t.Fatalf("want CodeAppInvalid, got %v", err)
	}
}

func TestSubmitCreate(t *testing.T) {
	auth, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
		App: noneApp,
		Values: map[string]string{
			"url":      " http://127.0.0.1:5244/ ",
			"username": " admin ",
			"password": " secret-pw ",
		},
		Reauth: false,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !reflect.DeepEqual(auth.CreateFields, map[string]any{"url": "http://127.0.0.1:5244"}) {
		t.Fatalf("CreateFields = %#v", auth.CreateFields)
	}
	if !reflect.DeepEqual(auth.Fields, map[string]any{
		"username": "admin",
		"password": " secret-pw ", // password is used as-is
		"token":    "",
	}) {
		t.Fatalf("Fields = %#v", auth.Fields)
	}
	if !reflect.DeepEqual(auth.Secrets, []string{" secret-pw "}) {
		t.Fatalf("Secrets = %v", auth.Secrets)
	}
	if auth.Driver != "OpenList" || auth.CredentialType != "account_password" || auth.AppMode != loginsdk.AppNone {
		t.Fatalf("auth = %+v", auth)
	}
	if auth.Renewal == "" {
		t.Fatal("Renewal is empty")
	}
	if auth.Account != "admin" {
		t.Fatalf("Account = %q, want the trimmed username", auth.Account)
	}
	if err := loginsdk.ValidateAuth(New(), noneApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestSubmitReauthIgnoresURL(t *testing.T) {
	auth, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
		App:    noneApp,
		Values: map[string]string{"username": "admin", "password": "pw"},
		Reauth: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if auth.CreateFields != nil {
		t.Fatalf("CreateFields = %#v, want nil on reauth", auth.CreateFields)
	}
	if err := loginsdk.ValidateAuth(New(), noneApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestSubmitValidation(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		reauth bool
	}{
		{"missing url on create", map[string]string{"username": "u", "password": "p"}, false},
		{"bad url scheme", map[string]string{"url": "ftp://x", "username": "u", "password": "p"}, false},
		{"url with query", map[string]string{"url": "http://x/?a=1", "username": "u", "password": "p"}, false},
		{"missing username", map[string]string{"url": "http://x"}, false},
		{"missing password", map[string]string{"url": "http://x", "username": "u"}, false},
		{"missing username on reauth", map[string]string{"password": "p"}, true},
		{"missing password on reauth", map[string]string{"username": "u"}, true},
	}
	for _, c := range cases {
		_, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
			App: noneApp, Values: c.values, Reauth: c.reauth,
		})
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeInvalidInput {
			t.Errorf("%s: want CodeInvalidInput, got %v", c.name, err)
		}
	}
	// No url required on reauth even when absent.
	if _, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
		App: noneApp, Values: map[string]string{"username": "u", "password": "p"}, Reauth: true,
	}); err != nil {
		t.Errorf("reauth submit without url: %v", err)
	}
}
