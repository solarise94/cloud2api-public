package connect

// Blank import: registers the GoAPI relay storage driver (drivers/goapi)
// into op's driver table at start-up. This lives beside the connect module
// because the router patch already loads this package; the driver itself
// knows nothing about connect. There is no import cycle: the driver only
// depends on internal/{driver,op,model} and pkg/goapiclient.
import _ "github.com/OpenListTeam/OpenList/v4/drivers/goapi"
