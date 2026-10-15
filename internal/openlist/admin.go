package openlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrStorageNotFound reports an OpenList storage id that no longer exists
// (GET /api/admin/storage/get answers {"code":500,"message":"record not found"}).
var ErrStorageNotFound = errors.New("openlist storage not found")

// StorageInfo is the subset of OpenList's storage model shared callers need:
// identity (id, mount, driver), health (status, disabled) and the linkage
// attributes read straight off the storage list. Note: OpenList exposes the
// negated flag `disabled` in its storage list. Addition carries the raw
// driver configuration JSON — it may contain credentials and is never
// serialized into any portal response.
type StorageInfo struct {
	ID              int64  `json:"id"`
	MountPath       string `json:"mount_path"`
	Driver          string `json:"driver"`
	Status          string `json:"status"`
	Disabled        bool   `json:"disabled"`
	Addition        string `json:"addition"`
	CacheExpiration any    `json:"cache_expiration"`
	Order           any    `json:"order"`
	Remark          any    `json:"remark"`
	Modified        string `json:"modified"`
}

// Enabled derives the positive flag from OpenList's `disabled`.
func (s StorageInfo) Enabled() bool { return !s.Disabled }

// Storage list paging (IMPLEMENTATION §存储列表读取): pages are requested
// explicitly so the result never depends on a server-side default page size.
const (
	storageListPerPage = 200
	storageListMaxPage = 1000
)

// ListStorages returns every storage registered in OpenList (admin API),
// reading /api/admin/storage/list in a loop until the total reported by the
// first page is collected. A partial list is never returned: an empty page
// before the total is reached, a total that changes between pages, a repeated
// storage id or an absurd page count all fail the call.
func (c *Client) ListStorages(ctx context.Context) ([]StorageInfo, error) {
	var collected []StorageInfo
	total := -1
	seen := map[int64]bool{}
	for page := 1; page <= storageListMaxPage; page++ {
		u := url.URL{Path: "/api/admin/storage/list",
			RawQuery: "page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(storageListPerPage)}
		req, err := c.NewRequest(ctx, http.MethodGet, u, nil, 0, http.Header{"Content-Type": []string{"application/json"}})
		if err != nil {
			return nil, err
		}
		resp, err := c.DoAutoHeal(req)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Content []StorageInfo `json:"content"`
				Total   int           `json:"total"`
			} `json:"data"`
		}
		err = func() error {
			defer resp.Body.Close()
			data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return fmt.Errorf("openlist returned non-JSON storage list (HTTP %d)", resp.StatusCode)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
		if envelope.Code != 0 && envelope.Code != http.StatusOK {
			return nil, fmt.Errorf("openlist storage list: code=%d message=%s", envelope.Code, envelope.Message)
		}
		if total == -1 {
			total = envelope.Data.Total
		} else if envelope.Data.Total != total {
			return nil, fmt.Errorf("openlist storage list total changed mid-read (page %d: %d, want %d)", page, envelope.Data.Total, total)
		}
		if len(envelope.Data.Content) == 0 {
			if len(collected) < total {
				return nil, fmt.Errorf("openlist storage list ended early (page %d: %d of %d storages)", page, len(collected), total)
			}
			break
		}
		for _, st := range envelope.Data.Content {
			if seen[st.ID] {
				return nil, fmt.Errorf("openlist storage list repeats storage %d", st.ID)
			}
			seen[st.ID] = true
			collected = append(collected, st)
		}
		if len(collected) >= total {
			break
		}
	}
	if len(collected) < total {
		return nil, fmt.Errorf("openlist storage list stopped at %d of %d storages", len(collected), total)
	}
	return collected, nil
}

// GetStorage reads one storage by id (admin API). A deleted id answers
// code 500 "record not found" and maps to ErrStorageNotFound; every other
// failure is an ordinary error.
func (c *Client) GetStorage(ctx context.Context, id int64) (StorageInfo, error) {
	u := url.URL{Path: "/api/admin/storage/get", RawQuery: "id=" + strconv.FormatInt(id, 10)}
	req, err := c.NewRequest(ctx, http.MethodGet, u, nil, 0, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return StorageInfo{}, err
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		return StorageInfo{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return StorageInfo{}, err
	}
	var envelope struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    StorageInfo `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return StorageInfo{}, fmt.Errorf("openlist returned non-JSON storage get (HTTP %d)", resp.StatusCode)
	}
	if envelope.Code != 0 && envelope.Code != http.StatusOK {
		msg := strings.ToLower(envelope.Message)
		if strings.Contains(msg, "record not found") || strings.Contains(msg, "storage not found") {
			return StorageInfo{}, fmt.Errorf("%w: storage %d", ErrStorageNotFound, id)
		}
		return StorageInfo{}, fmt.Errorf("openlist storage get: code=%d message=%s", envelope.Code, envelope.Message)
	}
	return envelope.Data, nil
}

// GuestEnabled probes the admin user list for the built-in guest account and
// reports whether it is usable (§3.7.6 security check).
func (c *Client) GuestEnabled(ctx context.Context) (bool, error) {
	u := url.URL{Path: "/api/admin/user/list"}
	req, err := c.NewRequest(ctx, http.MethodGet, u, nil, 0, http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return false, err
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Content []struct {
				Username string `json:"username"`
				Disabled bool   `json:"disabled"`
			} `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, fmt.Errorf("openlist returned non-JSON user list (HTTP %d)", resp.StatusCode)
	}
	if envelope.Code != 0 && envelope.Code != http.StatusOK {
		return false, fmt.Errorf("openlist user list: code=%d message=%s", envelope.Code, envelope.Message)
	}
	for _, u := range envelope.Data.Content {
		if u.Username == "guest" {
			return !u.Disabled, nil
		}
	}
	return false, nil
}

// PublicSettings is the subset of /api/public/settings used by /readyz and
// the admin overview.
type PublicSettings struct {
	Version string `json:"version"`
}

// GetPublicSettings reads the unauthenticated public settings.
func (c *Client) GetPublicSettings(ctx context.Context) (PublicSettings, error) {
	u := url.URL{Path: "/api/public/settings"}
	req, err := c.NewRequest(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return PublicSettings{}, err
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		return PublicSettings{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return PublicSettings{}, err
	}
	var envelope struct {
		Code int            `json:"code"`
		Data PublicSettings `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return PublicSettings{}, fmt.Errorf("openlist returned non-JSON public settings (HTTP %d)", resp.StatusCode)
	}
	return envelope.Data, nil
}
