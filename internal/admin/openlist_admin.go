package admin

import (
	"fmt"
	"time"

	"cloud2api/internal/openlist"
)

// UpstreamError carries an OpenList business error (non-200 body code) so
// the portal can surface it to the user verbatim.
type UpstreamError struct {
	Code    int
	Message string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("openlist error %d: %s", e.Code, e.Message)
}

// storage is the portal's alias for the shared storage row.
type storage = openlist.StorageInfo

// defaultAdminCallTimeout bounds a single admin API call.
const defaultAdminCallTimeout = 60 * time.Second
