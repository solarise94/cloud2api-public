package openlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrConnectUnavailable reports that this OpenList build does not carry the
// login module (the openlist-patch): an unpatched OpenList answers the module's
// API path with its SPA index.html (HTTP 200, text/html) or with an envelope
// code 404. That means "module not available" — not an operational failure
// worth a warning.
var ErrConnectUnavailable = errors.New("openlist connect module not available")

// ConnectPlatform is the identity subset of one login-module platform: which
// storage driver it can re-authorize and how. The platform's app configuration
// fields are deliberately absent from the decode structs
// (IMPLEMENTATION-openlist-login-fork §适配补丁范围: cloud2api neither receives
// nor stores login credentials), so they are never even unmarshalled.
type ConnectPlatform struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Driver string `json:"driver"`
	Method string `json:"method"`
}

// ConnectInfo describes the login module of a patched OpenList.
type ConnectInfo struct {
	Version         string            `json:"version"`
	UpstreamVersion string            `json:"upstream_version"`
	Handoff         bool              `json:"handoff"`
	Platforms       []ConnectPlatform `json:"platforms"`
}

// SupportsDriver reports whether some platform re-authorizes storages of that
// driver.
func (i ConnectInfo) SupportsDriver(driver string) bool {
	for _, pl := range i.Platforms {
		if pl.Driver == driver {
			return true
		}
	}
	return false
}

// connectBodyLimit bounds the platforms response read (openlist-patch docs
// §平台与应用配置: the payload stays far below this).
const connectBodyLimit = 1 << 20

// ConnectInfo reads GET /api/admin/connect/platforms. Only
// data.module.version, data.module.upstream_version and
// data.platforms[].id/name/driver/method are decoded. A non-JSON body (the
// unpatched SPA) or an envelope code 404 maps to ErrConnectUnavailable; any
// other non-200 envelope code is an ordinary error with code and message.
func (c *Client) ConnectInfo(ctx context.Context) (ConnectInfo, error) {
	u := url.URL{Path: "/api/admin/connect/platforms"}
	req, err := c.NewRequest(ctx, http.MethodGet, u, nil, 0, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return ConnectInfo{}, err
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		return ConnectInfo{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, connectBodyLimit))
	if err != nil {
		return ConnectInfo{}, err
	}
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Module struct {
				Version         string `json:"version"`
				UpstreamVersion string `json:"upstream_version"`
				Handoff         bool   `json:"handoff"`
			} `json:"module"`
			Platforms []ConnectPlatform `json:"platforms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ConnectInfo{}, fmt.Errorf("%w: non-JSON response (HTTP %d)", ErrConnectUnavailable, resp.StatusCode)
	}
	if envelope.Code == http.StatusNotFound {
		return ConnectInfo{}, fmt.Errorf("%w: code=404 message=%s", ErrConnectUnavailable, envelope.Message)
	}
	if envelope.Code != 0 && envelope.Code != http.StatusOK {
		return ConnectInfo{}, fmt.Errorf("openlist connect platforms: code=%d message=%s", envelope.Code, envelope.Message)
	}
	info := ConnectInfo{
		Version:         envelope.Data.Module.Version,
		UpstreamVersion: envelope.Data.Module.UpstreamVersion,
		Handoff:         envelope.Data.Module.Handoff,
		Platforms:       envelope.Data.Platforms,
	}
	if info.Platforms == nil {
		info.Platforms = []ConnectPlatform{}
	}
	return info, nil
}
