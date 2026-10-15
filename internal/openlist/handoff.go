package openlist

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
)

type ConnectHandoff struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"`
}

// StartConnectHandoff mints a one-use connect-only ticket. Neither the
// management token nor OpenList's upstream error text is returned to callers.
func (c *Client) StartConnectHandoff(ctx context.Context) (ConnectHandoff, error) {
	req, err := c.NewRequest(ctx, http.MethodPost,
		url.URL{Path: "/api/admin/connect/handoff/start"}, bytes.NewReader([]byte("{}")), 2,
		http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: request failed")
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: service unavailable")
	}
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode == http.StatusOK && mt == "text/html") {
		return ConnectHandoff{}, ErrConnectUnavailable
	}
	if resp.StatusCode != http.StatusOK || mt != "application/json" {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: invalid response (HTTP %d)", resp.StatusCode)
	}
	const limit = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: invalid response body")
	}
	var doc struct {
		Code *int           `json:"code"`
		Data ConnectHandoff `json:"data"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Code == nil {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: invalid response envelope")
	}
	if *doc.Code == http.StatusNotFound {
		return ConnectHandoff{}, ErrConnectUnavailable
	}
	if *doc.Code != http.StatusOK {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: rejected (code %d)", *doc.Code)
	}
	code, err := base64.RawURLEncoding.Strict().DecodeString(doc.Data.Code)
	if err != nil || len(code) != 32 || doc.Data.ExpiresIn < 1 || doc.Data.ExpiresIn > 60 {
		return ConnectHandoff{}, fmt.Errorf("openlist connect handoff: invalid ticket")
	}
	return doc.Data, nil
}
