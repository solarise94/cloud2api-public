package connect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/loginbridge"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
)

// Request bodies are capped so a runaway browser cannot feed unbounded JSON
// into the decoder.
const maxBodyBytes = 64 << 10

// ---- error envelope ----

// httpCodeFor maps a loginsdk error code onto the OpenList envelope code
// (API.md's table). The HTTP status stays 200 per upstream convention.
func httpCodeFor(code string) int {
	switch code {
	case loginsdk.CodeInvalidInput, loginsdk.CodeAppMissing, loginsdk.CodeAppInvalid,
		loginsdk.CodeUnsupported, loginsdk.CodeStateMismatch:
		return 400
	case loginsdk.CodeNotFound:
		return 404
	case loginsdk.CodeConflict:
		return 409
	case loginsdk.CodeExpired, loginsdk.CodeCancelled:
		return 410
	case loginsdk.CodeLimit:
		return 429
	case loginsdk.CodeProvider, loginsdk.CodeNetwork:
		return 502
	default:
		return 500
	}
}

// sdkErr writes the failure envelope for err: the mapped code and the
// machine-readable error_code in data.
func sdkErr(c *gin.Context, err error) {
	code := loginsdk.CodeOf(err)
	common.ErrorWithDataResp(c, err, httpCodeFor(code), gin.H{"error_code": code})
}

// errMessage returns the safe, showable message of err.
func errMessage(err error) string {
	var e *loginsdk.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// ---- request helpers ----

// decodeBody reads one JSON request body of at most 64 KiB.
func decodeBody(c *gin.Context, out any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	if err := json.NewDecoder(c.Request.Body).Decode(out); err != nil {
		return loginsdk.Errorf(loginsdk.CodeInvalidInput, "请求体不是有效的 JSON，且不超过 64 KiB")
	}
	return nil
}

// parseIDQuery reads a query parameter that must be a positive integer.
func parseIDQuery(c *gin.Context, name string) (uint, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, loginsdk.Errorf(loginsdk.CodeInvalidInput, "缺少参数 %s", name)
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || n == 0 {
		return 0, loginsdk.Errorf(loginsdk.CodeInvalidInput, "参数 %s 必须是正整数", name)
	}
	return uint(n), nil
}

// ownerFrom binds a session to the requesting admin: user id plus an owner
// fingerprint. On the legacy bearer path the fingerprint is a hash of the
// exact Authorization header, so another token of the same user cannot drive
// this browser's session; on the scoped-cookie path it is the per-cookie
// random fingerprint assigned at exchange time and stays stable for the
// cookie's lifetime. A request without a recognized admin is an error, never
// a panic.
func ownerFrom(c *gin.Context) (loginsdk.Owner, error) {
	user, _ := c.Request.Context().Value(conf.UserKey).(*model.User)
	if user == nil {
		return loginsdk.Owner{}, loginsdk.Errorf(loginsdk.CodeInvalidInput, "未识别的管理员身份")
	}
	fp := c.GetString(cKeyOwnerFP)
	if fp == "" {
		sum := sha256.Sum256([]byte(c.GetHeader("Authorization")))
		fp = hex.EncodeToString(sum[:])
	}
	return loginsdk.Owner{
		UserID:   user.ID,
		Username: user.Username,
		TokenFP:  fp,
	}, nil
}

// basePath is the site base path without a trailing slash ("" for "" and "/").
func basePath() string {
	if conf.URL == nil {
		return ""
	}
	return strings.TrimSuffix(conf.URL.Path, "/")
}

// adapter looks up a platform and reports the not-found error of API.md.
func (s *server) adapter(platform string) (loginsdk.Adapter, error) {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return nil, loginsdk.Errorf(loginsdk.CodeInvalidInput, "请提供平台标识 platform")
	}
	a, ok := s.reg.Get(platform)
	if !ok {
		return nil, loginsdk.Errorf(loginsdk.CodeNotFound, "未知的登录平台 %s", platform)
	}
	return a, nil
}

// needsApp reports whether a platform has a configurable app profile.
func needsApp(meta loginsdk.Meta) bool {
	return !(len(meta.AppModes) == 1 && meta.AppModes[0] == loginsdk.AppNone)
}

// ---- app payloads ----

type appPayload struct {
	Required   bool             `json:"required"`
	Default    bool             `json:"default,omitempty"`
	Configured bool             `json:"configured"`
	Mode       loginsdk.AppMode `json:"mode"`
	Identity   string           `json:"identity,omitempty"`
	ClientID   string           `json:"client_id,omitempty"`
	// ClientSecretSet / ClientID never apply to a public app: its credentials
	// come from the adapter, not from the administrator.
	ClientSecretSet bool       `json:"client_secret_set"`
	RedirectURI     string     `json:"redirect_uri,omitempty"`
	ServiceURL      string     `json:"service_url,omitempty"`
	Revision        int64      `json:"revision,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	Problem         string     `json:"problem"`
}

// effectiveApp returns the app a platform currently logs in with: the saved
// profile (with the public app's credentials re-attached), else the platform
// default (DefaultApp). isDefault marks the second case; ok is false when
// neither exists (CodeAppMissing).
func effectiveApp(a loginsdk.Adapter) (app loginsdk.App, isDefault, ok bool, err error) {
	meta := a.Meta()
	saved, found, err := loginbridge.GetApp(meta.ID)
	if err != nil {
		return loginsdk.App{}, false, false, loginsdk.Wrap(loginsdk.CodeInternal, "读取应用配置失败", err)
	}
	if found {
		return loginsdk.ResolveApp(a, saved), false, true, nil
	}
	def, has := loginsdk.DefaultApp(a)
	if !has {
		return loginsdk.App{}, false, false, nil
	}
	return def, true, true, nil
}

// appPayloadFor renders a platform's current app object, reading the saved
// profile from the database.
func (s *server) appPayloadFor(a loginsdk.Adapter) (any, error) {
	meta := a.Meta()
	if !needsApp(meta) {
		return gin.H{"required": false, "configured": true, "mode": "none"}, nil
	}
	app, isDefault, ok, err := effectiveApp(a)
	if err != nil {
		return nil, err
	}
	if !ok {
		return appPayload{Required: true}, nil
	}
	return buildAppPayload(a, app, isDefault), nil
}

// buildAppPayload renders one app object. The client secret itself never
// leaves the server; only the boolean client_secret_set does.
func buildAppPayload(a loginsdk.Adapter, app loginsdk.App, isDefault bool) any {
	p := appPayload{
		Required: true,
		Default:  isDefault,
		Mode:     app.Mode,
		Identity: app.Identity(),
	}
	if app.Mode != loginsdk.AppPublic {
		p.ClientID = app.ClientID
		p.ClientSecretSet = app.ClientSecret != ""
		p.RedirectURI = app.RedirectURI
		p.ServiceURL = app.ServiceURL
		p.Revision = app.Revision
		if !app.UpdatedAt.IsZero() {
			updatedAt := app.UpdatedAt
			p.UpdatedAt = &updatedAt
		}
	}
	p.Problem = savedAppProblem(a, app)
	p.Configured = p.Problem == ""
	return p
}

// savedAppProblem explains why a saved app profile currently cannot be used;
// "" means it can.
func savedAppProblem(a loginsdk.Adapter, app loginsdk.App) string {
	meta := a.Meta()
	if !savedModeAllowed(meta.AppModes, app.Mode) {
		return fmt.Sprintf("应用模式 %s 不受该平台支持", app.Mode)
	}
	if err := a.CheckApp(app); err != nil {
		return loginsdk.Scrub(errMessage(err), app.ClientSecret)
	}
	return ""
}

func savedModeAllowed(modes []loginsdk.AppMode, mode loginsdk.AppMode) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

// ---- target payloads ----

type targetPlatformPayload struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Problem string `json:"problem"`
}

// adaptersForDriver lists the registered adapters for one OpenList driver.
func (s *server) adaptersForDriver(driver string) []loginsdk.Adapter {
	var out []loginsdk.Adapter
	for _, a := range s.reg.List() {
		if a.Meta().Driver == driver {
			out = append(out, a)
		}
	}
	return out
}

// storageSecrets collects the credential values stored in a storage row under
// the reauth fields of its driver's adapters: driver status strings echo them
// on failed logins, so they must be scrubbed out of every response.
func storageSecrets(st *model.Storage, adapters []loginsdk.Adapter) []string {
	if len(adapters) == 0 {
		return nil
	}
	keys := map[string]bool{}
	for _, a := range adapters {
		for _, k := range a.Meta().ReauthFields {
			keys[k] = true
		}
	}
	addition, err := loginbridge.DecodeAddition(st.Addition)
	if err != nil {
		return nil
	}
	var secrets []string
	for k := range keys {
		if v, ok := addition[k].(string); ok && v != "" {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// targetProblem says why a platform currently cannot re-authorize st.
func targetProblem(a loginsdk.Adapter, st *model.Storage) (string, error) {
	meta := a.Meta()
	if needsApp(meta) {
		app, _, ok, err := effectiveApp(a)
		if err != nil {
			return "", err
		}
		if !ok {
			return "尚未配置应用", nil
		}
		if problem := savedAppProblem(a, app); problem != "" {
			return problem, nil
		}
	}
	if err := loginbridge.CheckTarget(st, a); err != nil {
		return errMessage(err), nil
	}
	return "", nil
}

// ---- handlers ----

func (s *server) handlePlatforms(c *gin.Context) {
	suggested := ""
	if strings.HasPrefix(conf.Conf.SiteURL, "http") {
		suggested = strings.TrimSuffix(conf.Conf.SiteURL, "/") + "/api/connect/oauth/callback"
	}
	type platformPayload struct {
		loginsdk.Meta
		// ActiveMethod is the method of the app the platform currently logs
		// in with (the saved profile's mode, or the default).
		ActiveMethod loginsdk.Method `json:"active_method"`
		App          any             `json:"app"`
	}
	platforms := make([]platformPayload, 0)
	for _, a := range s.reg.List() {
		app, err := s.appPayloadFor(a)
		if err != nil {
			sdkErr(c, err)
			return
		}
		meta := a.Meta()
		active := meta.Method
		if needsApp(meta) {
			if effective, _, ok, err := effectiveApp(a); err != nil {
				sdkErr(c, err)
				return
			} else if ok {
				active = meta.MethodFor(effective.Mode)
			}
		}
		platforms = append(platforms, platformPayload{Meta: meta, ActiveMethod: active, App: app})
	}
	common.SuccessResp(c, gin.H{
		"module": gin.H{
			"name":             "connect",
			"version":          Version,
			"upstream_version": conf.Version,
			"upstream_commit":  conf.GitCommit,
			"web_version":      conf.WebVersion,
			// Explicit capability flag: cloud2api checks this instead of
			// parsing versions to decide whether the password-free handoff
			// entry can be offered.
			"handoff": true,
		},
		"callback_path":          basePath() + "/api/connect/oauth/callback",
		"suggested_redirect_uri": suggested,
		"platforms":              platforms,
	})
}

type appSaveReq struct {
	Platform     string `json:"platform"`
	Mode         string `json:"mode"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
	ServiceURL   string `json:"service_url"`
}

func (s *server) handleAppSave(c *gin.Context) {
	var req appSaveReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	a, err := s.adapter(req.Platform)
	if err != nil {
		sdkErr(c, err)
		return
	}
	meta := a.Meta()
	if !needsApp(meta) {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeUnsupported, "该平台不需要应用配置"))
		return
	}
	if !savedModeAllowed(meta.AppModes, loginsdk.AppMode(strings.TrimSpace(req.Mode))) {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeAppInvalid, "该平台不支持应用模式 %q", req.Mode))
		return
	}
	// A public_app profile stores the mode only: its credentials always come
	// from the adapter, and any client fields the request carried are dropped.
	input := loginbridge.AppInput{
		Platform:     req.Platform,
		Mode:         req.Mode,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
		RedirectURI:  req.RedirectURI,
		ServiceURL:   req.ServiceURL,
	}
	check := a.CheckApp
	if loginsdk.AppMode(strings.TrimSpace(req.Mode)) == loginsdk.AppPublic {
		input = loginbridge.AppInput{Platform: req.Platform, Mode: req.Mode}
		check = func(app loginsdk.App) error {
			return a.CheckApp(loginsdk.ResolveApp(a, app))
		}
	}
	app, err := loginbridge.SaveApp(input, check)
	if err != nil {
		// A CheckApp rejection carries its own code; storage errors do not.
		var e *loginsdk.Error
		if !errors.As(err, &e) {
			err = loginsdk.Wrap(loginsdk.CodeInternal, "保存应用配置失败", err)
		}
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, buildAppPayload(a, loginsdk.ResolveApp(a, app), false))
}

type platformReq struct {
	Platform string `json:"platform"`
}

func (s *server) handleAppDelete(c *gin.Context) {
	var req platformReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	a, err := s.adapter(req.Platform)
	if err != nil {
		sdkErr(c, err)
		return
	}
	if err := loginbridge.DeleteApp(strings.TrimSpace(req.Platform)); err != nil {
		sdkErr(c, loginsdk.Wrap(loginsdk.CodeInternal, "删除应用配置失败", err))
		return
	}
	app, err := s.appPayloadFor(a)
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, app)
}

// handleMountSuggest proposes the default mount path of a new storage: the
// platform name, suffixed -2 … -99 while taken; "" when nothing is free.
func (s *server) handleMountSuggest(c *gin.Context) {
	a, err := s.adapter(c.Query("platform"))
	if err != nil {
		sdkErr(c, err)
		return
	}
	mount, err := loginbridge.SuggestMount(a.Meta().Name)
	if err != nil {
		sdkErr(c, loginsdk.Wrap(loginsdk.CodeInternal, "查询挂载路径失败", err))
		return
	}
	common.SuccessResp(c, gin.H{"mount_path": mount})
}

func (s *server) handleTarget(c *gin.Context) {
	id, err := parseIDQuery(c, "id")
	if err != nil {
		sdkErr(c, err)
		return
	}
	st, err := loginbridge.ReadTarget(id)
	if err != nil {
		sdkErr(c, err)
		return
	}
	payload, err := s.targetPayload(st)
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, payload)
}

func (s *server) targetPayload(st *model.Storage) (gin.H, error) {
	matching := s.adaptersForDriver(st.Driver)
	secrets := storageSecrets(st, matching)
	platforms := make([]targetPlatformPayload, 0, len(matching))
	for _, a := range matching {
		meta := a.Meta()
		problem, err := targetProblem(a, st)
		if err != nil {
			return nil, err
		}
		platforms = append(platforms, targetPlatformPayload{
			ID:      meta.ID,
			Name:    meta.Name,
			Problem: loginsdk.Scrub(problem, secrets...),
		})
	}
	return gin.H{
		"storage_id": st.ID,
		"mount_path": st.MountPath,
		"driver":     st.Driver,
		"status":     loginsdk.Scrub(st.Status, secrets...),
		"disabled":   st.Disabled,
		"modified":   st.Modified,
		"platforms":  platforms,
		"edit_path":  basePath() + "/@manage/storages/edit/" + strconv.FormatUint(uint64(st.ID), 10),
	}, nil
}

type sessionStartReq struct {
	Platform        string `json:"platform"`
	ReauthStorageID int64  `json:"reauth_storage_id"`
}

func (s *server) handleSessionStart(c *gin.Context) {
	var req sessionStartReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	a, err := s.adapter(req.Platform)
	if err != nil {
		sdkErr(c, err)
		return
	}
	meta := a.Meta()
	if req.ReauthStorageID < 0 {
		sdkErr(c, loginsdk.Errorf(loginsdk.CodeInvalidInput, "reauth_storage_id 必须是正整数"))
		return
	}

	var target *loginsdk.Target
	if req.ReauthStorageID > 0 {
		st, err := loginbridge.ReadTarget(uint(req.ReauthStorageID))
		if err != nil {
			sdkErr(c, err)
			return
		}
		if err := loginbridge.CheckTarget(st, a); err != nil {
			sdkErr(c, err)
			return
		}
		t := loginbridge.TargetOf(st)
		t.Status = loginsdk.Scrub(t.Status, storageSecrets(st, s.adaptersForDriver(st.Driver))...)
		target = &t
	}

	// Platforms that need no app get the zero App; the manager substitutes
	// App{Platform, Mode: AppNone} for them anyway. Every other platform logs
	// in with its saved profile or, when none is saved and the default needs
	// no administrator input, with that default.
	app := loginsdk.App{}
	if needsApp(meta) {
		effective, _, ok, err := effectiveApp(a)
		if err != nil {
			sdkErr(c, err)
			return
		}
		if !ok {
			sdkErr(c, loginsdk.Errorf(loginsdk.CodeAppMissing, "请先配置%s的应用", meta.Name))
			return
		}
		app = effective
	}

	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.Start(c.Request.Context(), loginsdk.StartInput{
		Owner:    owner,
		Platform: meta.ID,
		App:      app,
		Target:   target,
	})
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}

type sessionIDReq struct {
	ID string `json:"id"`
}

func (s *server) handleSessionGet(c *gin.Context) {
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.Get(c.Request.Context(), owner, strings.TrimSpace(c.Query("id")))
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}

type sessionSubmitReq struct {
	ID     string            `json:"id"`
	Values map[string]string `json:"values"`
}

func (s *server) handleSessionSubmit(c *gin.Context) {
	var req sessionSubmitReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.Submit(c.Request.Context(), owner, strings.TrimSpace(req.ID), req.Values)
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}

func (s *server) handleSessionRefresh(c *gin.Context) {
	var req sessionIDReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.RefreshQR(c.Request.Context(), owner, strings.TrimSpace(req.ID))
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}

func (s *server) handleSessionCancel(c *gin.Context) {
	var req sessionIDReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.Cancel(owner, strings.TrimSpace(req.ID))
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}

type sessionSaveReq struct {
	ID        string `json:"id"`
	MountPath string `json:"mount_path"`
	Root      string `json:"root"`
	Remark    string `json:"remark"`
}

func (s *server) handleSessionSave(c *gin.Context) {
	var req sessionSaveReq
	if err := decodeBody(c, &req); err != nil {
		sdkErr(c, err)
		return
	}
	owner, err := ownerFrom(c)
	if err != nil {
		sdkErr(c, err)
		return
	}
	view, err := s.mgr.Save(c.Request.Context(), owner, strings.TrimSpace(req.ID),
		func(ctx context.Context, r loginsdk.SaveRequest) loginsdk.SaveResult {
			if r.Target != nil {
				return loginbridge.Reauth(ctx, r.Adapter, r.App, r.Auth, *r.Target)
			}
			return loginbridge.Create(ctx, r.Adapter, r.App, r.Auth, loginbridge.CreateInput{
				MountPath: req.MountPath,
				Root:      req.Root,
				Remark:    req.Remark,
			})
		})
	if err != nil {
		sdkErr(c, err)
		return
	}
	common.SuccessResp(c, view)
}
