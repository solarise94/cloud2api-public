package loginsdk

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	// longest first: a secret containing another is replaced whole
	got := Scrub("pre supersecretvalue post secret end", "secret", "supersecretvalue")
	if got != "pre *** post *** end" {
		t.Fatalf("Scrub = %q", got)
	}
	// values shorter than 4 characters are ignored (they would mangle text)
	if got := Scrub("ab cd", "ab", "c"); got != "ab cd" {
		t.Fatalf("short secrets must be ignored, got %q", got)
	}
	if got := Scrub("", "secret"); got != "" {
		t.Fatalf("empty message = %q", got)
	}
}

func TestMaskID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"short", "****"},
		{"1234567890", "****"},
		{"client-id-1234567890", "clie…7890"},
	}
	for _, c := range cases {
		if got := MaskID(c.in); got != c.want {
			t.Errorf("MaskID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNewPKCEChallenge(t *testing.T) {
	p := NewPKCE()
	if p.Method != "S256" {
		t.Fatalf("method = %s", p.Method)
	}
	if n := len(p.Verifier); n < 43 || n > 128 {
		t.Fatalf("verifier length = %d, want 43..128", n)
	}
	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Fatalf("challenge = %s, want %s", p.Challenge, want)
	}
	if q := NewPKCE(); q.Verifier == p.Verifier {
		t.Fatal("two PKCE pairs share a verifier")
	}
}

func TestNewStateAndSessionID(t *testing.T) {
	s1, s2 := NewState(), NewState()
	if s1 == s2 || len(s1) < 32 {
		t.Fatalf("states not fresh: %q %q", s1, s2)
	}
	if strings.ContainsAny(s1, "+/=") {
		t.Fatalf("state must be base64url without padding: %q", s1)
	}
	id := NewSessionID()
	if len(id) != 32 || strings.ContainsAny(id, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("session id must be 32 hex chars: %q", id)
	}
}

func TestParseCallbackURL(t *testing.T) {
	p, err := ParseCallbackURL("https://app.example.com/cb?code=abc&state=xyz")
	if err != nil || p.Code != "abc" || p.State != "xyz" {
		t.Fatalf("query parse = %+v %v", p, err)
	}
	// fragment is read when the query has neither code nor error
	p, err = ParseCallbackURL("https://app.example.com/cb#code=abc&state=xyz")
	if err != nil || p.Code != "abc" || p.State != "xyz" {
		t.Fatalf("fragment parse = %+v %v", p, err)
	}
	// query wins over fragment
	p, err = ParseCallbackURL("https://app.example.com/cb?error=denied#code=abc")
	if err != nil || p.Error != "denied" || p.Code != "" {
		t.Fatalf("query precedence = %+v %v", p, err)
	}
	if _, err := ParseCallbackURL(""); err == nil {
		t.Fatal("empty URL must fail")
	} else {
		wantCode(t, err, CodeInvalidInput)
	}
	if _, err := ParseCallbackURL("not a url"); err == nil {
		t.Fatal("non-URL must fail")
	}
	if _, err := ParseCallbackURL("https://app.example.com/cb?state=xyz"); err == nil {
		t.Fatal("URL without a code must fail")
	}
	// spaces around a pasted URL are tolerated
	p, err = ParseCallbackURL("  https://app.example.com/cb?code=abc  ")
	if err != nil || p.Code != "abc" {
		t.Fatalf("trim parse = %+v %v", p, err)
	}
}
