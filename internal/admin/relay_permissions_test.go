package admin

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/store"
)

// TestRemovedRequestRoutes covers the deleted agent-approval module: both the
// admin endpoints and the mutation they used to perform are gone.
func TestRemovedRequestRoutes(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	if _, _, err := h.keys.Create(store.APIKey{
		ID:     "target",
		Drives: []store.KeyDrive{{Drive: "static1", Root: "/", Perms: []string{"list", "read"}}},
		Trash:  store.TrashSettings{Enabled: true, Days: 7},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/admin/api/requests"},
		{http.MethodGet, "/admin/api/requests?state=pending"},
		{http.MethodPost, "/admin/api/requests/1/approve"},
		{http.MethodPost, "/admin/api/requests/1/deny"},
	} {
		rec := doPortal(t, h.handler, tc.method, tc.target, cookie, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d (%s), want 404", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
	// No endpoint can change the key's grants.
	k, err := h.keys.Get("target")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Drives) != 1 || k.Drives[0].Drive != "static1" || len(k.Drives[0].Perms) != 2 {
		t.Fatalf("grants mutated: %+v", k.Drives)
	}
}

// addSecondDrive registers one more managed drive for cross-drive tests.
func addSecondDrive(t *testing.T, h *portalHarness, id string) {
	t.Helper()
	if err := h.st.InsertDrive(store.Drive{
		ID: id, Provider: "local", Driver: "Local", MountPath: "/m-" + id,
		Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestKeyCrossDriveRequiresDefaultDrive pins the grant constraints: a NEW
// cross-drive key must name its default drive, PATCHes that change the drive
// set or default must leave a valid cross-drive result, and single-drive keys
// keep working without one (legacy compatibility).
func TestKeyCrossDriveRequiresDefaultDrive(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)
	addSecondDrive(t, h, "second")

	drives := func(ids ...string) []map[string]any {
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"drive": id, "perms": []string{"list", "read"}})
		}
		return out
	}

	// Cross-drive create without default_drive -> 400, nothing stored.
	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "xd-nodflt", "drives": drives("static1", "second"),
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "default_drive") {
		t.Fatalf("cross-drive no default = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := h.keys.Get("xd-nodflt"); err == nil {
		t.Fatal("rejected key was stored")
	}

	// Cross-drive create with default among the grants -> 200.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "xd-ok", "default_drive": "second", "drives": drives("static1", "second"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-drive ok = %d (%s)", rec.Code, rec.Body.String())
	}

	// Single drive without default stays valid (legacy shape preserved).
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "single-ok", "drives": drives("static1"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("single no default = %d (%s)", rec.Code, rec.Body.String())
	}

	// PATCH to cross-drive without a default -> 400 and the stored key keeps
	// its previous grants.
	rec = doPortal(t, h.handler, http.MethodPatch, "/admin/api/keys/single-ok", cookie, map[string]any{
		"drives": drives("static1", "second"),
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "default_drive") {
		t.Fatalf("patch cross-drive = %d (%s)", rec.Code, rec.Body.String())
	}
	k, _ := h.keys.Get("single-ok")
	if len(k.Drives) != 1 || k.Drives[0].Drive != "static1" {
		t.Fatalf("rejected patch mutated the store: %+v", k.Drives)
	}

	// Clearing the default of a cross-drive key -> 400.
	rec = doPortal(t, h.handler, http.MethodPatch, "/admin/api/keys/xd-ok", cookie, map[string]any{
		"default_drive": "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clear default of cross-drive = %d (%s)", rec.Code, rec.Body.String())
	}

	// An unrelated PATCH on the cross-drive key works without forcing a
	// default (nothing about the drive set changes).
	rec = doPortal(t, h.handler, http.MethodPatch, "/admin/api/keys/xd-ok", cookie, map[string]any{
		"trash": map[string]any{"enabled": false, "days": 3},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated patch = %d (%s)", rec.Code, rec.Body.String())
	}

	// default_drive outside the grants keeps its existing rejection.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "xd-bad", "default_drive": "ghost", "drives": drives("static1", "second"),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("default outside grants = %d", rec.Code)
	}

	// Root hygiene: "/" is fine, escapes are not.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "root-ok", "drives": []map[string]any{{"drive": "static1", "root": "/", "perms": []string{"read"}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("root / = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "root-bad", "drives": []map[string]any{{"drive": "static1", "root": "/../escape", "perms": []string{"read"}}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("root escape = %d", rec.Code)
	}
}

// TestNoImplicitFutureDriveRights proves that a key grants exactly the drives
// it was created with: newly managed drives never leak into existing keys.
func TestNoImplicitFutureDriveRights(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.login(t, adminPassword)

	rec := doPortal(t, h.handler, http.MethodPost, "/admin/api/keys", cookie, map[string]any{
		"id": "scoped", "drives": []map[string]any{{"drive": "static1", "perms": []string{"list", "read"}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}

	addSecondDrive(t, h, "future")
	k, err := h.keys.Get("scoped")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Drives) != 1 || k.Drives[0].Drive != "static1" || k.DefaultDrive != "" {
		t.Fatalf("new drive leaked into the key: %+v (default=%q)", k.Drives, k.DefaultDrive)
	}
}

// TestRelayPageServing covers the public restricted /relay/ page: GET only,
// no session required, hardened headers, assets from the embedded web tree.
func TestRelayPageServing(t *testing.T) {
	h := newPortalHarness(t)
	h.p.relayPage = []byte("<!doctype html><html><body>relay</body></html>")

	// No session cookie anywhere.
	for _, target := range []string{"/relay", "/relay/"} {
		rec := doPortal(t, h.handler, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d (%s)", target, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s content-type = %q", target, ct)
		}
		for header, want := range map[string]string{
			"Cache-Control":           "no-store",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "default-src 'none'",
		} {
			if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
				t.Errorf("%s %s = %q, want prefix %q", target, header, got, want)
			}
		}
	}

	// Assets serve from the embedded tree without a session (relay.js/relay.css
	// are frontend-owned; app.css proves the path).
	rec := doPortal(t, h.handler, http.MethodGet, "/relay/assets/app.css", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("asset = %d (%s) ct=%q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("asset lacks nosniff")
	}

	// Only GET is allowed; traversal and non-asset paths are refused.
	if rec := doPortal(t, h.handler, http.MethodPost, "/relay/", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /relay/ = %d, want 405", rec.Code)
	}
	if rec := doPortal(t, h.handler, http.MethodGet, "/relay/assets/../app.html", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("traversal = %d, want 400", rec.Code)
	}
	if rec := doPortal(t, h.handler, http.MethodGet, "/relay/assets/nope.js", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}
	if rec := doPortal(t, h.handler, http.MethodGet, "/relay/other", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("other path = %d, want 404", rec.Code)
	}

	// The admin login stays password-only: an API key does not authenticate
	// against any admin endpoint.
	rec = doPortal(t, h.handler, http.MethodPost, "/admin/api/login", "", map[string]any{"password": dataPlaneKey})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("key-as-password login = %d, want 401", rec.Code)
	}
}
