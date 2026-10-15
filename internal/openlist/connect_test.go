package openlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// connectPayload mirrors the patched OpenList's GET /api/admin/connect/platforms
// response (openlist-patch docs §平台与应用配置), including the app fields with
// a recognizable secret: cloud2api must not retain them.
const connectPayload = `{
  "code": 200, "message": "success",
  "data": {
    "module": {"name": "connect", "version": "1", "upstream_version": "v4.2.6",
               "upstream_commit": "2bdf16d", "web_version": "v4.2.6"},
    "callback_path": "/api/connect/oauth/callback",
    "suggested_redirect_uri": "",
    "platforms": [{
      "id": "baidu", "name": "百度网盘", "driver": "BaiduNetdisk", "method": "oauth",
      "app_modes": ["own_app"], "scopes": ["basic", "netdisk"], "pkce": false,
      "reauth_fields": ["refresh_token"],
      "app": {"required": true, "configured": true, "mode": "own_app",
              "client_id": "SECRET-CLIENT-ID", "client_secret_set": true,
              "redirect_uri": "http://192.0.2.10:5244/api/connect/oauth/callback"}
    }, {
      "id": "pan123", "name": "123云盘", "driver": "Pan123", "method": "qr",
      "app": {"required": false, "configured": true, "mode": "none"}
    }]
  }
}`

// newConnectClient serves one canned response body for every request; calls
// counts how many were made.
func newConnectClient(t *testing.T, status int, contentType, body string) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	c, err := New(ts.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestConnectInfoParsesModuleAndPlatforms(t *testing.T) {
	c, _ := newConnectClient(t, http.StatusOK, "application/json", connectPayload)
	info, err := c.ConnectInfo(context.Background())
	if err != nil {
		t.Fatalf("ConnectInfo: %v", err)
	}
	if info.Version != "1" || info.UpstreamVersion != "v4.2.6" {
		t.Errorf("module identity = %+v, want version 1 / upstream v4.2.6", info)
	}
	if len(info.Platforms) != 2 {
		t.Fatalf("platforms = %+v, want 2", info.Platforms)
	}
	p := info.Platforms[0]
	if p.ID != "baidu" || p.Name != "百度网盘" || p.Driver != "BaiduNetdisk" || p.Method != "oauth" {
		t.Errorf("platform[0] = %+v", p)
	}
	if !info.SupportsDriver("BaiduNetdisk") || !info.SupportsDriver("Pan123") || info.SupportsDriver("Local") {
		t.Errorf("SupportsDriver disagrees with the platform list: %+v", info.Platforms)
	}

	// The app configuration never survives the decode …
	blob, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"SECRET-CLIENT-ID", "client_secret_set", "redirect_uri", "refresh_token"} {
		if strings.Contains(string(blob), needle) {
			t.Errorf("ConnectInfo retained %q: %s", needle, blob)
		}
	}
	// … because the decode structs have no fields for it: only the identity
	// subset exists, so nothing else is even unmarshalled.
	if n := reflect.TypeOf(p).NumField(); n != 4 {
		t.Errorf("ConnectPlatform has %d fields, want exactly id/name/driver/method", n)
	}
	if n := reflect.TypeOf(info).NumField(); n != 4 {
		t.Errorf("ConnectInfo has %d fields, want exactly version/upstream_version/handoff/platforms", n)
	}
}

func TestConnectInfoSPAHTMLIsUnavailable(t *testing.T) {
	// An unpatched OpenList answers the unknown API path with its SPA
	// index.html (HTTP 200, text/html): module not available, not an error.
	c, _ := newConnectClient(t, http.StatusOK, "text/html; charset=utf-8",
		"<!doctype html><html><body>OpenList SPA</body></html>")
	_, err := c.ConnectInfo(context.Background())
	if !errors.Is(err, ErrConnectUnavailable) {
		t.Fatalf("SPA HTML err = %v, want ErrConnectUnavailable", err)
	}
}

func TestConnectInfoEnvelope404IsUnavailable(t *testing.T) {
	c, _ := newConnectClient(t, http.StatusOK, "application/json", `{"code":404,"message":"not found"}`)
	_, err := c.ConnectInfo(context.Background())
	if !errors.Is(err, ErrConnectUnavailable) {
		t.Fatalf("envelope 404 err = %v, want ErrConnectUnavailable", err)
	}
}

func TestConnectInfoEnvelope500Fails(t *testing.T) {
	c, _ := newConnectClient(t, http.StatusOK, "application/json", `{"code":500,"message":"broken"}`)
	_, err := c.ConnectInfo(context.Background())
	if err == nil || errors.Is(err, ErrConnectUnavailable) {
		t.Fatalf("envelope 500 err = %v, want an ordinary error", err)
	}
	if !strings.Contains(err.Error(), "code=500") || !strings.Contains(err.Error(), "broken") {
		t.Errorf("envelope 500 err = %v, want code and message in it", err)
	}
}

func TestConnectInfoEmptyPlatformList(t *testing.T) {
	c, _ := newConnectClient(t, http.StatusOK, "application/json",
		`{"code":200,"message":"success","data":{"module":{"version":"1","upstream_version":"v4.2.6"},"platforms":[]}}`)
	info, err := c.ConnectInfo(context.Background())
	if err != nil {
		t.Fatalf("ConnectInfo: %v", err)
	}
	if info.Platforms == nil || len(info.Platforms) != 0 {
		t.Errorf("platforms = %#v, want an empty non-nil slice", info.Platforms)
	}
}
