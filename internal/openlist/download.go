package openlist

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxDownloadRedirects = 5

var downloadHeaderAllowlist = []string{
	"Range", "If-Range", "If-Modified-Since", "If-None-Match", "Accept", "User-Agent",
}

// DoDownload streams a signed /d download, following at most five redirects.
// Managed credentials never cross origins or return after leaving OpenList.
// Download payloads must not undergo DoAutoHeal's JSON envelope inspection.
func (c *Client) DoDownload(ctx context.Context, method string, u url.URL, header http.Header) (*http.Response, error) {
	if method != http.MethodGet && method != http.MethodHead {
		return nil, errors.New("openlist download: only GET and HEAD are supported")
	}
	u.Scheme = c.base.Scheme
	u.Host = c.base.Host
	u.User = nil
	if u.Opaque != "" {
		return nil, errors.New("openlist download: invalid request URL")
	}

	forwarded := allowlistDownloadHeaders(header)
	dst := u
	followed := 0
	healed := false
	leftOrigin := false
	for {
		req, err := c.downloadRequest(ctx, method, dst, forwarded, !leftOrigin)
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, &downloadError{err: err}
		}

		// Only an initial HTTP 401 may heal; a downloaded JSON file or a
		// provider rejection must never trigger an administrator login.
		if followed == 0 && !healed && resp.StatusCode == http.StatusUnauthorized && c.healingEnabled() {
			healed = true
			failed := req.Header.Get("Authorization")
			_ = resp.Body.Close()
			if err := c.heal(ctx, failed); err != nil {
				return nil, &downloadError{err: err}
			}
			continue
		}

		if downloadRedirectStatus(resp.StatusCode) {
			// Draining even a bounded byte count can wait indefinitely for a
			// peer that sends headers but withholds the redirect body.
			_ = resp.Body.Close()
			if followed >= maxDownloadRedirects {
				return nil, errors.New("openlist download: too many redirects")
			}
			next, err := resolveDownloadTarget(&dst, resp.Header.Get("Location"))
			if err != nil {
				return nil, err
			}
			if err := validateDownloadTarget(&dst, next); err != nil {
				return nil, err
			}
			if !downloadSameOrigin(next, c.base) {
				leftOrigin = true
			}
			dst = *next
			followed++
			continue
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
			_ = resp.Body.Close()
			return nil, errors.New("openlist download: redirect status is not followed")
		}
		return resp, nil
	}
}

func (c *Client) downloadRequest(ctx context.Context, method string, dst url.URL, forwarded http.Header, atOrigin bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, dst.String(), nil)
	if err != nil {
		return nil, errors.New("openlist download: invalid request URL")
	}
	req.Header = forwarded.Clone()
	if atOrigin {
		if hop := hopFromContext(ctx); hop > 0 {
			req.Header.Set(hopHeader, strconv.Itoa(hop))
		}
		req.Header.Set("Authorization", c.Token())
	}
	return req, nil
}

func allowlistDownloadHeaders(header http.Header) http.Header {
	out := make(http.Header, len(downloadHeaderAllowlist))
	for _, name := range downloadHeaderAllowlist {
		if values := header.Values(name); len(values) > 0 {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}

func downloadRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func resolveDownloadTarget(base *url.URL, location string) (*url.URL, error) {
	if strings.TrimSpace(location) == "" {
		return nil, errors.New("openlist download: redirect response has no Location")
	}
	ref, err := url.Parse(location)
	if err != nil {
		return nil, errors.New("openlist download: redirect Location is malformed")
	}
	return base.ResolveReference(ref), nil
}

func validateDownloadTarget(from *url.URL, next *url.URL) error {
	if next.Scheme != "http" && next.Scheme != "https" {
		return errors.New("openlist download: redirect target scheme is not http(s)")
	}
	if next.Host == "" {
		return errors.New("openlist download: redirect target has no host")
	}
	if next.User != nil {
		return errors.New("openlist download: redirect target carries credentials")
	}
	if from.Scheme == "https" && next.Scheme == "http" {
		return errors.New("openlist download: redirect must not downgrade https to http")
	}
	return nil
}

func downloadSameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// Transport errors can contain signed URLs; expose a fixed message while
// preserving errors.Is for cancellation and timeout handling.
type downloadError struct {
	err error
}

func (e *downloadError) Error() string { return "openlist download: upstream request failed" }

func (e *downloadError) Unwrap() error { return e.err }
