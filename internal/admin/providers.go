package admin

import "strings"

// The provider table is the remnant of the former connector registry: the
// OpenList admin UI owns all storage configuration and login now
// (IMPLEMENTATION §结论), so cloud2api keeps only the logical provider id ↔
// OpenList driver name mapping. Static config.yaml drives need the driver
// name pre-filled before the first reconcile pass observes the storage, and
// adopted storages derive their display provider from the driver.

// providerDrivers maps the logical provider id used in config.yaml accounts
// and store rows onto the OpenList storage driver implementing it.
var providerDrivers = map[string]string{
	"local":       "Local",
	"baidu":       "BaiduNetdisk",
	"aliyun":      "AliyundriveOpen",
	"115open":     "115 Open",
	"123open":     "123 Open",
	"quarkopen":   "QuarkOpen",
	"onedrive":    "Onedrive",
	"googledrive": "GoogleDrive",
	"goapi":       "GoAPI",
	"115":         "115 Cloud",
	"quark":       "Quark",
	"uc":          "UC",
	"123pan":      "123Pan",
	"pikpak":      "PikPak",
	"thunder":     "Thunder",
}

// DriverFor returns the OpenList driver name of a provider (static
// config.yaml accounts and rows predating the first reconcile pass).
func DriverFor(provider string) (string, bool) {
	driver, ok := providerDrivers[provider]
	return driver, ok
}

// ProviderForDriver maps an OpenList driver name back onto the logical
// provider id when one exists (display purposes only; unknown drivers —
// storages adopted from OpenList that config.yaml never described — display
// under their lowercased driver name).
func ProviderForDriver(driver string) string {
	for provider, d := range providerDrivers {
		if d == driver {
			return provider
		}
	}
	return strings.ToLower(driver)
}
