// Package all registers the login adapters bundled into the OpenList connect
// module, in display order. To add a platform, add its package under
// pkg/loginsdk/adapters/<name> and one Register line here.
package all

import (
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/aliyunopen"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/baidu"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/goapi"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/googledrive"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/onedrive"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/pan123"
)

func init() {
	loginsdk.Register(baidu.New())
	loginsdk.Register(aliyunopen.New())
	loginsdk.Register(pan123.New())
	loginsdk.Register(onedrive.New())
	loginsdk.Register(googledrive.New())
	loginsdk.Register(goapi.New())
}
