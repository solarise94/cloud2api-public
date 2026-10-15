// Package goapi is the loginsdk adapter for the GoAPI relay driver: an
// endpoint URL and an upstream API key chained to somebody else's
// cloud2api/GoAPI instance. Submit verifies the pair against the peer's
// /v1/me and probes a listing of the resolved drive's root, so a wrong URL,
// a rejected key or an ungranted drive fail at the form instead of at save
// time (the driver validates again when the storage is saved). Values are
// written only into the OpenList storage addition; nothing is kept in
// cloud2api and no key text appears in any message or metadata.
package goapi

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

var meta = loginsdk.Meta{
	ID:     "goapi",
	Name:   "GoAPI 中转",
	Driver: "GoAPI",
	Method: loginsdk.MethodForm,
	// No developer application exists in this protocol: the peer's key is
	// the only credential.
	AppModes: []loginsdk.AppMode{loginsdk.AppNone},
	FormFields: []loginsdk.Field{
		{Key: "url", Label: "GoAPI 端点地址", Type: "url", Required: true,
			Help: "对端 GoAPI（cloud2api）服务的根地址，例如 https://goapi.example.com"},
		{Key: "api_key", Label: "上游 API Key", Type: "password", Required: true, Secret: true,
			Help: "对端管理员签发的 API Key，只写入本存储配置，不再出现在任何页面"},
		{Key: "drive", Label: "上游网盘（可选）", Type: "text", Required: false, CreateOnly: true,
			Help: "对端网盘 ID；留空使用对端 Key 的默认网盘（必须是该 Key 明确授权的网盘）"},
	},
	RootField:   "root_folder_path",
	RootDefault: "/",
	RootLabel:   "根目录路径",
	RootHelp:    "对端网盘内已存在的目录，作为本存储的根；超出该目录的访问都会被拒绝",
	ReauthFields: []string{
		"url", "api_key",
	},
	FormLabel: "API 中转",
	Notes: []string{
		"仅支持 GoAPI（cloud2api）/v1 协议，不兼容任意 REST 网盘 API",
		"对端必须支持串联跳数保护（/v1/me 返回 relay_hop_limit=4），旧版本上游会被拒绝",
		"URL 与 Key 只保存在 OpenList 存储配置中；对端 Key 的权限、根目录和限额决定实际可访问范围",
		"中转链路最多经过 4 层 GoAPI 存储，超出会被拒绝以防止成环",
		"0 字节与未知大小的文件通过对端 multipart 接口上传；上传以对端类型化回执（路径、大小、CRC64、SHA256）为准",
	},
	DocsURL:  "",
	Verified: "",
}

// New returns the GoAPI login adapter.
func New() loginsdk.Adapter { return Adapter{} }

// Adapter implements loginsdk.FormAdapter for the GoAPI relay.
type Adapter struct{}

func (Adapter) Meta() loginsdk.Meta { return meta }

func (Adapter) CheckApp(app loginsdk.App) error {
	if app.Mode != loginsdk.AppNone {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "GoAPI 中转不需要应用配置，请使用端点地址 + API Key 登录")
	}
	return nil
}

func (Adapter) Submit(ctx context.Context, in loginsdk.SubmitInput) (*loginsdk.AuthConfig, error) {
	endpoint := strings.TrimSpace(in.Values["url"])
	endpoint = strings.TrimRight(endpoint, "/")
	apiKey := in.Values["api_key"]
	cli, err := goapiclient.New(endpoint, apiKey)
	if err != nil {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "%s", err)
	}
	me, err := cli.Me(ctx)
	if err != nil {
		return nil, goapiSubmitError(err)
	}
	// A peer without hop protection resets the relay counter across the
	// chain; refuse it instead of pretending the old /v1 peer is safe.
	if err := me.ValidateRelayCapability(); err != nil {
		return nil, loginsdk.Errorf(loginsdk.CodeUnsupported, "%v", err)
	}
	if len(me.Drives) == 0 {
		return nil, loginsdk.Errorf(loginsdk.CodeProvider, "对端拒绝访问：该 API Key 未被授予任何网盘")
	}

	drive := strings.TrimSpace(in.Values["drive"])
	if in.Reauth {
		// Create-only field absent on re-auth: the stored drive and root
		// stay as they are; only the endpoint/key are validated here.
		drive = ""
	} else {
		if drive != "" {
			if err := goapiclient.ValidDriveID(drive); err != nil {
				return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "上游网盘 ID 含非法字符")
			}
		}
		if drive == "" {
			switch {
			case me.Key.DefaultDrive != "" && me.HasDrive(me.Key.DefaultDrive):
				drive = me.Key.DefaultDrive
			case len(me.Drives) == 1:
				drive = me.Drives[0].ID
			default:
				return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput,
					"请填写上游网盘：该 Key 的授权列表为 [%s]", strings.Join(me.GrantedIDs(), ", "))
			}
		} else if !me.HasDrive(drive) {
			return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput,
				"上游网盘 %s 不在该 Key 的授权列表 [%s] 内", display(drive), strings.Join(me.GrantedIDs(), ", "))
		}
		// Probe that the key can stat and list the drive root (the
		// configured root is re-checked by the driver when the storage is
		// saved).
		if _, err := cli.Stat(ctx, drive, "/"); err != nil {
			return nil, goapiSubmitError(err)
		}
		if _, err := cli.List(ctx, drive, "/", 1); err != nil {
			return nil, goapiSubmitError(err)
		}
	}

	auth := &loginsdk.AuthConfig{
		Driver:         meta.Driver,
		CredentialType: "api_key",
		AppMode:        loginsdk.AppNone,
		Renewal:        "API Key 由对端管理员签发，不会自动续期；失效后请重新填写",
		Account:        displayHost(endpoint),
		Fields: map[string]any{
			"url":     endpoint,
			"api_key": apiKey,
		},
		Secrets: []string{apiKey},
	}
	if !in.Reauth {
		auth.CreateFields = map[string]any{"drive": drive}
	}
	return auth, nil
}

// goapiSubmitError maps a client error onto a displayable SDK error. Client
// messages are already single-line and scrubbed of the key.
func goapiSubmitError(err error) *loginsdk.Error {
	var pe *goapiclient.Error
	if errors.As(err, &pe) {
		if pe.Status == 401 {
			return loginsdk.Errorf(loginsdk.CodeProvider, "对端拒绝了这个 API Key，请核对后重试")
		}
		return loginsdk.Errorf(loginsdk.CodeProvider, "对端返回错误（HTTP %d）：%s", pe.Status, pe.Message)
	}
	return loginsdk.Errorf(loginsdk.CodeNetwork, "无法连接对端 /v1/me：%s", err)
}

func display(s string) string { return s }

// displayHost shows only where the relay points, never any key material.
func displayHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "（地址无效）"
}
