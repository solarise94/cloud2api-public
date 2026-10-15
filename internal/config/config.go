// Package config loads and validates the cloud2api YAML configuration
// (DESIGN.md §3). Secret material (OpenList token, key hashes) is resolved
// from environment variables named by *_env fields, never stored in YAML.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cloud2api/internal/auth"
	"gopkg.in/yaml.v3"
)

const defaultListen = ":8080"

// DefaultSessionTTL is used when admin.session_ttl is omitted.
const DefaultSessionTTL = 24 * time.Hour

var keyHashRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Duration wraps time.Duration for YAML parsing ("24h", "30m", ...).
type Duration time.Duration

func (d Duration) Standard() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Listen   string    `yaml:"listen"`
	Openlist Openlist  `yaml:"openlist"`
	Admin    *Admin    `yaml:"admin"`
	Accounts []Account `yaml:"accounts"`
	APIKeys  []APIKey  `yaml:"api_keys"`
}

type Openlist struct {
	Endpoint string `yaml:"endpoint"`
	// TokenEnv names the environment variable holding the OpenList admin
	// token; the token itself never appears in the config file.
	TokenEnv string `yaml:"token_env"`
	// Token is resolved from TokenEnv during Load.
	Token string `yaml:"-"`
}

type Account struct {
	ID        string `yaml:"id"`
	Provider  string `yaml:"provider"`
	MountPath string `yaml:"mount_path"`
	Enabled   bool   `yaml:"enabled"`
}

// Admin configures the management portal. It is optional: when absent, or
// when Enabled is false or the password env var is unset, the portal is not
// mounted at all (/admin/* answers 404).
type Admin struct {
	Enabled bool `yaml:"enabled"`
	// PasswordEnv names the environment variable holding the portal admin
	// password; the password never appears in the config file.
	PasswordEnv string `yaml:"password_env"`
	// Password is resolved from PasswordEnv during Load.
	Password string `yaml:"-"`
	// StateFile persisted the v0.3 portal accounts; v0.4 imports it into the
	// SQLite database on first start (renamed to <state_file>.migrated).
	StateFile string `yaml:"state_file"`
	// DBFile is the SQLite database path (v0.4). Defaults to
	// cloud2api.db next to StateFile.
	DBFile string `yaml:"db_file"`
	// SessionTTL defaults to DefaultSessionTTL when omitted.
	SessionTTL Duration `yaml:"session_ttl"`
}

// DatabasePath resolves the SQLite database location.
func (a *Admin) DatabasePath() string {
	if a == nil {
		return "cloud2api.db"
	}
	if a.DBFile != "" {
		return a.DBFile
	}
	if a.StateFile != "" {
		return filepath.Join(filepath.Dir(a.StateFile), "cloud2api.db")
	}
	return "cloud2api.db"
}

type APIKey struct {
	ID         string   `yaml:"id"`
	KeyHash    string   `yaml:"key_hash"`
	KeyHashEnv string   `yaml:"key_hash_env"`
	Accounts   []string `yaml:"accounts"`
	// Permissions use the vocabulary validated by package auth.
	Permissions []string `yaml:"permissions"`
}

// Load reads, resolves and validates the configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.resolve(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) resolve() error {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	c.Openlist.Endpoint = strings.TrimRight(c.Openlist.Endpoint, "/")
	if c.Openlist.TokenEnv != "" {
		c.Openlist.Token = os.Getenv(c.Openlist.TokenEnv)
	}
	if c.Admin != nil && c.Admin.Enabled {
		c.Admin.Password = os.Getenv(c.Admin.PasswordEnv)
	}
	for i := range c.APIKeys {
		k := &c.APIKeys[i]
		switch {
		case k.KeyHash != "" && k.KeyHashEnv != "":
			return fmt.Errorf("api key %q: key_hash and key_hash_env are mutually exclusive", k.ID)
		case k.KeyHashEnv != "":
			k.KeyHash = os.Getenv(k.KeyHashEnv)
		}
	}
	return nil
}

// ValidateAccountID applies the shared account id rules (non-empty, no '/' or
// whitespace). Used both by config validation and the admin portal.
func ValidateAccountID(id string) error {
	if id == "" {
		return errors.New("account id must not be empty")
	}
	if strings.ContainsAny(id, "/ \t") {
		return fmt.Errorf("account id %q must not contain '/' or whitespace", id)
	}
	return nil
}

// ValidateMountPath applies the shared mount_path rules (absolute, clean,
// non-root). Used both by config validation and the admin portal.
func ValidateMountPath(mp string) error {
	if !strings.HasPrefix(mp, "/") {
		return fmt.Errorf("mount_path %q must be an absolute path", mp)
	}
	if mp == "/" {
		return errors.New(`mount_path "/" is not supported`)
	}
	if path.Clean(mp) != mp {
		return fmt.Errorf("mount_path %q must be a clean path", mp)
	}
	return nil
}

func (c *Config) validate() error {
	if u, err := url.Parse(c.Openlist.Endpoint); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("openlist.endpoint %q must be an absolute http(s) URL", c.Openlist.Endpoint)
	}
	if c.Openlist.TokenEnv == "" {
		return errors.New("openlist.token_env is required")
	}
	if c.Openlist.Token == "" {
		return fmt.Errorf("environment variable %q (openlist.token_env) is not set", c.Openlist.TokenEnv)
	}
	if c.Admin != nil && c.Admin.Enabled {
		a := c.Admin
		if a.PasswordEnv == "" {
			return errors.New("admin.password_env is required when admin is enabled")
		}
		if a.Password == "" {
			return fmt.Errorf("environment variable %q (admin.password_env) is not set", a.PasswordEnv)
		}
		if a.StateFile == "" {
			return errors.New("admin.state_file is required when admin is enabled")
		}
		if a.SessionTTL == 0 {
			a.SessionTTL = Duration(DefaultSessionTTL)
		}
		if a.SessionTTL <= 0 {
			return fmt.Errorf("admin.session_ttl %q must be positive", time.Duration(a.SessionTTL))
		}
	}

	ids := make(map[string]bool, len(c.Accounts))
	mounts := make(map[string]bool, len(c.Accounts))
	for _, a := range c.Accounts {
		if err := ValidateAccountID(a.ID); err != nil {
			return err
		}
		if ids[a.ID] {
			return fmt.Errorf("duplicate account id %q", a.ID)
		}
		ids[a.ID] = true

		if err := ValidateMountPath(a.MountPath); err != nil {
			return fmt.Errorf("account %q: %w", a.ID, err)
		}
		if mounts[a.MountPath] {
			return fmt.Errorf("duplicate mount_path %q", a.MountPath)
		}
		mounts[a.MountPath] = true
	}

	keyIDs := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if k.ID == "" {
			return errors.New("api key id must not be empty")
		}
		if keyIDs[k.ID] {
			return fmt.Errorf("duplicate api key id %q", k.ID)
		}
		keyIDs[k.ID] = true
		if !keyHashRe.MatchString(k.KeyHash) {
			return fmt.Errorf("api key %q: key_hash must match \"sha256:<64 hex chars>\"", k.ID)
		}
		for _, acct := range k.Accounts {
			if !ids[acct] {
				return fmt.Errorf("api key %q references unknown account %q", k.ID, acct)
			}
		}
		for _, p := range k.Permissions {
			if !auth.ValidPermission(auth.Permission(p)) {
				return fmt.Errorf("api key %q: unknown permission %q (valid: %s)", k.ID, p, auth.AllPermissionNames())
			}
		}
	}
	return nil
}
