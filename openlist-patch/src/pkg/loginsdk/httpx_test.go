package loginsdk

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBuildURL(t *testing.T) {
	if got := BuildURL("https://x.example.com/a", nil); got != "https://x.example.com/a" {
		t.Fatalf("no params: %s", got)
	}
	q := url.Values{"b": {"2"}, "a": {"1"}}
	if got := BuildURL("https://x.example.com/a", q); got != "https://x.example.com/a?a=1&b=2" {
		t.Fatalf("query: %s", got)
	}
	if got := BuildURL("https://x.example.com/a?z=9", q); got != "https://x.example.com/a?z=9&a=1&b=2" {
		t.Fatalf("existing query: %s", got)
	}
}

func TestParseCallbackQuery(t *testing.T) {
	p := ParseCallbackQuery(url.Values{
		"code": {"  abc  "}, "state": {" xyz "}, "error": {" e "}, "error_description": {" d "},
	})
	if p.Code != "abc" || p.State != "xyz" || p.Error != "e" || p.ErrorDescription != "d" {
		t.Fatalf("params = %+v", p)
	}
}

func TestParsePastedToken(t *testing.T) {
	// Obviously synthetic fixture: the "122." + 32-character body shape of a
	// Baidu-style refresh token, built from repeating zeroes with a FAKE
	// marker (ParsePastedToken only checks length and absence of whitespace).
	const token = "122.0000000000000000000000000000FAKE"
	cases := []struct {
		name string
		raw  string
	}{
		{"bare", token},
		{"bare_with_spaces", "  " + token + "\n"},
		{"json_object", `{"refresh_token":"` + token + `","other":"x"}`},
		{"callback_hash", "https://api.oplist.org/#" + base64.StdEncoding.EncodeToString(
			[]byte(`{"driver_txt":"baiduyun_go","refresh_token":"`+token+`"}`))},
	}
	for _, c := range cases {
		got, err := ParsePastedToken(c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != token {
			t.Errorf("%s: token = %q, want %q", c.name, got, token)
		}
	}

	bad := []struct {
		name string
		raw  string
	}{
		{"empty", "   "},
		{"inner_whitespace", "122.0000000000000000000000000000 FAKE"},
		{"too_short", "short-token-123"},
		{"too_long", token + strings.Repeat("x", 8192)},
		{"json_without_token", `{"refresh_token":""}`},
		{"json_broken", `{"refresh_token":`},
	}
	for _, c := range bad {
		if _, err := ParsePastedToken(c.raw); err == nil {
			t.Errorf("%s: accepted %q", c.name, c.raw)
		} else {
			wantCode(t, err, CodeInvalidInput)
		}
	}

	// A hash that is not base64(JSON) with a token is rejected outright.
	if _, err := ParsePastedToken("https://x.example/#not-a-token"); err == nil {
		t.Error("non-token hash accepted")
	}
}

func TestDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/echo" {
			body, _ := io.ReadAll(r.Body)
			w.WriteHeader(418)
			w.Write([]byte(r.Method + " " + string(body)))
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	status, body, err := Do(context.Background(), Request{URL: srv.URL + "/echo", Method: http.MethodPost, Form: url.Values{"x": {"1"}}})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != 418 || string(body) != "POST x=1" {
		t.Fatalf("status = %d body = %s", status, body)
	}
}

func TestDoNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // unreachable
	_, _, err := Do(context.Background(), Request{URL: srv.URL})
	if err == nil {
		t.Fatal("closed server must fail")
	}
	wantCode(t, err, CodeNetwork)
	if strings.Contains(err.Error(), "?") {
		t.Fatalf("network error must not echo the query: %v", err)
	}
}

func TestDoJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"t"}`))
	}))
	defer srv.Close()
	var out TokenResponse
	if _, _, err := DoJSON(context.Background(), Request{URL: srv.URL}, &out); err != nil {
		t.Fatalf("dojson: %v", err)
	}
	if out.AccessToken != "t" {
		t.Fatalf("token = %q", out.AccessToken)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer bad.Close()
	if _, _, err := DoJSON(context.Background(), Request{URL: bad.URL}, &out); err == nil {
		t.Fatal("non-json body must fail")
	} else {
		wantCode(t, err, CodeProvider)
	}
}
