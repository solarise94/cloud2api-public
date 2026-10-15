package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cloud2api/internal/openlist"
	"cloud2api/internal/reconcile"
	"cloud2api/internal/storageid"
	"cloud2api/internal/store"
)

// testedOpenListVersion is the OpenList release this cloud2api build was
// verified against (§3.7.6); versions outside the same major.minor warn.
const testedOpenListVersion = "v4.2.6"

// handleOverview implements GET /admin/api/overview (§8.2, 1a rows; the 1c
// security checks fill guest_enabled, tested_version and the extra warnings).
func (p *Portal) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultAdminCallTimeout)
	defer cancel()

	version := ""
	reachable := false
	if settings, err := p.client.GetPublicSettings(ctx); err == nil {
		reachable = true
		version = settings.Version
	}
	tokenOK := false
	var snapshot []openlist.StorageInfo
	if snap, err := p.cachedStorages(ctx); err == nil {
		tokenOK = true
		snapshot = snap
	}
	guestEnabled := false
	if reachable && tokenOK {
		if g, err := p.client.GuestEnabled(ctx); err == nil {
			guestEnabled = g
		}
	}
	// Login-module probe (IMPLEMENTATION-openlist-login-fork §适配补丁范围):
	// cloud2api links to the module's pages; an unpatched OpenList is a normal
	// absent state, only a real probe failure surfaces below.
	connect, connectOK, connectErr := p.connectState(ctx, false)

	driveCount := 0
	var drives []store.Drive
	if list, err := p.st.ListDrives(); err == nil {
		driveCount = len(list)
		drives = list
	}
	keyCount := 0
	if keys, err := p.keys.List(); err == nil {
		for _, k := range keys {
			if !k.Revoked {
				keyCount++
			}
		}
	}

	publicURL, manageURL, publicURLError := "", "", ""
	if stored, ok := p.st.GetSetting("openlist_public_url"); ok {
		publicURL, manageURL, publicURLError = openlistURLParts(stored)
	}
	warnings := []map[string]any{}
	if !reachable {
		warnings = append(warnings, map[string]any{"level": "error", "text": "OpenList 不可达，请检查 openlist 容器", "action_url": "#/openlist"})
	} else if !tokenOK {
		warnings = append(warnings, map[string]any{"level": "error", "text": "OpenList 管理令牌无效（401），请检查 OPENLIST_TOKEN 或配置自愈凭据", "action_url": "#/openlist"})
	}
	if guestEnabled && publishedBeyondLoopback(publicURL) {
		warnings = append(warnings, map[string]any{
			"level":      "warn",
			"text":       "OpenList 的 guest 用户已启用，且 OpenList 已发布到非回环地址（openlist_public_url=" + publicURL + "）；未授权访问者可以匿名浏览文件",
			"action_url": "#/openlist",
		})
	}
	if reachable && !versionInTestedRange(version) {
		warnings = append(warnings, map[string]any{
			"level":      "warn",
			"text":       "OpenList 版本 " + version + " 不在已验证范围（" + testedOpenListVersion + "），联动行为可能变化",
			"action_url": "#/openlist",
		})
	}
	// An absent login module is unremarkable (unpatched OpenList); only a real
	// probe failure warns.
	if connectErr != "" {
		warnings = append(warnings, map[string]any{
			"level":      "warn",
			"text":       "OpenList 登录模块状态读取失败：" + connectErr,
			"action_url": "#/openlist",
		})
	}
	// Drive states needing attention (IMPLEMENTATION §新流程): needs_reauth,
	// error, orphaned and replacement_found — an unconfirmed replacement
	// counts — surface on the overview with a jump to the drives page.
	byMount, byID := indexStorages(snapshot)
	for _, d := range drives {
		state, statusText := reconcile.DisplayState(d.State), ""
		if snapshot != nil {
			res := storageid.Resolve(toStorageIDDrive(d), byMount, byID)
			state, statusText = res.State, res.StatusText
		}
		if statusText == "" {
			statusText = state
		}
		switch state {
		case storageid.StateNeedsReauth:
			warnings = append(warnings, map[string]any{
				"level": "error", "action_url": "#/drives",
				"text": "网盘 " + d.ID + " 需要更新登录凭据：请在 OpenList 中更新后「刷新并纳管」",
			})
		case storageid.StateError:
			warnings = append(warnings, map[string]any{
				"level": "error", "action_url": "#/drives",
				"text": "网盘 " + d.ID + " 上游错误：" + statusText,
			})
		case storageid.StateOrphaned:
			warnings = append(warnings, map[string]any{
				"level": "warning", "action_url": "#/drives",
				"text": "网盘 " + d.ID + " 的 OpenList 存储已被删除（orphaned）",
			})
		case storageid.StateReplacementFound:
			warnings = append(warnings, map[string]any{
				"level": "error", "action_url": "#/drives",
				"text": "网盘 " + d.ID + " 的存储被替换，等待管理员确认替代存储",
			})
		}
	}
	if publicURLError != "" {
		warnings = append(warnings, map[string]any{
			"level":      "warning",
			"text":       "OpenList 公开访问地址无效：" + publicURLError,
			"action_url": "#/settings",
		})
	}

	// The connect_* fields describe the login module for the entry links: only
	// the platform identity (id/name/driver) is exposed, never app fields.
	connectPlatforms := make([]map[string]any, 0, len(connect.Platforms))
	for _, pl := range connect.Platforms {
		connectPlatforms = append(connectPlatforms, map[string]any{
			"id": pl.ID, "name": pl.Name, "driver": pl.Driver,
		})
	}
	connectURL := ""
	if connectOK && publicURLError == "" {
		connectURL = openlistConnectURL(publicURL)
	}
	respondData(w, map[string]any{
		"openlist": map[string]any{
			"version":                   version,
			"reachable":                 reachable,
			"token_ok":                  tokenOK,
			"guest_enabled":             guestEnabled,
			"public_url":                publicURL,
			"manage_url":                manageURL,
			"public_url_error":          publicURLError,
			"tested_version":            testedOpenListVersion,
			"connect_available":         connectOK,
			"connect_handoff_available": connectOK && connect.Handoff && connectURL != "",
			"connect_url":               connectURL,
			"connect_version":           connect.Version,
			"connect_platforms":         connectPlatforms,
			"connect_error":             connectErr,
		},
		"counts": map[string]any{
			"drives": driveCount,
			"keys":   keyCount,
		},
		"warnings": warnings,
	})
}

// publishedBeyondLoopback reports whether the configured openlist_public_url
// points somewhere other than this host's loopback (the guest warning only
// matters then, §3.7.6; by default OpenList publishes on 127.0.0.1 only).
func publishedBeyondLoopback(publicURL string) bool {
	if publicURL == "" {
		return false
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return true // unparseable: assume the worst
	}
	host := u.Hostname()
	return host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1"
}

// versionInTestedRange checks the running version against the tested
// major.minor (patch-level drift is accepted).
func versionInTestedRange(version string) bool {
	cur, ok1 := majorMinor(version)
	tested, ok2 := majorMinor(testedOpenListVersion)
	if !ok1 || !ok2 {
		return false
	}
	return cur == tested
}

func majorMinor(version string) ([2]int, bool) {
	var a, b int
	n, err := fmt.Sscanf(version, "v%d.%d", &a, &b)
	if err != nil || n < 2 {
		n, err = fmt.Sscanf(version, "%d.%d", &a, &b)
	}
	if err != nil || n < 2 {
		return [2]int{}, false
	}
	return [2]int{a, b}, true
}

// handleAudit implements GET /admin/api/audit with filters and next_before
// paging (§8.2).
func (p *Portal) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	entries, nextBefore, err := p.st.QueryAudit(store.AuditFilter{
		Key:    q.Get("key"),
		Drive:  q.Get("drive"),
		Op:     q.Get("op"),
		Status: q.Get("status"),
		Before: q.Get("before"),
		Limit:  limit,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "store error")
		return
	}
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, map[string]any{
			"ts":          e.TS.UTC().Format(time.RFC3339Nano),
			"key_id":      e.KeyID,
			"drive":       e.Drive,
			"op":          e.Op,
			"path":        e.Path,
			"bytes":       e.Bytes,
			"status":      e.Status,
			"duration_ms": e.DurationMS,
			"error":       e.Error,
		})
	}
	var next any
	if nextBefore != "" {
		next = nextBefore
	}
	respondData(w, map[string]any{"entries": rows, "next_before": next})
}

// settingNames are the §8.2 settings fields persisted in the settings table.
// oplist_api_base is gone (IMPLEMENTATION §删除的部分): it only served the
// login wizard and the batch rewrite, and OpenList owns the online API
// address now.
var settingNames = []string{"public_base_url", "openlist_public_url", "policy_text"}

func (p *Portal) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, name := range settingNames {
		v := ""
		if got, ok := p.st.GetSetting(name); ok {
			v = got
		}
		out[name] = v
	}
	respondData(w, out)
}

func (p *Portal) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := decodeBody(w, r, &body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	allowed := map[string]bool{}
	for _, name := range settingNames {
		allowed[name] = true
	}
	for k := range body {
		if !allowed[k] {
			respondError(w, http.StatusBadRequest, "unknown setting "+k)
			return
		}
	}

	// openlist_public_url is validated BEFORE any side effect (IMPLEMENTATION
	// §地址配置): an invalid value must persist nothing.
	if v, ok := body["openlist_public_url"]; ok {
		norm, err := NormalizeOpenlistPublicURL(v)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		body["openlist_public_url"] = norm
	}

	for _, name := range settingNames {
		if v, ok := body[name]; ok {
			if err := p.st.SetSetting(name, v); err != nil {
				respondError(w, http.StatusInternalServerError, "failed to persist setting")
				return
			}
		}
	}

	out := map[string]any{}
	for _, name := range settingNames {
		v := ""
		if got, ok := p.st.GetSetting(name); ok {
			v = got
		}
		out[name] = v
	}
	respondData(w, out)
}
