package goapiclient

import (
	"context"
	"errors"
	"strconv"
)

// The GoAPI relay loop guard. X-GoAPI-Hop counts how many GoAPI storages a
// request has already traversed. Every GoAPI-speaking ingress (cloud2api /v1,
// the GoAPI driver's outbound calls) validates it and the counter propagates
// through request contexts, so a relay chain is cut by HTTP after MaxHops
// transits no matter what the mount topology claims. The same rule is
// implemented by:
//   - server/goapihop.Middleware (OpenList ingress, this module),
//   - the cloud2api /v1 and Gokapi-compat ingress (separate module).
//
// The header name and the bound are part of the protocol; keep both in sync.
const (
	// HopHeader is the relay hop counter header.
	HopHeader = "X-GoAPI-Hop"
	// MaxHops is the largest hop value any ingress accepts. A driver always
	// sends ingress+1, so at most MaxHops GoAPI storages can be traversed;
	// the next outbound (MaxHops+1) is rejected by the peer's ingress.
	MaxHops = 4
)

// Hop errors distinguish the two ingress failure shapes: 400 for a malformed
// header (multiple values, not a number, negative) and 403 for a well-formed
// value beyond the cap.
var (
	// ErrHopMalformed reports a syntactically invalid hop header.
	ErrHopMalformed = errors.New("goapi: invalid " + HopHeader + " header")
	// ErrHopExceeded reports a well-formed hop value above MaxHops.
	ErrHopExceeded = errors.New("goapi: " + HopHeader + " exceeds the relay limit")
)

// ParseHop validates the raw header values of one request. No header means
// zero hops (a request that has not traversed a GoAPI storage yet). Exactly
// one value is accepted; it must be a decimal integer in [0, MaxHops].
func ParseHop(values []string) (int, error) {
	switch {
	case len(values) == 0:
		return 0, nil
	case len(values) > 1:
		return 0, ErrHopMalformed
	}
	raw := values[0]
	h, err := strconv.Atoi(raw)
	if err != nil || h < 0 {
		return 0, ErrHopMalformed
	}
	// Canonical decimal only: " 1", "+1" and "01" are refused.
	if strconv.Itoa(h) != raw {
		return 0, ErrHopMalformed
	}
	if h > MaxHops {
		return 0, ErrHopExceeded
	}
	return h, nil
}

type hopContextKey struct{}

// WithHop marks a context with a hop count. Storage-driver calls mark their
// context (with the inbound count, 0 included): the marker means "this call
// is a relay transit", and the wire header carries marker+1. Control-plane
// calls (the login adapter's probes) use an unmarked context and send no
// header. A peer ingress rejects values above MaxHops, which bounds every
// relay chain.
func WithHop(ctx context.Context, hop int) context.Context {
	if hop < 0 {
		hop = 0
	}
	return context.WithValue(ctx, hopContextKey{}, hop)
}

// HopFromContext returns the hop count carried by ctx and whether the
// context is marked as a transit at all.
func HopFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	h, ok := ctx.Value(hopContextKey{}).(int)
	if !ok || h < 0 {
		return 0, false
	}
	return h, true
}

// OutboundHop returns the wire hop value for a marked context and whether a
// header must be sent: one more than the inbound count.
func OutboundHop(ctx context.Context) (int, bool) {
	h, ok := HopFromContext(ctx)
	if !ok {
		return 0, false
	}
	return h + 1, true
}

// Transit marks ctx as one relay transit at the current inbound count (zero
// included), so the driver's outbound calls always carry inbound+1.
func Transit(ctx context.Context) context.Context {
	h, _ := HopFromContext(ctx)
	return WithHop(ctx, h)
}
