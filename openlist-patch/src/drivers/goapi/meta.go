package goapi

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// Addition is the storage configuration of the GoAPI relay driver. The
// endpoint URL and the upstream API key live ONLY here (OpenList-owned);
// they are validated against the peer during Init before the storage is
// usable.
type Addition struct {
	driver.RootPath
	URL string `json:"url" required:"true" type:"string" help:"GoAPI (cloud2api) 服务的根地址，例如 https://goapi.example.com"`
	// APIKey is the peer's relay key. type:"password" keeps the native
	// storage form from displaying it; it is never logged or echoed.
	APIKey string `json:"api_key" required:"true" type:"password" help:"对端签发的 API Key，仅写入本存储配置"`
	// Drive is the granted drive on the peer. Empty resolves to the peer
	// key's default drive, accepted only when it is inside the key's
	// explicit grants.
	Drive string `json:"drive" help:"上游网盘 ID；留空使用对端 Key 的默认网盘（必须在其明确授权列表内）"`
	// SystemNS is this storage's private system namespace: a random 128-bit
	// value the driver generates at first Init, after which OpenList's own
	// storage save convention persists it and re-authorization keeps it. It
	// names the ordinary peer directories backing the reserved virtual
	// system view (/.c2a-trash, /.relay — see system.go). It is not a
	// credential and appears in no wizard; admins must neither edit nor copy
	// it (clearing it orphans this storage's previous alias directories on
	// the peer, but never claims or overwrites unrelated paths).
	SystemNS string `json:"system_ns" type:"text" help:"驱动私有的系统命名空间（Init 自动生成并随存储保存），用于命名 /.c2a-trash 与 /.relay 的后端目录；请勿编辑或复制"`
}

var config = driver.Config{
	Name:             "GoAPI",
	LocalSort:        true,
	DefaultRoot:      "/",
	OnlyProxy:        true,
	NoLinkURL:        true,
	ProxyRangeOption: true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &GoAPI{}
	})
}
