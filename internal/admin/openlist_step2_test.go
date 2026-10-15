package admin

// Step-2 tests (IMPLEMENTATION-openlist-entry.md 第二步): removed endpoints
// answer 404, portal write operations never write OpenList storage config,
// the drive row separates cloud2api's switch from the OpenList-side disabled
// flag, settings no longer manage oplist_api_base, and no portal response
// leaks storage credentials.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRemovedEndpointsAnswer404: every endpoint of the deleted wizard /
// settings / creation surface must answer 404 {"code":404,"message":"not
// found"} (IMPLEMENTATION §实施步骤 2.1).
func TestRemovedEndpointsAnswer404(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	seedDrive(t, h, "dyn", "/dyn", "Local")

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"create drive", http.MethodPost, "/admin/api/drives"},
		{"create account (legacy)", http.MethodPost, "/admin/api/accounts"},
		{"get drive settings", http.MethodGet, "/admin/api/drives/dyn/settings"},
		{"put drive settings", http.MethodPut, "/admin/api/drives/dyn/settings"},
		{"get account settings (legacy)", http.MethodGet, "/admin/api/accounts/dyn/settings"},
		{"providers", http.MethodGet, "/admin/api/providers"},
		{"start login flow", http.MethodPost, "/admin/api/login-flows"},
		{"poll login flow", http.MethodGet, "/admin/api/login-flows/abc"},
		{"complete login flow", http.MethodPost, "/admin/api/login-flows/abc/complete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body any
			if tc.method == http.MethodPost || tc.method == http.MethodPut {
				body = map[string]any{}
			}
			rec := doPortal(t, h.handler, tc.method, tc.target, cookie, body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d (%s), want 404", tc.method, tc.target, rec.Code, rec.Body.String())
			}
			m := decodePortal(t, rec)
			if m["code"] != float64(404) || m["message"] != "not found" {
				t.Fatalf("body = %v, want {code:404,message:\"not found\"}", m)
			}
		})
	}
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("removed endpoints produced %d storage writes, want 0", got)
	}
}

// TestDriveRowStorageDisabledFromOpenList: a storage disabled on the OpenList
// side (while cloud2api's own switch stays on) shows enabled=true,
// storage_disabled=true and state disabled; the cloud2api-side disable of
// task 5 is covered by TestEnableDisableOnlyFlipsCloud2apiSwitch.
func TestDriveRowStorageDisabledFromOpenList(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	sid := seedDrive(t, h, "dyn", "/dyn", "Local")

	h.fake.mu.Lock()
	for i := range h.fake.storages {
		if h.fake.storages[i].ID == sid {
			h.fake.storages[i].Disabled = true
			h.fake.storages[i].Status = "disabled"
		}
	}
	h.fake.mu.Unlock()

	for _, target := range []string{"/admin/api/drives", "/admin/api/drives/sync"} {
		method := http.MethodGet
		if target == "/admin/api/drives/sync" {
			method = http.MethodPost
		}
		rec := doPortal(t, h.handler, method, target, cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d (%s)", target, rec.Code, rec.Body.String())
		}
		doc := dataOf(t, decodePortal(t, rec))
		row := driveRowOf(t, doc, "dyn")
		if row["enabled"] != true {
			t.Errorf("%s: enabled = %v, want true (cloud2api switch untouched)", target, row["enabled"])
		}
		if row["storage_disabled"] != true {
			t.Errorf("%s: storage_disabled = %v, want true (OpenList side)", target, row["storage_disabled"])
		}
		if row["state"] != "disabled" {
			t.Errorf("%s: state = %v, want disabled", target, row["state"])
		}
	}
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("listings produced %d storage writes, want 0", got)
	}
}

// TestSettingsOplistAPIBaseRemoved: oplist_api_base is no longer a setting —
// PUT with it answers 400 naming the key, GET never returns it.
func TestSettingsOplistAPIBaseRemoved(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodPut, "/admin/api/settings", cookie, map[string]any{
		"oplist_api_base": "https://api-cn.oplist.org",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT oplist_api_base = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if msg := fmt.Sprint(decodePortal(t, rec)["message"]); !strings.Contains(msg, "oplist_api_base") {
		t.Fatalf("400 message = %v, want it to name oplist_api_base", msg)
	}
	if _, ok := h.st.GetSetting("oplist_api_base"); ok {
		t.Error("oplist_api_base was persisted")
	}

	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/settings", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d", rec.Code)
	}
	if _, still := dataOf(t, decodePortal(t, rec))["oplist_api_base"]; still {
		t.Fatal("GET settings still lists oplist_api_base")
	}
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("settings produced %d storage writes, want 0", got)
	}
}

// TestResponsesNeverLeakStorageCredentials: a recognizable secret inside a
// storage's addition must never appear in any portal response — nor its field
// name (IMPLEMENTATION §安全: 门户响应、日志和事件只出现驱动名、挂载点、状态和
// ID).
func TestResponsesNeverLeakStorageCredentials(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	const secretAddition = `{"refresh_token":"SECRET-XYZ","root_folder_path":"/mnt/x"}`

	// Two managed storages carrying the secret: one healthy, one whose
	// recorded storage is gone (rebind candidate below).
	old := seedDrive(t, h, "sec1", "/d/sec", "BaiduNetdisk")
	h.fake.mu.Lock()
	for i := range h.fake.storages {
		if h.fake.storages[i].ID == old {
			h.fake.storages[i].Addition = secretAddition
		}
	}
	h.fake.storages = append(h.fake.storages, fakeStorage{
		ID: 4242, MountPath: "/d/sec", Driver: "Onedrive", Status: "work",
		Addition: secretAddition, Modified: "2026-10-13T00:00:00Z",
	})
	h.fake.mu.Unlock()
	// Drop the original so 4242 becomes sec1's replacement candidate.
	removeFakeStorage(t, h, old)

	responses := map[string]*httptest.ResponseRecorder{}
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	responses["GET /drives"] = rec
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	responses["POST /drives/sync"] = rec
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives/unmanaged", cookie, nil)
	responses["GET /drives/unmanaged"] = rec

	// Adopt a fresh secret-carrying storage.
	h.fake.mu.Lock()
	h.fake.storages = append(h.fake.storages, fakeStorage{
		ID: 4243, MountPath: "/d/adopt", Driver: "Quark", Status: "work",
		Addition: secretAddition, Modified: "2026-10-13T00:00:00Z",
	})
	h.fake.mu.Unlock()
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/adopt", cookie, map[string]any{"storage_id": 4243, "id": "sec2"})
	responses["POST /drives/adopt"] = rec

	// Rebind sec1 onto the replacement candidate 4242.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sec1/rebind", cookie, map[string]any{"storage_id": 4242})
	responses["POST /drives/sec1/rebind"] = rec

	// Events and the overview must stay clean too.
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/events", cookie, nil)
	responses["GET /events"] = rec
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	responses["GET /overview"] = rec

	for name, rec := range responses {
		if rec == nil || rec.Code != http.StatusOK {
			t.Fatalf("%s failed: %v", name, rec)
		}
		body := rec.Body.String()
		for _, needle := range []string{"SECRET-XYZ", "refresh_token"} {
			if strings.Contains(body, needle) {
				t.Errorf("%s leaked %q: %s", name, needle, body)
			}
		}
	}
}
