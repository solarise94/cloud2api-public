//go:build loginsdk_example

package all

import (
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk/adapters/example"
)

// The contributor example adapter stays out of production builds; build or
// test with -tags loginsdk_example to register it (it doubles as the module's
// end-to-end harness). It registers last, after the bundled adapters.
func init() {
	loginsdk.Register(example.New())
}
