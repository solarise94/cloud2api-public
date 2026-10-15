package loginsdk

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"
)

// randomToken returns n random bytes, base64url without padding.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("loginsdk: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewState returns a fresh OAuth state value (256 bits).
func NewState() string { return randomToken(32) }

// NewSessionID returns a fresh session id (128 bits, hex).
func NewSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("loginsdk: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewPKCE returns an RFC 7636 S256 verifier/challenge pair.
func NewPKCE() PKCE {
	verifier := randomToken(48) // 64 characters, within 43..128
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		Method:    "S256",
	}
}

// MaskID shortens an identifier for display: "abcd…wxyz" (or "****" when it is
// too short to show any of it).
func MaskID(s string) string {
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= 10 {
		return "****"
	}
	r := []rune(s)
	return string(r[:4]) + "…" + string(r[len(r)-4:])
}

// Scrub replaces every occurrence of each secret (ignoring values shorter than
// 4 characters, which would mangle ordinary text) with "***". Use it on every
// provider message, driver error and log line that might echo a credential.
func Scrub(msg string, secrets ...string) string {
	list := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) >= 4 {
			list = append(list, s)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, s := range list {
		msg = strings.ReplaceAll(msg, s, "***")
	}
	return msg
}

// Summary returns the browser-safe view of the credentials.
func (a *AuthConfig) Summary() *AuthSummary {
	if a == nil {
		return nil
	}
	identity := a.AppIdentity
	if a.AppMode == AppOwn {
		identity = MaskID(a.AppIdentity)
	}
	return &AuthSummary{
		CredentialType: a.CredentialType,
		AppMode:        a.AppMode,
		AppIdentity:    identity,
		Renewal:        a.Renewal,
		Account:        a.Account,
		Fields:         sortedKeys(a.Fields),
		CreateFields:   sortedKeys(a.CreateFields),
	}
}

// Wipe drops every reference to the credential values. (Go strings cannot be
// zeroed in place; dropping the references lets them be collected.)
func (a *AuthConfig) Wipe() {
	if a == nil {
		return
	}
	a.Fields = nil
	a.CreateFields = nil
	a.Secrets = nil
}

func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
