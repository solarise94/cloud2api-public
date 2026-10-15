package op

import (
	"context"
	"strconv"

	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
)

// Relay cycles can re-enter the same path before an ancestor's outbound call
// finishes. Separate their flight keys by hop; cache keys remain unsuffixed.
func relayFlightKey(ctx context.Context, base string) string {
	if hop, transit := goapiclient.HopFromContext(ctx); transit {
		return base + "\x00hop:" + strconv.Itoa(hop)
	}
	return base
}
