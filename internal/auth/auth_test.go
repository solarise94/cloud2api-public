package auth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHashKeyKnownVector(t *testing.T) {
	// sha256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
	want := "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashKey("abc"); got != want {
		t.Errorf("HashKey(abc) = %q, want %q", got, want)
	}
	if HashKey("abc") != HashKey("abc") {
		t.Error("HashKey is not deterministic")
	}
	if HashKey("abc") == HashKey("abd") {
		t.Error("HashKey collision on distinct inputs")
	}
	if err := ValidateHash(HashKey("x")); err != nil {
		t.Errorf("ValidateHash rejected a good digest: %v", err)
	}
	for _, bad := range []string{"", "sha256:zz", "md5:abc", strings.Repeat("a", 71)} {
		if err := ValidateHash(bad); err == nil {
			t.Errorf("ValidateHash accepted %q", bad)
		}
	}
}

func TestHashKeyGenkeyFormat(t *testing.T) {
	// Legacy keys produced by `cloud2api genkey` are "sk-" + base64url
	// chars; their hashes must validate.
	plain := "sk-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM0123456789-_"
	h := HashKey(plain)
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Errorf("hash %q has wrong shape", h)
	}
	if strings.ToLower(h) != h {
		t.Errorf("hash %q must be lowercase hex", h)
	}
	if _, err := json.Marshal(h); err != nil {
		t.Errorf("hash is not JSON-safe: %v", err)
	}
}

func TestSplitNewFormat(t *testing.T) {
	if pub, secret, ok := SplitNewFormat("sk-c2a-ab12cd34-S3cr3t"); !ok || pub != "ab12cd34" || secret != "S3cr3t" {
		t.Errorf("SplitNewFormat = %q %q %v", pub, secret, ok)
	}
	for _, bad := range []string{
		"", "sk-", "sk-c2a-", "sk-c2a-short-x", "sk-c2a-toolongid12-x", "sk-plainlegacy",
	} {
		if _, _, ok := SplitNewFormat(bad); ok {
			t.Errorf("SplitNewFormat(%q) accepted", bad)
		}
	}
}

func TestMapLegacyPermission(t *testing.T) {
	if got := MapLegacyPermission("write"); len(got) != 2 || got[0] != PermWrite || got[1] != PermDelete {
		t.Errorf("write -> %v, want [write delete]", got)
	}
	if got := MapLegacyPermission("transfer"); len(got) != 1 || got[0] != PermTransfer {
		t.Errorf("transfer -> %v", got)
	}
	if got := MapLegacyPermission("purge"); len(got) != 1 || got[0] != PermPurge {
		t.Errorf("new vocab passthrough failed: %v", got)
	}
	if got := MapLegacyPermission("exec"); got != nil {
		t.Errorf("unknown -> %v, want nil", got)
	}
}

func mustKey(id, plain string, mut ...func(*Key)) *Key {
	k := &Key{
		ID:          id,
		GlobalPerms: map[Permission]bool{PermList: true},
		Drives: []KeyDrive{{
			Drive: "baidu-main", Root: "/",
			Perms: map[Permission]bool{PermList: true, PermRead: true},
		}},
	}
	k.SetHash(HashKey(plain))
	for _, m := range mut {
		m(k)
	}
	return k
}

func TestAuthenticateLegacyKey(t *testing.T) {
	a := NewAuthenticator()
	a.Reload([]*Key{mustKey("histopilot", "sk-legacy-key")})
	k, out := a.Authenticate("sk-legacy-key")
	if out != AuthOK || k == nil || k.ID != "histopilot" {
		t.Fatalf("legacy authenticate = %v %v", k, out)
	}
	if _, out := a.Authenticate("sk-wrong"); out != AuthUnknownKey {
		t.Errorf("wrong key outcome = %v", out)
	}
}

func TestAuthenticateNewFormatKey(t *testing.T) {
	plain := "sk-c2a-ab12cd34-SECRETSECRETSECRETSECRET00"
	k := mustKey("agent1", plain)
	k.PublicID = "ab12cd34"
	a := NewAuthenticator()
	a.Reload([]*Key{k})

	got, out := a.Authenticate(plain)
	if out != AuthOK || got == nil || got.ID != "agent1" {
		t.Fatalf("new-format authenticate = %v %v", got, out)
	}
	// Wrong secret with the same public id must fail.
	if _, out := a.Authenticate("sk-c2a-ab12cd34-WRONGWRONGWRONGWRONGWRONGWRONG"); out == AuthOK {
		t.Error("wrong secret authenticated")
	}
	// Unknown public id falls through to the legacy scan and fails.
	if _, out := a.Authenticate("sk-c2a-zzzzzzzz-SECRETSECRETSECRETSECRET00"); out == AuthOK {
		t.Error("unknown public id authenticated")
	}
}

func TestAuthenticateRevokedAndExpired(t *testing.T) {
	revoked := mustKey("revoked", "sk-revoked", func(k *Key) { k.Revoked = true })
	past := time.Now().Add(-time.Minute)
	expired := mustKey("expired", "sk-expired", func(k *Key) { t := past; k.ExpiresAt = &t })
	future := time.Now().Add(time.Hour)
	valid := mustKey("valid", "sk-valid", func(k *Key) { t := future; k.ExpiresAt = &t })
	a := NewAuthenticator()
	a.Reload([]*Key{revoked, expired, valid})

	if _, out := a.Authenticate("sk-revoked"); out != AuthRevoked {
		t.Errorf("revoked outcome = %v", out)
	}
	if _, out := a.Authenticate("sk-expired"); out != AuthExpired {
		t.Errorf("expired outcome = %v", out)
	}
	if _, out := a.Authenticate("sk-valid"); out != AuthOK {
		t.Errorf("unexpired outcome = %v", out)
	}
}

func TestEffectiveDefaultDrive(t *testing.T) {
	k := mustKey("k", "sk-x")
	if got := k.EffectiveDefaultDrive(); got != "baidu-main" {
		t.Errorf("implicit default = %q, want first granted drive", got)
	}
	k.DefaultDrive = "other"
	if got := k.EffectiveDefaultDrive(); got != "other" {
		t.Errorf("explicit default = %q", got)
	}
	empty := &Key{GlobalPerms: map[Permission]bool{}}
	empty.SetHash(HashKey("sk-e"))
	if got := empty.EffectiveDefaultDrive(); got != "" {
		t.Errorf("no drives default = %q", got)
	}
}

func TestCanOn(t *testing.T) {
	k := mustKey("k", "sk-x")
	k.GlobalPerms[PermTransfer] = true
	if !k.CanOn("baidu-main", PermList) {
		t.Error("per-drive list missing")
	}
	if !k.CanOn("baidu-main", PermTransfer) {
		t.Error("global transfer missing")
	}
	if k.CanOn("baidu-main", PermWrite) {
		t.Error("write granted without basis")
	}
	if k.CanOn("ghost", PermRead) {
		t.Error("unknown drive granted a per-drive permission")
	}
	if !k.CanOn("ghost", PermList) {
		t.Error("global list permission must apply on any drive reference")
	}
	if !k.HasAccount("baidu-main") || k.HasAccount("ghost") {
		t.Error("HasAccount misbehaves")
	}
	if !k.HasPermission(PermTransfer) || k.HasPermission(PermWrite) {
		t.Error("HasPermission misbehaves")
	}
}

func TestPrefix(t *testing.T) {
	k := mustKey("k", "sk-x")
	if k.Prefix() != "" {
		t.Errorf("legacy prefix = %q, want empty", k.Prefix())
	}
	k.PublicID = "ab12cd34"
	if k.Prefix() != "sk-c2a-ab12cd34" {
		t.Errorf("prefix = %q", k.Prefix())
	}
}

func TestValidPermissionVocabulary(t *testing.T) {
	want := []string{"list", "read", "write", "delete", "purge", "transfer", "link", "fetch", "delegate", "relay"}
	if got := strings.Join(func() []string {
		out := make([]string, 0, len(AllPermissions))
		for _, p := range AllPermissions {
			out = append(out, string(p))
		}
		return out
	}(), ","); got != strings.Join(want, ",") {
		t.Errorf("AllPermissions = %s", got)
	}
	for _, p := range want {
		if !ValidPermission(Permission(p)) {
			t.Errorf("%s not valid", p)
		}
	}
	if ValidPermission("exec") {
		t.Error("exec is valid")
	}
}
