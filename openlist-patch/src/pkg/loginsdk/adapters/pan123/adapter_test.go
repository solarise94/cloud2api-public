package pan123

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var noneApp = loginsdk.App{Platform: "123pan", Mode: loginsdk.AppNone}

func TestValidateMeta(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := New().Meta()
	if len(m.FormFields) != 2 || m.FormFields[0].Key != "username" || m.FormFields[1].Key != "password" {
		t.Fatalf("form fields = %+v", m.FormFields)
	}
	if !m.FormFields[1].Secret {
		t.Fatalf("password field must be secret")
	}
}

func TestCheckApp(t *testing.T) {
	a := New()
	if err := a.CheckApp(noneApp); err != nil {
		t.Fatalf("mode none: %v", err)
	}
	for _, mode := range []loginsdk.AppMode{loginsdk.AppOwn, loginsdk.AppOnlineService} {
		err := a.CheckApp(loginsdk.App{Mode: mode, ClientID: "c", ClientSecret: "s"})
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeAppInvalid {
			t.Errorf("mode %s: want CodeAppInvalid, got %v", mode, err)
		}
	}
}

func TestSubmit(t *testing.T) {
	auth, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
		App:    noneApp,
		Values: map[string]string{"username": " 13800135789 ", "password": "hunter2"},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	want := map[string]any{
		"username":    "13800135789",
		"password":    "hunter2",
		"AccessToken": "",
	}
	if !reflect.DeepEqual(auth.Fields, want) {
		t.Fatalf("Fields = %#v, want %#v", auth.Fields, want)
	}
	if auth.Driver != "123Pan" || auth.CredentialType != "account_password" || auth.AppMode != loginsdk.AppNone {
		t.Fatalf("auth = %+v", auth)
	}
	if auth.Account != "138******89" {
		t.Fatalf("Account = %q", auth.Account)
	}
	if !reflect.DeepEqual(auth.Secrets, []string{"hunter2"}) {
		t.Fatalf("Secrets = %v", auth.Secrets)
	}
	if err := loginsdk.ValidateAuth(New(), noneApp, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestSubmitValidation(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"missing username", map[string]string{"password": "p"}, "请填写手机号或邮箱"},
		{"blank username", map[string]string{"username": "   ", "password": "p"}, "请填写手机号或邮箱"},
		{"missing password", map[string]string{"username": "13800135789"}, "请填写密码"},
		{"long username", map[string]string{"username": strings.Repeat("a", 257), "password": "p"}, "手机号或邮箱过长"},
		{"long password", map[string]string{"username": "u", "password": strings.Repeat("a", 257)}, "密码过长"},
	}
	for _, c := range cases {
		_, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
			App: noneApp, Values: c.values,
		})
		if err == nil || loginsdk.CodeOf(err) != loginsdk.CodeInvalidInput {
			t.Errorf("%s: want CodeInvalidInput, got %v", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: message %q lacks %q", c.name, err, c.want)
		}
	}
	if _, err := New().(loginsdk.FormAdapter).Submit(context.Background(), loginsdk.SubmitInput{
		App:    noneApp,
		Values: map[string]string{"username": strings.Repeat("中", 256), "password": "p"},
	}); err != nil {
		t.Errorf("256-rune username should pass: %v", err)
	}
}

func TestMaskUsername(t *testing.T) {
	cases := []struct{ in, want string }{
		{"13800135789", "138******89"},
		{"abcdef@example.com", "abc***@example.com"},
		{"ab@example.com", "**@example.com"},
		{"abc", "***"},
		{"12345", "*****"},
		{"123456", "123*56"},
	}
	for _, c := range cases {
		if got := maskUsername(c.in); got != c.want {
			t.Errorf("maskUsername(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
