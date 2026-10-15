package loginbridge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"gorm.io/gorm"
)

// ConnectApp persists one administrator-configured application profile per
// platform. OpenList's naming strategy with the default table prefix puts it
// in "x_connect_apps".
type ConnectApp struct {
	Platform     string `gorm:"primaryKey;size:64"`
	Mode         string `gorm:"size:32"`
	ClientID     string `gorm:"type:text"`
	ClientSecret string `gorm:"type:text"`
	RedirectURI  string `gorm:"type:text"`
	ServiceURL   string `gorm:"type:text"`
	Revision     int64
	UpdatedAt    time.Time
}

// Migrate creates or updates the app profile table; safe to call on every
// start-up.
func Migrate() error {
	return db.AutoMigrate(&ConnectApp{})
}

func appOfRow(row ConnectApp) loginsdk.App {
	return loginsdk.App{
		Platform:     row.Platform,
		Mode:         loginsdk.AppMode(row.Mode),
		ClientID:     row.ClientID,
		ClientSecret: row.ClientSecret,
		RedirectURI:  row.RedirectURI,
		ServiceURL:   row.ServiceURL,
		Revision:     row.Revision,
		UpdatedAt:    row.UpdatedAt,
	}
}

// GetApp returns the saved app profile of a platform; found is false when no
// profile exists yet.
func GetApp(platform string) (loginsdk.App, bool, error) {
	var row ConnectApp
	err := db.GetDb().Where("platform = ?", platform).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return loginsdk.App{}, false, nil
		}
		return loginsdk.App{}, false, fmt.Errorf("读取应用配置失败: %w", err)
	}
	return appOfRow(row), true, nil
}

// ListApps returns every saved app profile, ordered by platform.
func ListApps() ([]loginsdk.App, error) {
	var rows []ConnectApp
	if err := db.GetDb().Order("platform").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取应用配置列表失败: %w", err)
	}
	apps := make([]loginsdk.App, 0, len(rows))
	for _, row := range rows {
		apps = append(apps, appOfRow(row))
	}
	return apps, nil
}

// AppInput is one app profile submission. An empty ClientSecret keeps the
// stored secret of the same application (same mode and client_id), so the
// browser never needs to re-enter it when editing.
type AppInput struct {
	Platform     string
	Mode         string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	ServiceURL   string
}

// SaveApp validates and stores one app profile. check runs on the merged App
// before anything is written; on success Revision is the previous value plus
// one and UpdatedAt is now.
func SaveApp(in AppInput, check func(loginsdk.App) error) (loginsdk.App, error) {
	in.Platform = strings.TrimSpace(in.Platform)
	in.Mode = strings.TrimSpace(in.Mode)
	in.ClientID = strings.TrimSpace(in.ClientID)
	in.ClientSecret = strings.TrimSpace(in.ClientSecret)
	in.RedirectURI = strings.TrimSpace(in.RedirectURI)
	in.ServiceURL = strings.TrimSpace(in.ServiceURL)

	prev, found, err := GetApp(in.Platform)
	if err != nil {
		return loginsdk.App{}, err
	}
	app := loginsdk.App{
		Platform:     in.Platform,
		Mode:         loginsdk.AppMode(in.Mode),
		ClientID:     in.ClientID,
		ClientSecret: in.ClientSecret,
		RedirectURI:  in.RedirectURI,
		ServiceURL:   in.ServiceURL,
	}
	// A different client_id (or mode) paired with the previous app's secret
	// could never authorize, so the secret is only inherited by the same app.
	if app.ClientSecret == "" && found && prev.Mode == app.Mode && prev.ClientID == app.ClientID {
		app.ClientSecret = prev.ClientSecret
	}
	app.Revision = prev.Revision + 1
	app.UpdatedAt = time.Now()

	if check != nil {
		if err := check(app); err != nil {
			return loginsdk.App{}, err
		}
	}
	row := ConnectApp{
		Platform:     app.Platform,
		Mode:         string(app.Mode),
		ClientID:     app.ClientID,
		ClientSecret: app.ClientSecret,
		RedirectURI:  app.RedirectURI,
		ServiceURL:   app.ServiceURL,
		Revision:     app.Revision,
		UpdatedAt:    app.UpdatedAt,
	}
	if err := db.GetDb().Save(&row).Error; err != nil {
		return loginsdk.App{}, fmt.Errorf("保存应用配置失败: %w", err)
	}
	return app, nil
}

// DeleteApp removes the app profile of a platform. Deleting a profile that
// does not exist is not an error.
func DeleteApp(platform string) error {
	if err := db.GetDb().Delete(&ConnectApp{}, "platform = ?", platform).Error; err != nil {
		return fmt.Errorf("删除应用配置失败: %w", err)
	}
	return nil
}
