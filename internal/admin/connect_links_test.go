package admin

// Login-module (openlist-patch connect) link tests: drive rows and the
// overview expose reauth/connect links only when the module answered the probe,
// the openlist_public_url base is valid and the matched storage's driver has a
// platform; the probe never fails a request, its result is cached per TTL
// (sync forces a refresh), and cloud2api stays read-only throughout (zero
// storage writes, no connect app fields in any response).

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// setConnect fakes the connect-module state: a platform list (nil = the
// unpatched SPA), or a real probe failure.
func setConnect(t *testing.T, h *portalHarness, platforms []fakeConnectPlatform, fail bool) {
	t.Helper()
	h.fake.mu.Lock()
	h.fake.connectPlatforms = platforms
	h.fake.connectFail = fail
	h.fake.mu.Unlock()
}

func setPublicURL(t *testing.T, h *portalHarness, v string) {
	t.Helper()
	if err := h.st.SetSetting("openlist_public_url", v); err != nil {
		t.Fatal(err)
	}
}

var testConnectPlatforms = []fakeConnectPlatform{
	{ID: "baidu", Name: "百度网盘", Driver: "BaiduNetdisk", Method: "oauth",
		App: map[string]any{"required": true, "mode": "own_app", "client_id": "SECRET-APP-ID", "client_secret_set": true}},
	{ID: "pan123", Name: "123云盘", Driver: "Pan123", Method: "qr"},
}

// TestDriveRowsReauthURL: with the module present and a valid base, a matched
// storage whose driver has a platform links to /@connect/#/reauth/<id>; a
// Local-driver drive (no platform), an orphaned drive and the module-absent
// fake all keep "" — while openlist_edit_url stays exactly as before.
func TestDriveRowsReauthURL(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setPublicURL(t, h, "http://192.0.2.10:5244/")
	setConnect(t, h, testConnectPlatforms, false)

	bd := seedDrive(t, h, "bd", "/d/bd", "BaiduNetdisk")
	seedDrive(t, h, "loc", "/d/loc", "Local")
	orph := seedDrive(t, h, "orph", "/d/orph", "BaiduNetdisk")
	removeFakeStorage(t, h, orph)

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives = %d (%s)", rec.Code, rec.Body.String())
	}
	// The connect app configuration must not leak into the response.
	if body := rec.Body.String(); strings.Contains(body, "SECRET-APP-ID") || strings.Contains(body, "client_secret") {
		t.Errorf("drives response leaked connect app fields: %s", body)
	}
	doc := dataOf(t, decodePortal(t, rec))
	row := driveRowOf(t, doc, "bd")
	want := "http://192.0.2.10:5244/@connect/#/reauth/" + strconv.FormatInt(bd, 10)
	if row["openlist_reauth_url"] != want {
		t.Errorf("bd openlist_reauth_url = %v, want %q", row["openlist_reauth_url"], want)
	}
	wantEdit := "http://192.0.2.10:5244/@manage/storages/edit/" + strconv.FormatInt(bd, 10)
	if row["openlist_edit_url"] != wantEdit {
		t.Errorf("bd openlist_edit_url = %v, want %q (unchanged native fallback)", row["openlist_edit_url"], wantEdit)
	}
	if got := driveRowOf(t, doc, "loc")["openlist_reauth_url"]; got != "" {
		t.Errorf("loc (Local driver, no platform) openlist_reauth_url = %v, want \"\"", got)
	}
	if got := driveRowOf(t, doc, "orph")["openlist_reauth_url"]; got != "" {
		t.Errorf("orph (orphaned) openlist_reauth_url = %v, want \"\"", got)
	}

	// Sync reuses its one forced probe for the rows it returns: same links.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d (%s)", rec.Code, rec.Body.String())
	}
	doc = dataOf(t, decodePortal(t, rec))
	if got := driveRowOf(t, doc, "bd")["openlist_reauth_url"]; got != want {
		t.Errorf("sync bd openlist_reauth_url = %v, want %q", got, want)
	}
	if got := driveRowOf(t, doc, "loc")["openlist_reauth_url"]; got != "" {
		t.Errorf("sync loc openlist_reauth_url = %v, want \"\"", got)
	}

	// Enable/disable rows carry the link too, and the module state survives
	// the switch (cloud2api's own switch never touches OpenList).
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/bd/disable", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d (%s)", rec.Code, rec.Body.String())
	}
	if row := dataOf(t, decodePortal(t, rec)); row["openlist_reauth_url"] != want {
		t.Errorf("disable row openlist_reauth_url = %v, want %q", row["openlist_reauth_url"], want)
	}

	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("connect linking produced %d OpenList storage writes, want 0", got)
	}
}

// TestDriveRowsReauthURLModuleAbsent: the unpatched fake (SPA HTML) keeps every
// reauth link off while the native edit link stays.
func TestDriveRowsReauthURLModuleAbsent(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setPublicURL(t, h, "http://192.0.2.10:5244")
	seedDrive(t, h, "bd", "/d/bd", "BaiduNetdisk")

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	row := driveRowOf(t, doc, "bd")
	if got := row["openlist_reauth_url"]; got != "" {
		t.Errorf("absent module openlist_reauth_url = %v, want \"\"", got)
	}
	if row["openlist_edit_url"] == "" {
		t.Error("absent module dropped openlist_edit_url, want the native editor link")
	}
	if got := h.fake.connectCallCount(); got == 0 {
		t.Error("the probe never ran against the module-absent fake")
	}
	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("module-absent listing produced %d storage writes, want 0", got)
	}
}

// TestOverviewConnectFields covers the overview openlist object: availability,
// the entry link (only with a valid base), the module version, the platform
// identity list and the absent/unset cases.
func TestOverviewConnectFields(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setConnect(t, h, testConnectPlatforms, false)

	// No base configured: available, platforms listed, but no link.
	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "SECRET-APP-ID") || strings.Contains(body, "client_secret") {
		t.Errorf("overview leaked connect app fields: %s", body)
	}
	doc := dataOf(t, decodePortal(t, rec))
	ol := doc["openlist"].(map[string]any)
	if ol["connect_available"] != true {
		t.Errorf("connect_available = %v, want true", ol["connect_available"])
	}
	if ol["connect_url"] != "" {
		t.Errorf("connect_url without a base = %v, want \"\"", ol["connect_url"])
	}
	if ol["connect_version"] != "1" {
		t.Errorf("connect_version = %v, want 1", ol["connect_version"])
	}
	if ol["connect_error"] != "" {
		t.Errorf("connect_error = %v, want \"\"", ol["connect_error"])
	}
	assertPlatformRows(t, ol["connect_platforms"])

	// A valid base turns the link on; still no warning.
	setPublicURL(t, h, "http://192.0.2.10:5244/")
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	ol = dataOf(t, decodePortal(t, rec))["openlist"].(map[string]any)
	if ol["connect_url"] != "http://192.0.2.10:5244/@connect/" {
		t.Errorf("connect_url = %v, want the @connect entry", ol["connect_url"])
	}

	// An invalid stored base keeps the link off.
	setPublicURL(t, h, "http://u:p@host/x")
	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	ol = dataOf(t, decodePortal(t, rec))["openlist"].(map[string]any)
	if ol["connect_url"] != "" {
		t.Errorf("connect_url with an invalid base = %v, want \"\"", ol["connect_url"])
	}
	if ol["connect_available"] != true {
		t.Errorf("connect_available with an invalid base = %v, want true", ol["connect_available"])
	}
}

// assertPlatformRows checks the connect_platforms shape: one {id,name,driver}
// row per platform, no method and no app fields.
func assertPlatformRows(t *testing.T, raw any) {
	t.Helper()
	pls, ok := raw.([]any)
	if !ok || len(pls) != 2 {
		t.Fatalf("connect_platforms = %#v, want the two platform rows", raw)
	}
	first := pls[0].(map[string]any)
	if first["id"] != "baidu" || first["name"] != "百度网盘" || first["driver"] != "BaiduNetdisk" {
		t.Errorf("connect_platforms[0] = %v", first)
	}
	for _, field := range []string{"method", "app", "app_modes", "scopes"} {
		if _, has := first[field]; has {
			t.Errorf("connect_platforms row carries %q, want id/name/driver only", field)
		}
	}
}

// TestOverviewConnectModuleAbsent: connect_available=false, empty link/version,
// an empty platform array and no warning about the module.
func TestOverviewConnectModuleAbsent(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setPublicURL(t, h, "http://192.0.2.10:5244")

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	ol := doc["openlist"].(map[string]any)
	if ol["connect_available"] != false {
		t.Errorf("connect_available = %v, want false", ol["connect_available"])
	}
	if ol["connect_url"] != "" || ol["connect_version"] != "" || ol["connect_error"] != "" {
		t.Errorf("absent module connect fields = %v/%v/%v, want \"\"s", ol["connect_url"], ol["connect_version"], ol["connect_error"])
	}
	if pls, ok := ol["connect_platforms"].([]any); !ok || len(pls) != 0 {
		t.Errorf("connect_platforms = %#v, want an empty array", ol["connect_platforms"])
	}
	for _, raw := range doc["warnings"].([]any) {
		if w := raw.(map[string]any); strings.Contains(w["text"].(string), "登录模块") {
			t.Errorf("absent module must not warn: %v", w)
		}
	}
}

// TestConnectProbeFailureDegrades: a really failing probe still leaves the
// listings, sync and enable/disable at 200 (rows without reauth links) and only
// surfaces as connect_error plus one warn warning on the overview.
func TestConnectProbeFailureDegrades(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setPublicURL(t, h, "http://192.0.2.10:5244")
	setConnect(t, h, nil, true)
	seedDrive(t, h, "bd", "/d/bd", "BaiduNetdisk")

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives under a failed probe = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got := driveRowOf(t, dataOf(t, decodePortal(t, rec)), "bd")["openlist_reauth_url"]; got != "" {
		t.Errorf("openlist_reauth_url under a failed probe = %v, want \"\"", got)
	}

	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync under a failed probe = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview under a failed probe = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	doc := dataOf(t, decodePortal(t, rec))
	ol := doc["openlist"].(map[string]any)
	if ol["connect_available"] != false {
		t.Errorf("connect_available = %v, want false", ol["connect_available"])
	}
	if ol["connect_error"] == "" {
		t.Error("connect_error empty for a real probe failure")
	}
	found := false
	for _, raw := range doc["warnings"].([]any) {
		w := raw.(map[string]any)
		if w["level"] == "warn" && w["action_url"] == "#/openlist" &&
			strings.HasPrefix(w["text"].(string), "OpenList 登录模块状态读取失败：") {
			found = true
		}
	}
	if !found {
		t.Errorf("no probe-failure warning in %v", doc["warnings"])
	}

	for _, op := range []string{"disable", "enable"} {
		rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/bd/"+op, cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s under a failed probe = %d (%s), want 200", op, rec.Code, rec.Body.String())
		}
	}

	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("probe-failure scenario produced %d storage writes, want 0", got)
	}
}

// TestConnectProbeCachedPerTTL: two listings within the TTL share one probe
// (one storage list each), the overview reuses it too, sync forces a fresh
// probe and its result is cached again.
func TestConnectProbeCachedPerTTL(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	setConnect(t, h, testConnectPlatforms, false)
	seedDrive(t, h, "bd", "/d/bd", "BaiduNetdisk")

	for i := 0; i < 2; i++ {
		rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("drives #%d = %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := h.fake.listCallCount(); got != 2 {
		t.Errorf("storage list calls = %d, want 2 (one per listing)", got)
	}
	if got := h.fake.connectCallCount(); got != 1 {
		t.Errorf("connect probes for two listings = %d, want 1 (cached within the TTL)", got)
	}

	rec := doPortal(t, h.handler, http.MethodGet, "/admin/api/overview", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := h.fake.connectCallCount(); got != 1 {
		t.Errorf("connect probes after the overview = %d, want still 1", got)
	}

	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/drives/sync", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := h.fake.connectCallCount(); got != 2 {
		t.Errorf("connect probes after sync = %d, want 2 (sync forces a refresh)", got)
	}

	rec = doPortal(t, h.handler, http.MethodGet, "/admin/api/drives", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("drives after sync = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := h.fake.connectCallCount(); got != 2 {
		t.Errorf("connect probes after sync + listing = %d, want still 2", got)
	}

	if got := h.fake.writeCallCount(); got != 0 {
		t.Fatalf("caching scenario produced %d storage writes, want 0", got)
	}
}
