package loginbridge

import (
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

func TestMigrateIdempotent(t *testing.T) {
	if err := Migrate(); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestAppSaveGetListDelete(t *testing.T) {
	platform := "app-save"
	check := func(app loginsdk.App) error { return nil }

	saved, err := SaveApp(AppInput{
		Platform:     "  " + platform + "  ",
		Mode:         " own_app ",
		ClientID:     " cid-1 ",
		ClientSecret: "secret-one",
		RedirectURI:  " http://x/cb ",
	}, check)
	if err != nil {
		t.Fatalf("SaveApp: %v", err)
	}
	if saved.Platform != platform || saved.Mode != loginsdk.AppOwn ||
		saved.ClientID != "cid-1" || saved.ClientSecret != "secret-one" ||
		saved.RedirectURI != "http://x/cb" {
		t.Fatalf("SaveApp did not trim fields: %+v", saved)
	}
	if saved.Revision != 1 {
		t.Fatalf("first Revision = %d, want 1", saved.Revision)
	}

	got, found, err := GetApp(platform)
	if err != nil || !found {
		t.Fatalf("GetApp: found=%v err=%v", found, err)
	}
	if got.ClientSecret != "secret-one" || got.Revision != 1 {
		t.Fatalf("GetApp returned %+v", got)
	}

	// An empty secret keeps the stored one of the same app and bumps the
	// revision.
	updated, err := SaveApp(AppInput{
		Platform:    platform,
		Mode:        "own_app",
		ClientID:    "cid-1",
		RedirectURI: "http://y/cb",
	}, check)
	if err != nil {
		t.Fatalf("SaveApp update: %v", err)
	}
	if updated.ClientSecret != "secret-one" || updated.RedirectURI != "http://y/cb" {
		t.Fatalf("empty ClientSecret lost the stored secret: %+v", updated)
	}
	if updated.Revision != 2 {
		t.Fatalf("Revision = %d, want 2", updated.Revision)
	}

	// A different client_id or mode never inherits the previous app's secret.
	var checked []loginsdk.App
	record := func(app loginsdk.App) error {
		checked = append(checked, app)
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "请填写 client_secret")
	}
	if _, err := SaveApp(AppInput{Platform: platform, Mode: "own_app", ClientID: "cid-other"}, record); err == nil {
		t.Fatal("SaveApp accepted a rejected app")
	}
	if _, err := SaveApp(AppInput{Platform: platform, Mode: "online_service", ClientID: "cid-1", ServiceURL: "https://svc"}, record); err == nil {
		t.Fatal("SaveApp accepted a rejected app")
	}
	for _, app := range checked {
		if app.ClientSecret != "" {
			t.Fatalf("app %+v inherited the previous app's secret", app)
		}
	}

	// Changing the client_id with a new secret is a normal update.
	updated, err = SaveApp(AppInput{
		Platform:     platform,
		Mode:         "own_app",
		ClientID:     "cid-2",
		ClientSecret: "secret-one",
	}, check)
	if err != nil {
		t.Fatalf("SaveApp update: %v", err)
	}
	if updated.Revision != 3 {
		t.Fatalf("Revision = %d, want 3", updated.Revision)
	}

	// A rejected check writes nothing.
	reject := func(app loginsdk.App) error {
		return loginsdk.Errorf(loginsdk.CodeAppInvalid, "缺少 redirect_uri")
	}
	if _, err := SaveApp(AppInput{Platform: platform, Mode: "own_app", ClientID: "cid-3"}, reject); err == nil {
		t.Fatal("SaveApp accepted a rejected app")
	}
	after, _, err := GetApp(platform)
	if err != nil {
		t.Fatalf("GetApp after rejection: %v", err)
	}
	if after.ClientID != "cid-2" || after.Revision != 3 {
		t.Fatalf("rejected SaveApp changed the stored profile: %+v", after)
	}

	apps, err := ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	foundInList := false
	for _, app := range apps {
		if app.Platform == platform {
			foundInList = true
			if app.ClientSecret != "secret-one" {
				t.Fatalf("ListApps returned %+v", app)
			}
		}
	}
	if !foundInList {
		t.Fatalf("ListApps does not contain %s: %+v", platform, apps)
	}

	if err := DeleteApp(platform); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	_, found, err = GetApp(platform)
	if err != nil || found {
		t.Fatalf("GetApp after delete: found=%v err=%v", found, err)
	}
	// Deleting a missing profile is not an error.
	if err := DeleteApp(platform); err != nil {
		t.Fatalf("DeleteApp of missing profile: %v", err)
	}
}

func TestGetAppMissing(t *testing.T) {
	_, found, err := GetApp("app-never-configured")
	if err != nil {
		t.Fatalf("GetApp missing: %v", err)
	}
	if found {
		t.Fatal("GetApp found a profile that was never saved")
	}
}

func TestScrubMessageCoversAppSecret(t *testing.T) {
	app := loginsdk.App{ClientSecret: "app-secret-value"}
	auth := &loginsdk.AuthConfig{Secrets: []string{"tok-secret-value"}}
	out := scrubMessage("tok-secret-value and app-secret-value", app, auth)
	if strings.Contains(out, "tok-secret-value") || strings.Contains(out, "app-secret-value") {
		t.Fatalf("scrubMessage leaked: %q", out)
	}
}
