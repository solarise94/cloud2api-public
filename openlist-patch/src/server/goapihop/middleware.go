// Package goapihop carries the GoAPI relay hop counter through OpenList's
// request context. The router registers Middleware() once (upstream hook,
// patches/0001): it validates the inbound X-GoAPI-Hop header of every request
// and stores the count in the request context, where the GoAPI storage
// driver's outbound calls pick it up (goapiclient.OutboundHop). Relay chains
// are therefore cut by HTTP after goapiclient.MaxHops transits, independent
// of any mount topology.
package goapihop

import (
	"errors"
	"net/http"

	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/gin-gonic/gin"
)

// Middleware validates the inbound relay hop header and publishes the count
// to the request context. A request without the header passes unchanged
// (zero hops). A malformed header is rejected with 400, a value above
// goapiclient.MaxHops with 403; both use the OpenList {code,message}
// envelope and never echo the header value.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Request.Header[http.CanonicalHeaderKey(goapiclient.HopHeader)]; ok {
			hop, err := goapiclient.ParseHop(c.Request.Header.Values(goapiclient.HopHeader))
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, goapiclient.ErrHopExceeded) {
					status = http.StatusForbidden
				}
				c.AbortWithStatusJSON(status, gin.H{
					"code":    status,
					"message": "invalid relay hop header",
				})
				return
			}
			if hop > 0 {
				c.Request = c.Request.WithContext(goapiclient.WithHop(c.Request.Context(), hop))
			}
		}
		c.Next()
	}
}
