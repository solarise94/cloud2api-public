// Relay hop context for the GoAPI storage chain (openlist-patch
// drivers/goapi + server/goapihop). The header name, the bound and the
// semantics are the protocol shared with the peer's goapiclient package;
// the two modules cannot import each other, so the constant is mirrored
// here and locked by tests on both sides.
//
// Ingress: /v1 and the Gokapi compat layer validate the inbound header
// before anything else (malformed or repeated -> 400 invalid_request,
// above the cap -> 403), then publish the count on the request context via
// openlist.WithHop. The OpenList client attaches it to every outbound call,
// where the peer's GoAPI driver reads it and adds one — so a relay chain is
// cut by HTTP after a bounded number of transits no matter what the mount
// topology claims.
package agentapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"cloud2api/internal/openlist"
)

const (
	// HopHeader mirrors openlist-patch pkg/goapiclient.HopHeader.
	HopHeader = "X-GoAPI-Hop"
	// hopLimit mirrors openlist-patch pkg/goapiclient.MaxHops.
	hopLimit = 4
)

// hopGuard validates the inbound X-GoAPI-Hop header and returns r with the
// count on its context (openlist.WithHop). Absent means zero hops. More
// than one value, a non-number, a negative number or a value above the cap
// yields a typed API error (400/403) that must be answered before any
// authentication work.
func hopGuard(r *http.Request) (*http.Request, *APIError) {
	values := r.Header.Values(HopHeader)
	switch {
	case len(values) == 0:
		return r, nil
	case len(values) > 1:
		e := errInvalidRequest("repeated " + HopHeader + " headers are not accepted")
		e.status = http.StatusBadRequest
		return r, e
	}
	h, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || h < 0 {
		e := errInvalidRequest(HopHeader + " must be a decimal hop counter between 0 and " + strconv.Itoa(hopLimit))
		e.status = http.StatusBadRequest
		return r, e
	}
	if h > hopLimit {
		e := newAPIError(http.StatusForbidden, CodeHopExceeded,
			fmt.Sprintf("%s %d exceeds the relay limit of %d", HopHeader, h, hopLimit))
		e.Hint = "the request has already traversed too many GoAPI relay storages"
		e.Retryable = false
		return r, e
	}
	if h > 0 {
		r = r.WithContext(openlist.WithHop(r.Context(), h))
	}
	return r, nil
}

// relayHopLimitJSON is the capability advertised by GET /v1/me: the largest
// hop value this instance's ingress accepts. GoAPI peers (drivers, login
// adapters) require it before trusting this instance inside a chain.
func relayHopLimitJSON() int { return hopLimit }
