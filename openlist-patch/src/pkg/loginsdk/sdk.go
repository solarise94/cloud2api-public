// Package loginsdk is the reusable login core of the OpenList connect module:
// platform adapter registry, login sessions, OAuth state and PKCE, QR-code
// polling and short-lived credential staging.
//
// The SDK never touches files or the storage database and never renews a
// token. An adapter turns one completed login into an AuthConfig; the config
// adaptation layer (internal/loginbridge) writes it into an OpenList storage
// through OpenList's own storage operations, and from then on the storage's
// driver renews the credential with the renewal parameters the AuthConfig
// carried. Adapters therefore must not call a provider's refresh endpoint:
// many providers rotate refresh tokens, and a second refresher racing the
// driver would invalidate the credential the driver holds.
//
// Credentials live only in server memory, only until the session is saved,
// cancelled or expires. Everything the browser receives is a View, which
// carries progress and redacted summaries and never a credential value.
package loginsdk

import (
	"context"
	"time"
)

// Method is how an adapter obtains credentials.
type Method string

const (
	// MethodOAuth is a browser authorization-code redirect. The provider sends
	// the browser to the connect callback (or the user pastes the final URL /
	// an out-of-band code) and the adapter exchanges the code server-side.
	MethodOAuth Method = "oauth"
	// MethodQR shows a provider QR code; the server polls its status.
	MethodQR Method = "qrcode"
	// MethodForm collects fields typed into the page (e.g. account+password).
	MethodForm Method = "form"
)

// AppMode says which application identity a login runs under. It is recorded
// in an app profile, or is the platform default (AppModes[0]) when that
// default needs no administrator input; it decides the renewal parameters the
// adapter writes, and nothing infers it from which credential fields are
// empty.
type AppMode string

const (
	// AppNone: the platform needs no developer application (account login).
	AppNone AppMode = "none"
	// AppOwn: the administrator's own developer application. The driver renews
	// directly with the provider using the app's client id/secret.
	AppOwn AppMode = "own_app"
	// AppOnlineService: the OpenList online authorization service (by default
	// DefaultServiceURL). The driver renews through that service.
	AppOnlineService AppMode = "online_service"
	// AppPublic: a public application the adapter ships (its credentials are
	// published by upstream tooling). The driver renews directly with the
	// provider using it.
	AppPublic AppMode = "public_app"
)

// DefaultServiceURL is the OpenList online authorization service used when no
// other service URL is configured.
const DefaultServiceURL = "https://api.oplist.org"

// Field describes one input of a form login or of a create-only connection
// setting. Secret values are write-only: they are never echoed to the browser.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // "text" | "password" | "url" | "select"
	Required bool     `json:"required"`
	Secret   bool     `json:"secret"`
	Default  string   `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
	Help     string   `json:"help,omitempty"`
	// CreateOnly fields belong to the storage's connection settings (for
	// example a server address). The page hides them during re-authorization
	// and the adapter must not require them then.
	CreateOnly bool `json:"create_only,omitempty"`
}

// Meta is the static description of an adapter. It is shown in the platform
// picker, returned by the platforms API and copied into the platform table of
// the contribution docs.
type Meta struct {
	ID     string `json:"id"`     // stable adapter id, lowercase, e.g. "baidu"
	Name   string `json:"name"`   // display name, e.g. "百度网盘"
	Driver string `json:"driver"` // OpenList driver name, e.g. "BaiduNetdisk"
	Method Method `json:"method"`
	// AppModes lists the supported application modes; the first entry is the
	// default the app form preselects (and the mode used when no profile is
	// saved and that default needs no administrator input). Adapters that need
	// no app use []AppMode{AppNone}.
	AppModes []AppMode `json:"app_modes"`
	// ModeMethods overrides Method for the listed modes. Convention: Method is
	// the method of AppModes[0], so that mode has no entry here.
	ModeMethods map[AppMode]Method `json:"mode_methods,omitempty"`
	Scopes      []string           `json:"scopes,omitempty"` // permissions the login requests
	// PKCE makes the SDK generate an S256 verifier/challenge for OAuth logins.
	PKCE bool `json:"pkce"`
	// AllowOOB lets an own app use redirect_uri "oob": the provider shows the
	// authorization code and the user pastes it (no state round trip).
	AllowOOB bool `json:"allow_oob"`
	// FormFields describe the inputs of a form login; they are used by the
	// modes whose method is form.
	FormFields []Field `json:"form_fields,omitempty"`
	// PublicAppLabel is the Chinese description of the public app shown next
	// to the public_app mode in the app form.
	PublicAppLabel string `json:"public_app_label,omitempty"`
	// FormLabel is the badge text for this adapter's form login (e.g.
	// 「粘贴令牌」); empty means the page shows 「账号密码」.
	FormLabel string `json:"form_label,omitempty"`
	// RootField is the addition key of the storage root ("root_folder_path"
	// or "root_folder_id"); the user picks its value at the storage step.
	RootField   string `json:"root_field"`
	RootDefault string `json:"root_default"`
	RootLabel   string `json:"root_label"`
	RootHelp    string `json:"root_help,omitempty"`
	// ReauthFields are the addition keys a re-authorization may replace: the
	// credential plus its renewal parameters. Every other setting of an
	// existing storage is preserved. AuthConfig.Fields must be a subset.
	ReauthFields []string `json:"reauth_fields"`
	Notes        []string `json:"notes,omitempty"` // limitations shown in the page (Chinese)
	// Verified records the real-account verification ("" = not verified with
	// a real account yet; otherwise a short record such as
	// "2026-10-20 个人账号 v4.2.6"). The page labels unverified adapters.
	Verified string `json:"verified"`
	DocsURL  string `json:"docs_url,omitempty"`
}

// MethodFor returns the login method of one app mode: ModeMethods overrides
// Method, which by convention is the method of AppModes[0].
func (m Meta) MethodFor(mode AppMode) Method {
	if method, ok := m.ModeMethods[mode]; ok && method != "" {
		return method
	}
	return m.Method
}

// App is one administrator-configured application profile (one per
// platform). ClientSecret never leaves the server.
type App struct {
	Platform     string    `json:"platform"`
	Mode         AppMode   `json:"mode"`
	ClientID     string    `json:"client_id,omitempty"`
	ClientSecret string    `json:"-"`
	RedirectURI  string    `json:"redirect_uri,omitempty"`
	ServiceURL   string    `json:"service_url,omitempty"`
	Revision     int64     `json:"revision"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Identity is the browser-safe description of the application: the masked
// client id for an own or public app, the service URL for the online service.
func (a App) Identity() string {
	switch a.Mode {
	case AppOwn, AppPublic:
		return MaskID(a.ClientID)
	case AppOnlineService:
		return a.ServiceURL
	}
	return ""
}

// DefaultApp returns the app a platform uses when no profile is saved, by its
// AppModes[0]: AppNone and AppPublic need no administrator input,
// AppOnlineService gets the default service URL. AppOwn (and an adapter with
// no app modes) has no default. The returned App carries the adapter's
// platform id.
func DefaultApp(a Adapter) (App, bool) {
	meta := a.Meta()
	if len(meta.AppModes) == 0 {
		return App{}, false
	}
	switch meta.AppModes[0] {
	case AppNone:
		return App{Platform: meta.ID, Mode: AppNone}, true
	case AppPublic:
		pa, ok := a.(PublicAppProvider)
		if !ok {
			return App{}, false
		}
		app := pa.PublicApp()
		app.Platform = meta.ID
		return app, true
	case AppOnlineService:
		return App{Platform: meta.ID, Mode: AppOnlineService, ServiceURL: DefaultServiceURL}, true
	}
	return App{}, false
}

// ResolveApp re-attaches the credentials a saved profile does not store: a
// saved AppPublic profile keeps only the mode, so its ClientID / ClientSecret
// / RedirectURI come from the adapter's PublicApp. Every other mode is
// returned unchanged.
func ResolveApp(a Adapter, saved App) App {
	if saved.Mode != AppPublic {
		return saved
	}
	pa, ok := a.(PublicAppProvider)
	if !ok {
		return saved
	}
	app := pa.PublicApp()
	app.Platform = saved.Platform
	app.Revision = saved.Revision
	app.UpdatedAt = saved.UpdatedAt
	return app
}

// AuthConfig is the complete, server-only result of one login: which driver
// it is for, which credential and application it carries, and the addition
// values to write. The config adaptation layer is its only consumer.
type AuthConfig struct {
	Driver         string  // must equal the adapter's Meta.Driver
	CredentialType string  // e.g. "oauth_refresh_token", "account_password"
	AppMode        AppMode // mode of the app the credential was issued to
	AppIdentity    string  // full client id or service URL (server-side record)
	// Renewal describes in Chinese how the driver will renew this credential
	// with the parameters in Fields, e.g. "驱动使用自有应用直接续期".
	Renewal string
	Account string // optional non-secret account label (nickname / e-mail)
	// Fields are the credential and renewal values written on create AND on
	// re-authorization. Keys must be a subset of Meta.ReauthFields.
	Fields map[string]any
	// CreateFields are connection settings written only when a new storage
	// is created (e.g. OneDrive region=global); ignored on re-authorization.
	CreateFields map[string]any
	// Secrets are the raw secret values (tokens, passwords, client secret)
	// that must be scrubbed from any message, log line or error.
	Secrets []string
}

// AuthSummary is the browser-safe view of an AuthConfig: no values, only what
// kind of credential it is, how it renews and which fields will be written.
type AuthSummary struct {
	CredentialType string   `json:"credential_type"`
	AppMode        AppMode  `json:"app_mode"`
	AppIdentity    string   `json:"app_identity"` // masked for own apps
	Renewal        string   `json:"renewal"`
	Account        string   `json:"account,omitempty"`
	Fields         []string `json:"fields"`                  // keys of Fields, sorted
	CreateFields   []string `json:"create_fields,omitempty"` // keys of CreateFields, sorted
}

// Adapter is implemented by every platform adapter. For every app mode it
// lists, it must also implement the interface that mode's method needs
// (Meta.MethodFor); Register enforces this.
type Adapter interface {
	Meta() Meta
	// CheckApp validates an app profile for this adapter: a supported mode
	// and every field that mode needs. It returns an *Error with
	// CodeAppInvalid and a Chinese message naming what is missing.
	CheckApp(app App) error
}

// OAuthAdapter performs an authorization-code login.
type OAuthAdapter interface {
	Adapter
	// AuthorizeURL builds the provider authorization URL for app (its
	// RedirectURI is the redirect_uri), state, and the PKCE challenge when
	// Meta().PKCE is set (pkce is the zero value otherwise).
	AuthorizeURL(app App, state string, pkce PKCE) (string, error)
	// Exchange redeems an authorization code. It must not refresh the token.
	Exchange(ctx context.Context, app App, code string, pkce PKCE) (*AuthConfig, error)
}

// QRAdapter performs a scan-to-login.
type QRAdapter interface {
	Adapter
	StartQR(ctx context.Context, app App) (*QRCode, error)
	// PollQR checks the code once. With QRConfirmed it returns the AuthConfig
	// (the adapter completes any code exchange inside this call). A transport
	// failure is reported as an error with CodeNetwork; the SDK then shows
	// QRNetworkError and keeps polling until the code expires.
	PollQR(ctx context.Context, app App, qr *QRCode) (*QRPoll, error)
}

// FormAdapter performs a login from typed fields.
type FormAdapter interface {
	Adapter
	Submit(ctx context.Context, in SubmitInput) (*AuthConfig, error)
}

// ReauthChecker is implemented by adapters that can only re-authorize some
// storages of their driver (e.g. OneDrive global region only). addition is the
// storage's current addition JSON decoded with numbers as json.Number.
type ReauthChecker interface {
	CheckReauth(addition map[string]any) error
}

// PublicAppProvider is required for adapters listing AppPublic. It returns the
// published app (Mode AppPublic, ClientID, ClientSecret, optional
// RedirectURI). The secret is still server-only.
type PublicAppProvider interface {
	PublicApp() App
}

// Guide is the external page a form login sends the user to first (e.g. to
// obtain a token there).
type Guide struct {
	URL   string   `json:"url"`
	Label string   `json:"label"`           // button text
	Steps []string `json:"steps,omitempty"` // ordered instructions (Chinese)
}

// FormGuider is implemented by form adapters that send the user to another
// page first (e.g. to get a token).
type FormGuider interface {
	FormGuide(app App, reauth bool) *Guide
}

// SubmitInput carries a form login. Reauth is true when the session re-
// authorizes an existing storage; CreateOnly fields are then absent.
type SubmitInput struct {
	App    App
	Values map[string]string
	Reauth bool
}

// PKCE is an RFC 7636 S256 pair. Zero value: PKCE not used.
type PKCE struct {
	Verifier  string
	Challenge string
	Method    string // "S256"
}

// QRCode is one issued QR code. ImageURL (provider-hosted image) or Content
// (text the page encodes itself) goes to the browser; Private never does.
type QRCode struct {
	ImageURL string
	Content  string
	// Message is the initial hint shown while waiting (e.g. the user code
	// alternative); the session's QR message starts with it and later polls
	// may replace it.
	Message   string
	Private   map[string]string // adapter poll state, e.g. {"sid": "..."}
	ExpiresAt time.Time         // upper bound; the provider status wins
	PollEvery time.Duration     // provider poll floor; the SDK enforces >= its minimum
}

// QRStatus is the user-visible state of a QR code.
type QRStatus string

const (
	QRWaiting      QRStatus = "waiting"       // shown, not scanned yet
	QRScanned      QRStatus = "scanned"       // scanned, waiting for confirmation on the phone
	QRConfirmed    QRStatus = "confirmed"     // confirmed; credentials obtained
	QRCancelled    QRStatus = "cancelled"     // the user rejected the login on the phone
	QRExpired      QRStatus = "expired"       // the code expired; a new one can be issued
	QRNetworkError QRStatus = "network_error" // the last poll failed; polling continues
)

// QRPoll is the outcome of one PollQR call.
type QRPoll struct {
	Status  QRStatus
	Auth    *AuthConfig // with QRConfirmed only
	Message string      // optional provider detail (Chinese or provider text)
}

// Owner binds a session to the OpenList administrator that started it: the
// user id plus a fingerprint of the Authorization header, so another token of
// the same user (for example cloud2api's permanent admin token) cannot drive
// a browser's session.
type Owner struct {
	UserID   uint
	Username string
	TokenFP  string
}

// Target is the storage a re-authorization is pinned to, as read when the
// session started. Modified is compared again right before saving.
type Target struct {
	StorageID uint      `json:"storage_id"`
	MountPath string    `json:"mount_path"`
	Driver    string    `json:"driver"`
	Status    string    `json:"status"`
	Modified  time.Time `json:"modified"`
}

// Phase is the lifecycle position of a session.
type Phase string

const (
	PhasePending    Phase = "pending"     // waiting for the user to authorize / scan / submit
	PhaseAuthorized Phase = "authorized"  // credentials staged; storage step and save
	PhaseSaving     Phase = "saving"      // the config adaptation layer is writing
	PhaseSaved      Phase = "saved"       // saved successfully; credentials wiped
	PhaseSaveFailed Phase = "save_failed" // the write failed; see Result; credentials wiped
	PhaseFailed     Phase = "failed"      // the login itself failed (provider error, bad state…)
	PhaseCancelled  Phase = "cancelled"   // cancelled by the user (page or provider)
	PhaseExpired    Phase = "expired"     // timed out; credentials wiped
)

// View is the browser-safe state of one session.
type View struct {
	ID           string       `json:"id"`
	Platform     string       `json:"platform"`
	PlatformName string       `json:"platform_name"`
	Driver       string       `json:"driver"`
	Method       Method       `json:"method"` // the session's effective method
	Mode         string       `json:"mode"`   // "create" | "reauth"
	Target       *Target      `json:"target,omitempty"`
	Phase        Phase        `json:"phase"`
	App          AppView      `json:"app"`
	OAuth        *OAuthView   `json:"oauth,omitempty"`
	QR           *QRView      `json:"qr,omitempty"`
	Form         []Field      `json:"form,omitempty"`
	Guide        *Guide       `json:"guide,omitempty"`      // form sessions whose adapter implements FormGuider
	FormLabel    string       `json:"form_label,omitempty"` // from Meta
	Auth         *AuthSummary `json:"auth,omitempty"`
	Result       *SaveResult  `json:"result,omitempty"`
	Error        string       `json:"error,omitempty"`
	ErrorCode    string       `json:"error_code,omitempty"`
	Root         RootView     `json:"root"`
	Notes        []string     `json:"notes,omitempty"`
	Verified     string       `json:"verified"`
	CreatedAt    time.Time    `json:"created_at"`
	ExpiresAt    time.Time    `json:"expires_at"`
}

// AppView describes the session's application snapshot.
type AppView struct {
	Mode     AppMode  `json:"mode"`
	Identity string   `json:"identity"`
	Scopes   []string `json:"scopes,omitempty"`
}

// OAuthView is the authorization step of an OAuth session.
type OAuthView struct {
	AuthorizeURL string `json:"authorize_url"`
	RedirectURI  string `json:"redirect_uri"`
	OOB          bool   `json:"oob"` // redirect_uri is "oob": the user pastes the code
}

// QRView is the scan step of a QR session.
type QRView struct {
	Status     QRStatus  `json:"status"`
	ImageURL   string    `json:"image_url,omitempty"`
	Content    string    `json:"content,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Message    string    `json:"message,omitempty"`
	CanRefresh bool      `json:"can_refresh"`
}

// RootView tells the storage step which root setting to collect.
type RootView struct {
	Field   string `json:"field"`
	Default string `json:"default"`
	Label   string `json:"label"`
	Help    string `json:"help,omitempty"`
}

// SaveRequest is what a SaveFunc receives: the adapter, the session's app
// snapshot, the staged credentials and the re-authorization target (nil when
// creating).
type SaveRequest struct {
	SessionID string
	Adapter   Adapter
	App       App
	Auth      *AuthConfig
	Target    *Target
}

// SaveFunc performs the write. The session manager calls it at most once per
// session at a time, with a context detached from the HTTP request so a
// disconnecting browser cannot abort a half-done write.
type SaveFunc func(ctx context.Context, req SaveRequest) SaveResult

// SaveResult is the outcome of one save, reported as it really happened.
type SaveResult struct {
	OK        bool   `json:"ok"`
	Mode      string `json:"mode"` // "create" | "reauth"
	StorageID uint   `json:"storage_id,omitempty"`
	MountPath string `json:"mount_path,omitempty"`
	Status    string `json:"status,omitempty"` // storage status after the save
	Message   string `json:"message"`          // Chinese summary for the page
	Error     string `json:"error,omitempty"`  // scrubbed underlying error
	// Retryable means nothing was written (input validation, mount path
	// taken, conflict detected before writing): the session stays authorized
	// and the user may correct the input and save again.
	Retryable bool `json:"retryable"`
	// CredentialsWritten reports whether the new credentials reached the
	// storage row at any point during this save, even if later undone.
	CredentialsWritten bool           `json:"credentials_written"`
	Cleanup            *CleanupReport `json:"cleanup,omitempty"`
	Restore            *RestoreReport `json:"restore,omitempty"`
}

// CleanupReport describes removing the storage a failed create left behind.
type CleanupReport struct {
	Attempted bool   `json:"attempted"`
	Deleted   bool   `json:"deleted"` // verified by re-reading the database
	StorageID uint   `json:"storage_id"`
	Error     string `json:"error,omitempty"`
}

// RestoreReport describes putting the original config back after a failed
// re-authorization.
type RestoreReport struct {
	Attempted bool `json:"attempted"`
	// Restored is true only when a re-read shows the original non-credential
	// settings back in place and the new credentials gone.
	Restored bool   `json:"restored"`
	Status   string `json:"status,omitempty"` // storage status after the restore
	Error    string `json:"error,omitempty"`
}
