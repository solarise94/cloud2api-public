package loginsdk

import (
	"fmt"
	"regexp"
	"sync"
)

// Registry holds the adapters compiled into the binary, in registration order.
// Adapters are registered at build time (package adapters/all); there is no
// dynamic plugin loading.
type Registry struct {
	mu    sync.RWMutex
	order []string
	byID  map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]Adapter{}} }

// Default is the process-wide registry the connect module serves.
var Default = NewRegistry()

// Register adds a to Default and panics when it is invalid or duplicated
// (registration happens in init, so a broken adapter stops the build's tests
// and the binary at start-up instead of failing at login time).
func Register(a Adapter) {
	if err := Default.Register(a); err != nil {
		panic("loginsdk: " + err.Error())
	}
}

// Lookup finds an adapter of Default by id.
func Lookup(id string) (Adapter, bool) { return Default.Get(id) }

// Adapters lists Default's adapters in registration order.
func Adapters() []Adapter { return Default.List() }

// Register adds a after validating it.
func (r *Registry) Register(a Adapter) error {
	if err := Validate(a); err != nil {
		return err
	}
	id := a.Meta().ID
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("adapter %q registered twice", id)
	}
	r.byID[id] = a
	r.order = append(r.order, id)
	return nil
}

// Get returns the adapter with the given id.
func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[id]
	return a, ok
}

// List returns the adapters in registration order.
func (r *Registry) List() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

var adapterIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func knownMethod(m Method) bool {
	return m == MethodOAuth || m == MethodQR || m == MethodForm
}

// Validate checks an adapter's metadata and that, for every app mode, it
// implements the interface that mode's method needs (MethodFor). Adapter
// tests should call it on their constructor.
func Validate(a Adapter) error {
	if a == nil {
		return fmt.Errorf("nil adapter")
	}
	m := a.Meta()
	if !adapterIDPattern.MatchString(m.ID) {
		return fmt.Errorf("adapter id %q must match %s", m.ID, adapterIDPattern)
	}
	if m.Name == "" || m.Driver == "" {
		return fmt.Errorf("adapter %q: name and driver are required", m.ID)
	}
	if !knownMethod(m.Method) {
		return fmt.Errorf("adapter %q: unknown method %q", m.ID, m.Method)
	}
	if len(m.AppModes) == 0 {
		return fmt.Errorf("adapter %q: at least one app mode is required", m.ID)
	}
	for mode, method := range m.ModeMethods {
		if !containsAppMode(m.AppModes, mode) {
			return fmt.Errorf("adapter %q: mode_methods overrides unlisted mode %q", m.ID, mode)
		}
		if !knownMethod(method) {
			return fmt.Errorf("adapter %q: unknown method %q for mode %q", m.ID, method, mode)
		}
	}
	usesOAuth := false
	for _, mode := range m.AppModes {
		switch mode {
		case AppNone:
			if len(m.AppModes) != 1 {
				return fmt.Errorf("adapter %q: app mode none cannot be combined", m.ID)
			}
		case AppOwn, AppOnlineService, AppPublic:
		default:
			return fmt.Errorf("adapter %q: unknown app mode %q", m.ID, mode)
		}
		switch method := m.MethodFor(mode); method {
		case MethodOAuth:
			usesOAuth = true
			if _, ok := a.(OAuthAdapter); !ok {
				return fmt.Errorf("adapter %q: mode %s requires OAuthAdapter (method oauth)", m.ID, mode)
			}
		case MethodQR:
			if _, ok := a.(QRAdapter); !ok {
				return fmt.Errorf("adapter %q: mode %s requires QRAdapter (method qrcode)", m.ID, mode)
			}
		case MethodForm:
			if _, ok := a.(FormAdapter); !ok {
				return fmt.Errorf("adapter %q: mode %s requires FormAdapter (method form)", m.ID, mode)
			}
			if len(m.FormFields) == 0 {
				return fmt.Errorf("adapter %q: method form requires form fields", m.ID)
			}
		default:
			return fmt.Errorf("adapter %q: unknown method %q for mode %q", m.ID, method, mode)
		}
		if mode == AppPublic {
			pa, ok := a.(PublicAppProvider)
			if !ok {
				return fmt.Errorf("adapter %q: app mode public_app requires PublicAppProvider", m.ID)
			}
			app := pa.PublicApp()
			if app.Mode != AppPublic || app.ClientID == "" || app.ClientSecret == "" {
				return fmt.Errorf("adapter %q: PublicApp must return the public app with client id and secret", m.ID)
			}
		}
	}
	if !usesOAuth && (m.PKCE || m.AllowOOB) {
		return fmt.Errorf("adapter %q: pkce/allow_oob only apply to oauth", m.ID)
	}
	seen := map[string]bool{}
	for _, f := range m.FormFields {
		if f.Key == "" || seen[f.Key] {
			return fmt.Errorf("adapter %q: form field keys must be unique and non-empty", m.ID)
		}
		seen[f.Key] = true
	}
	if m.RootField != "root_folder_path" && m.RootField != "root_folder_id" {
		return fmt.Errorf("adapter %q: root field must be root_folder_path or root_folder_id", m.ID)
	}
	if m.RootDefault == "" || m.RootLabel == "" {
		return fmt.Errorf("adapter %q: root default and label are required", m.ID)
	}
	if len(m.ReauthFields) == 0 {
		return fmt.Errorf("adapter %q: reauth fields are required", m.ID)
	}
	seen = map[string]bool{}
	for _, k := range m.ReauthFields {
		if k == "" || seen[k] {
			return fmt.Errorf("adapter %q: reauth fields must be unique and non-empty", m.ID)
		}
		if k == m.RootField {
			return fmt.Errorf("adapter %q: the root field is the user's choice, not a reauth field", m.ID)
		}
		seen[k] = true
	}
	return nil
}

// ValidateAuth checks an AuthConfig produced by adapter a for app: it must be
// for a's driver and the app's mode, carry at least one field, and only touch
// a's declared reauth fields (create-only fields may not touch the root or a
// reauth field). The session manager applies it to every login result and
// the config adaptation layer again before writing.
func ValidateAuth(a Adapter, app App, auth *AuthConfig) error {
	m := a.Meta()
	if auth == nil {
		return Errorf(CodeInternal, "适配器 %s 没有返回认证配置", m.ID)
	}
	if auth.Driver != m.Driver {
		return Errorf(CodeInternal, "适配器 %s 返回了驱动 %q 的认证配置，应为 %q", m.ID, auth.Driver, m.Driver)
	}
	if auth.AppMode != app.Mode {
		return Errorf(CodeInternal, "认证配置的应用模式 %q 与所选应用 %q 不一致", auth.AppMode, app.Mode)
	}
	if len(auth.Fields) == 0 {
		return Errorf(CodeInternal, "适配器 %s 返回的认证配置没有任何字段", m.ID)
	}
	allowed := map[string]bool{}
	for _, k := range m.ReauthFields {
		allowed[k] = true
	}
	for k := range auth.Fields {
		if !allowed[k] {
			return Errorf(CodeInternal, "适配器 %s 试图写入未声明的字段 %q", m.ID, k)
		}
	}
	for k := range auth.CreateFields {
		if k == m.RootField || allowed[k] {
			return Errorf(CodeInternal, "适配器 %s 的新建字段 %q 与根目录或凭据字段冲突", m.ID, k)
		}
	}
	return nil
}
