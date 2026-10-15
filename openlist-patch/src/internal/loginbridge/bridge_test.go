package loginbridge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// The whole package shares one in-memory database (as upstream's op tests
// do), so every test uses its own mount paths.
func init() {
	conf.Conf = conf.DefaultConfig("data")
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: conf.Conf.Database.TablePrefix},
	})
	if err != nil {
		panic("failed to connect database: " + err.Error())
	}
	db.Init(dB)
	op.RegisterDriver(func() driver.Driver { return &fakeDriver{} })
	if err := Migrate(); err != nil {
		panic("loginbridge migrate: " + err.Error())
	}
}

// fakeAddition mirrors the shape of a real cloud-drive driver addition:
// typed defaults, an embedded root, a secret credential and one field
// without a json tag (invisible to the form item registration).
type fakeAddition struct {
	driver.RootPath
	RefreshToken string `json:"refresh_token" required:"true"`
	AccessToken  string // no json tag: not a form item, but still serialized
	UseOnlineAPI bool   `json:"use_online_api" default:"true"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	UploadThread string `json:"upload_thread" default:"3"`
	ChunkSize    int64  `json:"chunk_size" type:"number" default:"5"`
	Mode         string `json:"mode" type:"select" options:"a,b" default:"b"`
	Keep         string `json:"keep"`
}

// fakeDriver initializes according to the refresh token: "good" succeeds,
// "bad…" fails with an error that echoes the token (to test scrubbing),
// "rotate" rotates the token and persists it, "rotate-bad" rotates then
// fails. Any other token initializes successfully.
type fakeDriver struct {
	model.Storage
	addition fakeAddition
}

func (d *fakeDriver) Config() driver.Config {
	return driver.Config{Name: "ConnectFake", DefaultRoot: "/"}
}

func (d *fakeDriver) GetAddition() driver.Additional { return &d.addition }

func (d *fakeDriver) Init(ctx context.Context) error {
	switch {
	case d.addition.RefreshToken == "good":
		return nil
	case strings.HasPrefix(d.addition.RefreshToken, "bad"):
		return fmt.Errorf("connect-fake: provider rejected refresh token %q", d.addition.RefreshToken)
	case d.addition.RefreshToken == "rotate":
		d.addition.RefreshToken = "rotated"
		op.MustSaveDriverStorage(d)
		return nil
	case d.addition.RefreshToken == "rotate-bad":
		d.addition.RefreshToken = "rotated"
		op.MustSaveDriverStorage(d)
		return fmt.Errorf("connect-fake: provider rejected refresh token %q", "rotate-bad")
	}
	return nil
}

// Drop fails for "bad-drop…" tokens; op.DeleteStorageById still deletes the
// row then and only reports the error.
func (d *fakeDriver) Drop(ctx context.Context) error {
	if strings.HasPrefix(d.addition.RefreshToken, "bad-drop") {
		return fmt.Errorf("connect-fake: drop failed")
	}
	return nil
}

func (d *fakeDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}

func (d *fakeDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, nil
}

// fakeAdapter is a form adapter for the fake driver; only Meta and CheckApp
// are used by loginbridge.
type fakeAdapter struct {
	rootField string
	reauthErr error
}

func newFakeAdapter() *fakeAdapter { return &fakeAdapter{rootField: "root_folder_path"} }

func (f *fakeAdapter) Meta() loginsdk.Meta {
	rootDefault := "/"
	if f.rootField == "root_folder_id" {
		rootDefault = "root"
	}
	return loginsdk.Meta{
		ID:          "fake",
		Name:        "测试网盘",
		Driver:      "ConnectFake",
		Method:      loginsdk.MethodForm,
		AppModes:    []loginsdk.AppMode{loginsdk.AppNone},
		FormFields:  []loginsdk.Field{{Key: "user", Label: "用户名"}},
		RootField:   f.rootField,
		RootDefault: rootDefault,
		RootLabel:   "根目录",
		ReauthFields: []string{
			"refresh_token", "use_online_api", "client_id", "client_secret",
		},
	}
}

func (f *fakeAdapter) CheckApp(app loginsdk.App) error { return nil }

func (f *fakeAdapter) Submit(ctx context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	return nil, nil
}

func (f *fakeAdapter) CheckReauth(addition map[string]any) error { return f.reauthErr }

func fakeApp() loginsdk.App {
	return loginsdk.App{Platform: "fake", Mode: loginsdk.AppNone}
}

// fakeAuth builds a valid AuthConfig whose only credential is the refresh
// token; extra adds more credential fields.
func fakeAuth(token string, extra map[string]any) *loginsdk.AuthConfig {
	fields := map[string]any{"refresh_token": token}
	for k, v := range extra {
		fields[k] = v
	}
	return &loginsdk.AuthConfig{
		Driver:         "ConnectFake",
		CredentialType: "oauth_refresh_token",
		AppMode:        loginsdk.AppNone,
		Fields:         fields,
		Secrets:        []string{token},
	}
}

func mustRow(t *testing.T, id uint) *model.Storage {
	t.Helper()
	row, err := db.GetStorageById(id)
	if err != nil {
		t.Fatalf("storage #%d not in db: %v", id, err)
	}
	return row
}

func mustDecode(t *testing.T, addition string) map[string]any {
	t.Helper()
	m, err := DecodeAddition(addition)
	if err != nil {
		t.Fatalf("decode addition %q: %v", addition, err)
	}
	return m
}
