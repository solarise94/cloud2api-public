package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var hashA = "sha256:" + strings.Repeat("a", 64)
var hashB = "sha256:" + strings.Repeat("b", 64)

const validConfigTmpl = `
listen: 0.0.0.0:8080
openlist:
  endpoint: http://openlist:5244/
  token_env: OPENLIST_TOKEN
accounts:
  - id: baidu-main
    provider: baidu
    mount_path: /baidu
    enabled: true
  - id: pan123-main
    provider: 123pan
    mount_path: /123pan
    enabled: true
api_keys:
  - id: histopilot
    key_hash: "%[1]s"
    accounts: [baidu-main, pan123-main]
    permissions: [list, read, write, transfer]
`

var validConfig = fmt.Sprintf(validConfigTmpl, hashA)

func loadString(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadValid(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok-123")
	cfg, err := loadString(t, validConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Openlist.Endpoint != "http://openlist:5244" {
		t.Errorf("Endpoint = %q, want trailing slash trimmed", cfg.Openlist.Endpoint)
	}
	if cfg.Openlist.Token != "tok-123" {
		t.Errorf("Token = %q, want resolved from env", cfg.Openlist.Token)
	}
	if len(cfg.Accounts) != 2 || cfg.Accounts[0].ID != "baidu-main" || cfg.Accounts[1].MountPath != "/123pan" {
		t.Errorf("Accounts = %+v", cfg.Accounts)
	}
	if len(cfg.APIKeys) != 1 || len(cfg.APIKeys[0].Accounts) != 2 || len(cfg.APIKeys[0].Permissions) != 4 {
		t.Errorf("APIKeys = %+v", cfg.APIKeys)
	}
}

func TestProductionExampleConfig(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "synthetic-openlist-token")
	t.Setenv("ADMIN_PASSWORD", "synthetic-portal-password")
	cfg, err := Load(filepath.Join("..", "..", "config.production.example.yaml"))
	if err != nil {
		t.Fatalf("Load production example: %v", err)
	}
	if len(cfg.Accounts) != 0 || len(cfg.APIKeys) != 0 {
		t.Fatal("production example must not seed static drives or keys")
	}
	if cfg.Openlist.Endpoint != "http://127.0.0.1:5244" {
		t.Errorf("Endpoint = %q, want shared-namespace loopback", cfg.Openlist.Endpoint)
	}
	if cfg.Listen != "0.0.0.0:8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Admin == nil || !cfg.Admin.Enabled {
		t.Fatal("production example must enable portal-first setup")
	}
	if cfg.Admin.StateFile != "/var/lib/cloud2api/state.json" || cfg.Admin.SessionTTL.Standard() != 24*time.Hour {
		t.Fatal("production example must retain persistent portal state and session defaults")
	}
}

func TestLoadListenDefault(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	content := strings.Replace(validConfig, "listen: 0.0.0.0:8080\n", "", 1)
	cfg, err := loadString(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want default :8080", cfg.Listen)
	}
}

func TestLoadKeyHashEnv(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	t.Setenv("K1_HASH", hashB)
	content := strings.Replace(validConfig, fmt.Sprintf("key_hash: %q", hashA), "key_hash_env: K1_HASH", 1)
	cfg, err := loadString(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKeys[0].KeyHash != hashB {
		t.Errorf("KeyHash = %q, want resolved from env", cfg.APIKeys[0].KeyHash)
	}
}

func TestLoadKeyHashBothSet(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	t.Setenv("K1_HASH", hashB)
	content := strings.Replace(validConfig, "id: histopilot", "id: histopilot\n    key_hash_env: K1_HASH", 1)
	_, err := loadString(t, content)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want mutually-exclusive error", err)
	}
}

func TestLoadTokenEnvUnset(t *testing.T) {
	os.Unsetenv("OPENLIST_TOKEN")
	_, err := loadString(t, validConfig)
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("err = %v, want token env not set", err)
	}
}

func TestLoadBadEndpoint(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	_, err := loadString(t, strings.Replace(validConfig, "http://openlist:5244", "ftp://x", 1))
	if err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("err = %v, want endpoint scheme error", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load succeeded for missing file")
	}
}

func TestLoadAdminConfig(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	t.Setenv("ADMIN_PASSWORD", "portal-pw")
	content := validConfig + `
admin:
  enabled: true
  password_env: ADMIN_PASSWORD
  state_file: /var/lib/cloud2api/state.json
  session_ttl: 1h30m
`
	cfg, err := loadString(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin == nil || !cfg.Admin.Enabled {
		t.Fatal("admin section missing or disabled")
	}
	if cfg.Admin.Password != "portal-pw" {
		t.Errorf("Password = %q, want resolved from env", cfg.Admin.Password)
	}
	if cfg.Admin.StateFile != "/var/lib/cloud2api/state.json" {
		t.Errorf("StateFile = %q", cfg.Admin.StateFile)
	}
	if got := cfg.Admin.SessionTTL.Standard(); got != 90*time.Minute {
		t.Errorf("SessionTTL = %v, want 1h30m", got)
	}
}

func TestLoadAdminSessionTTLDefault(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	t.Setenv("ADMIN_PASSWORD", "pw")
	content := validConfig + `
admin:
  enabled: true
  password_env: ADMIN_PASSWORD
  state_file: /tmp/state.json
`
	cfg, err := loadString(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Admin.SessionTTL.Standard(); got != DefaultSessionTTL {
		t.Errorf("SessionTTL = %v, want default %v", got, DefaultSessionTTL)
	}
}

func TestLoadAdminDisabledNeedsNoPassword(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	content := validConfig + `
admin:
  enabled: false
`
	if _, err := loadString(t, content); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadAdminErrors(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name:    "password env unset",
			content: validConfig + "\nadmin:\n  enabled: true\n  password_env: ADMIN_PASSWORD\n  state_file: /tmp/s.json\n",
			wantSub: "not set",
		},
		{
			name:    "password_env missing",
			content: validConfig + "\nadmin:\n  enabled: true\n  state_file: /tmp/s.json\n",
			wantSub: "password_env",
		},
		{
			name:    "state_file missing",
			content: validConfig + "\nadmin:\n  enabled: true\n  password_env: ADMIN_PASSWORD\n",
			wantSub: "state_file",
		},
		{
			name:    "bad duration",
			content: validConfig + "\nadmin:\n  enabled: true\n  password_env: ADMIN_PASSWORD\n  state_file: /tmp/s.json\n  session_ttl: xyz\n",
			wantSub: "invalid duration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "password env unset" {
				os.Unsetenv("ADMIN_PASSWORD")
			} else {
				t.Setenv("ADMIN_PASSWORD", "pw")
			}
			_, err := loadString(t, tc.content)
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantSub)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("OPENLIST_TOKEN", "tok")
	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name:    "duplicate account id",
			content: strings.Replace(validConfig, "id: pan123-main", "id: baidu-main", 1),
			wantSub: "duplicate account id",
		},
		{
			name:    "duplicate mount path",
			content: strings.Replace(validConfig, "mount_path: /123pan", "mount_path: /baidu", 1),
			wantSub: "duplicate mount_path",
		},
		{
			name:    "relative mount path",
			content: strings.Replace(validConfig, "mount_path: /123pan", "mount_path: 123pan", 1),
			wantSub: "absolute path",
		},
		{
			name:    "root mount path",
			content: strings.Replace(validConfig, "mount_path: /123pan", "mount_path: /", 1),
			wantSub: "not supported",
		},
		{
			name:    "unclean mount path",
			content: strings.Replace(validConfig, "mount_path: /123pan", "mount_path: /123pan/", 1),
			wantSub: "clean path",
		},
		{
			name:    "mount path traversal",
			content: strings.Replace(validConfig, "mount_path: /123pan", "mount_path: /x/../123pan", 1),
			wantSub: "clean path",
		},
		{
			name:    "account id with slash",
			content: strings.Replace(validConfig, "id: pan123-main", "id: pan123/main", 1),
			wantSub: "must not contain",
		},
		{
			name:    "unknown account reference",
			content: strings.Replace(validConfig, "accounts: [baidu-main, pan123-main]", "accounts: [baidu-main, nope]", 1),
			wantSub: "unknown account",
		},
		{
			name:    "unknown permission",
			content: strings.Replace(validConfig, "permissions: [list, read, write, transfer]", "permissions: [list, exec]", 1),
			wantSub: "unknown permission",
		},
		{
			name:    "bad key hash format",
			content: strings.Replace(validConfig, fmt.Sprintf("key_hash: %q", hashA), `key_hash: "md5:deadbeef"`, 1),
			wantSub: "key_hash",
		},
		{
			name:    "bad key hash hex",
			content: strings.Replace(validConfig, "sha256:aaaa", "sha256:zzzz", 1),
			wantSub: "key_hash",
		},
		{
			name: "duplicate api key id",
			content: strings.Replace(validConfig,
				"id: histopilot\n",
				fmt.Sprintf("id: histopilot\n    key_hash: %q\n    accounts: [baidu-main]\n    permissions: [list]\n  - id: histopilot\n", hashB),
				1),
			wantSub: "duplicate api key id",
		},
		{
			name:    "unknown yaml field",
			content: strings.Replace(validConfig, "listen: 0.0.0.0:8080", "listen: 0.0.0.0:8080\nunknown_field: 1", 1),
			wantSub: "unknown_field",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadString(t, tc.content)
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantSub)
			}
		})
	}
}
