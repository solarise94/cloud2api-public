package loginsdk

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Defaults of the zero Config.
const (
	defaultPendingTTL    = 10 * time.Minute
	defaultAuthorizedTTL = 15 * time.Minute
	defaultResultTTL     = 30 * time.Minute
	defaultMaxPerOwner   = 5
	defaultMaxSessions   = 200
	defaultMinPollEvery  = 2 * time.Second
	defaultMaxQRRefresh  = 5
	defaultCallTimeout   = 30 * time.Second
	defaultSaveTimeout   = 3 * time.Minute

	// qrFallbackTTL bounds a QR code whose provider sent no expiry.
	qrFallbackTTL = 5 * time.Minute
	// maxProviderMessage caps (in runes) a provider failure message before it
	// is stored in a view.
	maxProviderMessage = 300
)

// busy markers: an adapter call or the SaveFunc of that session is in flight.
// They are only read and written while the session lock is held, and no lock
// is ever held across the call itself.
const (
	busyExchanging = "exchanging"
	busyPolling    = "polling"
	busySubmitting = "submitting"
	busySaving     = "saving"
)

// Log event names recorded on the session and flushed outside every lock, so
// a Logf callback can never deadlock against the manager.
const (
	eventAuthorized = "authorized"
	eventCancelled  = "cancelled"
	eventExpired    = "expired"
	eventFailed     = "failed"
	eventSaved      = "saved"
)

// Config bounds one Manager; the zero value of every field takes the default
// listed on it.
type Config struct {
	PendingTTL    time.Duration                    // default 10m: time to finish authorizing
	AuthorizedTTL time.Duration                    // default 15m: time to save after authorizing
	ResultTTL     time.Duration                    // default 30m: how long a finished session stays readable
	MaxPerOwner   int                              // default 5 live sessions (pending/authorized/saving) per Owner.UserID
	MaxSessions   int                              // default 200 sessions in memory (all phases)
	MinPollEvery  time.Duration                    // default 2s: floor between two PollQR calls of one session
	MaxQRRefresh  int                              // default 5 new QR codes per session
	CallTimeout   time.Duration                    // default 30s: timeout of each adapter call (AuthorizeURL excluded)
	SaveTimeout   time.Duration                    // default 3m: timeout of the SaveFunc
	Now           func() time.Time                 // default time.Now (tests inject a fake clock)
	Logf          func(format string, args ...any) // optional lifecycle log; never secrets
}

// StartInput opens one login session. Target nil means a new storage is being
// created; non-nil re-authorizes that storage.
type StartInput struct {
	Owner    Owner
	Platform string
	App      App     // app snapshot; ignored (replaced by App{Platform, Mode: AppNone}) when the adapter's AppModes is [AppNone]
	Target   *Target // nil = create a new storage; non-nil = re-authorize that storage
}

// CallbackOutcome is the browser-safe result page data of one OAuth callback.
type CallbackOutcome struct {
	SessionID    string
	PlatformName string
	Phase        Phase
	Message      string // Chinese, secret-free, for the callback result page
}

// Manager runs login sessions in memory. It never touches storage and never
// renews a token: a completed login stages an AuthConfig until a SaveFunc
// writes it, then wipes it.
type Manager struct {
	reg *Registry
	cfg Config

	// mu guards the two maps only. Lock order: mu before a session's mu, and
	// never the reverse; adapter calls and SaveFuncs run under no lock.
	mu       sync.Mutex
	sessions map[string]*session
	byState  map[string]string // oauth state -> session id
}

// NewManager returns a Manager over reg; zero cfg fields take the defaults.
func NewManager(reg *Registry, cfg Config) *Manager {
	if reg == nil {
		reg = NewRegistry()
	}
	if cfg.PendingTTL <= 0 {
		cfg.PendingTTL = defaultPendingTTL
	}
	if cfg.AuthorizedTTL <= 0 {
		cfg.AuthorizedTTL = defaultAuthorizedTTL
	}
	if cfg.ResultTTL <= 0 {
		cfg.ResultTTL = defaultResultTTL
	}
	if cfg.MaxPerOwner <= 0 {
		cfg.MaxPerOwner = defaultMaxPerOwner
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.MinPollEvery <= 0 {
		cfg.MinPollEvery = defaultMinPollEvery
	}
	if cfg.MaxQRRefresh <= 0 {
		cfg.MaxQRRefresh = defaultMaxQRRefresh
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultCallTimeout
	}
	if cfg.SaveTimeout <= 0 {
		cfg.SaveTimeout = defaultSaveTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{
		reg:      reg,
		cfg:      cfg,
		sessions: map[string]*session{},
		byState:  map[string]string{},
	}
}

// session is one login. Fields set at Start are immutable after the session
// enters m.sessions; every other field is read and written under mu only.
type session struct {
	mu sync.Mutex

	id      string
	owner   Owner
	adapter Adapter
	meta    Meta
	app     App
	target  *Target
	mode    string // "create" | "reauth"
	// method is the effective login method of the session's app mode
	// (Meta.MethodFor), fixed at Start.
	method Method

	phase     Phase
	errMsg    string
	errCode   string
	createdAt time.Time
	expiresAt time.Time
	busy      string

	state        string
	stateUsed    bool
	pkce         PKCE
	authorizeURL string

	qr          *QRCode
	qrStatus    QRStatus
	qrMessage   string
	qrExpires   time.Time
	qrEvery     time.Duration
	qrRefreshes int
	lastPoll    time.Time

	formFields []Field
	guide      *Guide

	auth    *AuthConfig
	summary *AuthSummary

	result   *SaveResult
	saveDone chan struct{}

	// pendingLog/pendingLogArg defer a lifecycle log line to the end of the
	// current manager call, after every lock has been released.
	pendingLog    string
	pendingLogArg string
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

func (m *Manager) logf(format string, args ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, args...)
	}
}

func (m *Manager) logEvent(s *session, event string) {
	m.logf("connect: session %.8s %s platform=%s mode=%s user=%s",
		s.id, event, s.meta.ID, s.mode, s.owner.Username)
}

func (m *Manager) flushLog(s *session) {
	s.mu.Lock()
	event, arg := s.pendingLog, s.pendingLogArg
	s.pendingLog, s.pendingLogArg = "", ""
	s.mu.Unlock()
	switch event {
	case "":
	case eventFailed:
		m.logf("connect: session %.8s failed platform=%s mode=%s user=%s error=%s",
			s.id, s.meta.ID, s.mode, s.owner.Username, arg)
	default:
		m.logEvent(s, event)
	}
}

// sweep applies the expiry rules. It runs at the start of every call; there
// is no background goroutine.
func (m *Manager) sweep() {
	now := m.now()
	var expired []*session
	m.mu.Lock()
	for id, s := range m.sessions {
		s.mu.Lock()
		if s.phase == PhaseSaving || !now.After(s.expiresAt) {
			s.mu.Unlock()
			continue
		}
		switch s.phase {
		case PhasePending, PhaseAuthorized:
			s.phase = PhaseExpired
			s.errMsg, s.errCode = "会话已过期", CodeExpired
			s.wipeAuthLocked()
			s.expiresAt = now.Add(m.cfg.ResultTTL)
			s.pendingLog = eventExpired
			state := s.state
			s.mu.Unlock()
			if state != "" {
				delete(m.byState, state)
			}
			expired = append(expired, s)
		default:
			state := s.state
			s.mu.Unlock()
			delete(m.sessions, id)
			if state != "" {
				delete(m.byState, state)
			}
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		m.flushLog(s)
	}
}

// find returns the session of id for owner, or a not-found error that is
// indistinguishable from an unknown id (another owner's session must not be
// revealed to exist).
func (m *Manager) find(owner Owner, id string) (*session, *Error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok || s.owner.UserID != owner.UserID || s.owner.TokenFP != owner.TokenFP {
		return nil, Errorf(CodeNotFound, "会话不存在")
	}
	return s, nil
}

func (m *Manager) sessionByID(id string) *session {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	return s
}

func (m *Manager) dropState(state, sid string) {
	if state == "" {
		return
	}
	m.mu.Lock()
	if cur, ok := m.byState[state]; ok && cur == sid {
		delete(m.byState, state)
	}
	m.mu.Unlock()
}

func isLivePhase(p Phase) bool {
	switch p {
	case PhasePending, PhaseAuthorized, PhaseSaving:
		return true
	}
	return false
}

func (m *Manager) liveCountLocked(userID uint) int {
	n := 0
	for _, s := range m.sessions {
		s.mu.Lock()
		if s.owner.UserID == userID && isLivePhase(s.phase) {
			n++
		}
		s.mu.Unlock()
	}
	return n
}

// dropFinishedLocked deletes finished sessions oldest-first until the map is
// below max. Callers hold m.mu.
func (m *Manager) dropFinishedLocked(max int) {
	if len(m.sessions) < max {
		return
	}
	type fin struct {
		id        string
		state     string
		createdAt time.Time
	}
	var fins []fin
	for id, s := range m.sessions {
		s.mu.Lock()
		if !isLivePhase(s.phase) {
			fins = append(fins, fin{id: id, state: s.state, createdAt: s.createdAt})
		}
		s.mu.Unlock()
	}
	sort.Slice(fins, func(i, j int) bool { return fins[i].createdAt.Before(fins[j].createdAt) })
	for _, f := range fins {
		if len(m.sessions) < max {
			break
		}
		delete(m.sessions, f.id)
		if f.state != "" {
			delete(m.byState, f.state)
		}
	}
}

// Start opens a login session. On any error nothing is stored.
func (m *Manager) Start(ctx context.Context, in StartInput) (View, error) {
	m.sweep()
	a, ok := m.reg.Get(in.Platform)
	if !ok {
		return View{}, Errorf(CodeNotFound, "未知的登录平台 %s", in.Platform)
	}
	meta := a.Meta()

	app := in.App
	if len(meta.AppModes) == 1 && meta.AppModes[0] == AppNone {
		app = App{Platform: in.Platform, Mode: AppNone}
	} else {
		if app.Platform != in.Platform {
			return View{}, Errorf(CodeAppInvalid, "应用配置与所选平台不一致")
		}
		if !containsAppMode(meta.AppModes, app.Mode) {
			return View{}, Errorf(CodeAppInvalid, "该平台不支持应用模式 %q", app.Mode)
		}
		if err := a.CheckApp(app); err != nil {
			return View{}, appInvalidErr(err)
		}
	}

	// Fast-fail an over-limit owner before any provider call; the binding
	// check runs again under the lock at insert time.
	m.mu.Lock()
	over := m.liveCountLocked(in.Owner.UserID) >= m.cfg.MaxPerOwner
	m.mu.Unlock()
	if over {
		return View{}, Errorf(CodeLimit, "进行中的登录会话过多，请先完成或取消已有会话")
	}

	now := m.now()
	s := &session{
		id:        NewSessionID(),
		owner:     in.Owner,
		adapter:   a,
		meta:      meta,
		app:       app,
		target:    in.Target,
		method:    meta.MethodFor(app.Mode),
		phase:     PhasePending,
		createdAt: now,
		expiresAt: now.Add(m.cfg.PendingTTL),
	}
	if in.Target != nil {
		s.mode = "reauth"
	} else {
		s.mode = "create"
	}

	switch s.method {
	case MethodOAuth:
		oa := a.(OAuthAdapter)
		s.state = NewState()
		if meta.PKCE {
			s.pkce = NewPKCE()
		}
		if app.RedirectURI == "oob" && !meta.AllowOOB {
			return View{}, Errorf(CodeAppInvalid, "该平台不支持 oob 回调地址")
		}
		u, err := oa.AuthorizeURL(app, s.state, s.pkce)
		if err != nil {
			return View{}, appInvalidErr(err)
		}
		s.authorizeURL = u
	case MethodQR:
		cctx, cancel := context.WithTimeout(orBackground(ctx), m.cfg.CallTimeout)
		qr, err := a.(QRAdapter).StartQR(cctx, app)
		cancel()
		if err != nil {
			return View{}, toProviderErr(err, app.ClientSecret)
		}
		s.setQRLocked(qr, m.cfg.MinPollEvery, now)
	case MethodForm:
		s.formFields = formFieldsFor(meta.FormFields, in.Target != nil)
		if fg, ok := a.(FormGuider); ok {
			s.guide = fg.FormGuide(app, in.Target != nil)
		}
	default:
		return View{}, Errorf(CodeInternal, "适配器 %s 的登录方式无效", meta.ID)
	}

	m.mu.Lock()
	if m.liveCountLocked(in.Owner.UserID) >= m.cfg.MaxPerOwner {
		m.mu.Unlock()
		return View{}, Errorf(CodeLimit, "进行中的登录会话过多，请先完成或取消已有会话")
	}
	m.dropFinishedLocked(m.cfg.MaxSessions)
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return View{}, Errorf(CodeLimit, "登录会话数已达上限，请稍后再试")
	}
	m.sessions[s.id] = s
	if s.state != "" {
		m.byState[s.state] = s.id
	}
	m.mu.Unlock()

	m.logEvent(s, "started")
	s.mu.Lock()
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// Get returns the current view. For a pending QR session it also polls the
// provider, at most once per poll interval and never concurrently.
func (m *Manager) Get(ctx context.Context, owner Owner, id string) (View, error) {
	m.sweep()
	s, err := m.find(owner, id)
	if err != nil {
		return View{}, err
	}
	defer m.flushLog(s)
	m.pollIfDue(ctx, s)
	s.mu.Lock()
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// pollIfDue performs one synchronous PollQR for a pending QR session when the
// interval has elapsed and no poll is in flight. The session lock is released
// for the call itself.
func (m *Manager) pollIfDue(ctx context.Context, s *session) {
	if s.method != MethodQR {
		return
	}
	now := m.now()
	s.mu.Lock()
	if s.phase != PhasePending || s.busy != "" {
		s.mu.Unlock()
		return
	}
	if now.After(s.qrExpires) && !qrTerminal(s.qrStatus) {
		s.qrStatus, s.qrMessage = QRExpired, ""
		s.mu.Unlock()
		return
	}
	if qrTerminal(s.qrStatus) {
		s.mu.Unlock()
		return
	}
	if now.Before(s.lastPoll.Add(s.qrEvery)) {
		s.mu.Unlock()
		return
	}
	s.busy = busyPolling
	s.lastPoll = now
	app, qr := s.app, s.qr
	adapter := s.adapter.(QRAdapter)
	secrets := s.secretsLocked()
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(orBackground(ctx), m.cfg.CallTimeout)
	poll, err := adapter.PollQR(cctx, app, qr)
	cancel()

	s.mu.Lock()
	if s.phase == PhasePending {
		m.applyPollLocked(s, poll, err, secrets)
	}
	s.busy = ""
	s.mu.Unlock()
}

// qrTerminal statuses end the QR flow: no further polls.
func qrTerminal(st QRStatus) bool {
	return st == QRConfirmed || st == QRCancelled || st == QRExpired
}

func (m *Manager) applyPollLocked(s *session, poll *QRPoll, err error, secrets []string) {
	scrubs := secrets
	if poll != nil && poll.Auth != nil {
		// The auth just obtained may name its own secrets (echoed in the
		// provider message); scrub with them too.
		scrubs = append(append([]string{}, secrets...), poll.Auth.Secrets...)
	}
	if err != nil {
		msg := Scrub(err.Error(), scrubs...)
		if CodeOf(err) == CodeNetwork {
			s.qrStatus, s.qrMessage = QRNetworkError, msg
			return
		}
		s.failLocked(msg, CodeOf(err))
		return
	}
	if poll == nil {
		s.failLocked("适配器没有返回扫码结果", CodeInternal)
		return
	}
	switch poll.Status {
	case QRWaiting, QRScanned:
		s.qrStatus = poll.Status
		// A poll with no message shows the code's own hint again; this also
		// clears the message of an earlier network error.
		s.qrMessage = Scrub(poll.Message, scrubs...)
		if s.qrMessage == "" && s.qr != nil {
			s.qrMessage = s.qr.Message
		}
	case QRExpired:
		// The code's hint (e.g. a user code to type) no longer applies.
		s.qrStatus, s.qrMessage = QRExpired, Scrub(poll.Message, scrubs...)
	case QRCancelled:
		msg := Scrub(poll.Message, scrubs...)
		if msg == "" {
			msg = "已在手机上取消扫码登录"
		}
		s.phase = PhaseCancelled
		s.errMsg, s.errCode = msg, CodeCancelled
		s.pendingLog = eventCancelled
	case QRConfirmed:
		if verr := m.stageAuthLocked(s, poll.Auth); verr != nil {
			s.failLocked(verr.Message, verr.Code)
			return
		}
		s.qrStatus = QRConfirmed
		s.qrMessage = Scrub(poll.Message, scrubs...)
	default:
		s.failLocked(fmt.Sprintf("适配器返回了未知的扫码状态 %q", poll.Status), CodeInternal)
	}
}

// stageAuthLocked validates and stages a login result, moving the session to
// PhaseAuthorized.
func (m *Manager) stageAuthLocked(s *session, auth *AuthConfig) *Error {
	if err := ValidateAuth(s.adapter, s.app, auth); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return e
		}
		return &Error{Code: CodeInternal, Message: err.Error()}
	}
	auth.Secrets = append(auth.Secrets, s.app.ClientSecret)
	s.auth = auth
	s.summary = auth.Summary()
	s.phase = PhaseAuthorized
	s.expiresAt = m.now().Add(m.cfg.AuthorizedTTL)
	s.pendingLog = eventAuthorized
	return nil
}

func (s *session) failLocked(msg, code string) {
	s.phase = PhaseFailed
	s.errMsg = truncateMessage(msg, maxProviderMessage)
	s.errCode = code
	s.pendingLog, s.pendingLogArg = eventFailed, s.errMsg
}

func (s *session) wipeAuthLocked() {
	if s.auth != nil {
		s.auth.Wipe()
	}
	s.auth = nil
}

// phaseErrLocked maps a non-pending phase to the error a state-changing call
// returns for it.
func phaseErrLocked(s *session) *Error {
	switch s.phase {
	case PhaseAuthorized:
		return Errorf(CodeConflict, "已完成授权")
	case PhaseExpired:
		return Errorf(CodeExpired, "会话已过期")
	case PhaseCancelled:
		return Errorf(CodeCancelled, "会话已取消")
	default:
		return Errorf(CodeConflict, "会话当前状态不允许该操作")
	}
}

// Submit carries one user input: form values, a pasted OAuth callback URL, or
// a pasted out-of-band code.
func (m *Manager) Submit(ctx context.Context, owner Owner, id string, values map[string]string) (View, error) {
	m.sweep()
	s, err := m.find(owner, id)
	if err != nil {
		return View{}, err
	}
	defer m.flushLog(s)

	s.mu.Lock()
	if s.phase != PhasePending {
		e := phaseErrLocked(s)
		s.mu.Unlock()
		return View{}, e
	}
	if s.busy != "" {
		s.mu.Unlock()
		return View{}, Errorf(CodeConflict, "正在处理上一次提交")
	}
	switch s.method {
	case MethodForm:
		return m.submitForm(ctx, s, values)
	case MethodOAuth:
		return m.submitOAuth(ctx, s, values)
	default:
		s.mu.Unlock()
		return View{}, Errorf(CodeInvalidInput, "扫码登录无需提交内容")
	}
}

// submitForm is called with s.mu held and releases it before returning.
func (m *Manager) submitForm(ctx context.Context, s *session, values map[string]string) (View, error) {
	s.busy = busySubmitting
	in := SubmitInput{App: s.app, Values: values, Reauth: s.target != nil}
	secrets := s.formSecretsLocked(values)
	adapter := s.adapter.(FormAdapter)
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(orBackground(ctx), m.cfg.CallTimeout)
	auth, err := adapter.Submit(cctx, in)
	cancel()

	s.mu.Lock()
	s.busy = ""
	if s.phase != PhasePending {
		e := phaseErrLocked(s)
		s.mu.Unlock()
		return View{}, e
	}
	if err != nil {
		s.mu.Unlock()
		return View{}, scrubbedErr(err, secrets)
	}
	if verr := m.stageAuthLocked(s, auth); verr != nil {
		s.failLocked(verr.Message, verr.Code)
		s.mu.Unlock()
		return View{}, verr
	}
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// submitOAuth is called with s.mu held and releases it before returning.
func (m *Manager) submitOAuth(ctx context.Context, s *session, values map[string]string) (View, error) {
	if raw := strings.TrimSpace(values["callback_url"]); raw != "" {
		p, err := ParseCallbackURL(raw)
		if err != nil {
			s.mu.Unlock()
			return View{}, err
		}
		if subtle.ConstantTimeCompare([]byte(p.State), []byte(s.state)) != 1 {
			s.mu.Unlock()
			return View{}, Errorf(CodeStateMismatch, "回调地址与当前会话不匹配")
		}
		if s.stateUsed {
			s.mu.Unlock()
			return View{}, Errorf(CodeStateMismatch, "该授权回调已处理过")
		}
		s.mu.Unlock()
		return m.completeOAuth(ctx, s, p)
	}
	if code := strings.TrimSpace(values["code"]); code != "" {
		if s.app.RedirectURI != "oob" {
			s.mu.Unlock()
			return View{}, Errorf(CodeInvalidInput, "该应用不支持授权码粘贴方式")
		}
		s.mu.Unlock()
		return m.completeOAuth(ctx, s, CallbackParams{Code: code})
	}
	s.mu.Unlock()
	return View{}, Errorf(CodeInvalidInput, "请粘贴回调地址或输入授权码")
}

// completeOAuth finishes a pending oauth session from callback params p; it
// takes the session lock itself and holds none while the adapter runs. The
// state is marked used (single use, even when the exchange fails) before the
// adapter call and dropped from the index once the outcome is applied.
func (m *Manager) completeOAuth(ctx context.Context, s *session, p CallbackParams) (View, error) {
	s.mu.Lock()
	if s.stateUsed {
		s.mu.Unlock()
		return View{}, Errorf(CodeStateMismatch, "该授权回调已处理过")
	}
	if s.phase != PhasePending || s.busy != "" {
		e := phaseErrLocked(s)
		s.mu.Unlock()
		return View{}, e
	}
	s.stateUsed = true
	s.busy = busyExchanging
	state := s.state
	secrets := s.secretsLocked()
	if p.Code != "" {
		secrets = append(secrets, p.Code)
	}
	app, pkce := s.app, s.pkce
	exchange := s.adapter.(OAuthAdapter)
	s.mu.Unlock()

	var (
		cancelled bool
		fail      *Error
		auth      *AuthConfig
	)
	switch {
	case p.Error == "access_denied":
		cancelled = true
	case p.Error != "":
		msg := "授权失败：" + p.Error
		if p.ErrorDescription != "" {
			msg += ": " + p.ErrorDescription
		}
		fail = Errorf(CodeProvider, "%s", truncateMessage(Scrub(msg, secrets...), maxProviderMessage))
	case p.Code == "":
		fail = Errorf(CodeProvider, "授权失败：提供商未返回授权码")
	default:
		cctx, cancelCtx := context.WithTimeout(orBackground(ctx), m.cfg.CallTimeout)
		var err error
		auth, err = exchange.Exchange(cctx, app, p.Code, pkce)
		cancelCtx()
		if err != nil {
			var e *Error
			code := CodeProvider
			if errors.As(err, &e) {
				code = e.Code
			}
			msg := Scrub(err.Error(), secrets...)
			fail = &Error{Code: code, Message: truncateMessage(msg, maxProviderMessage)}
		}
	}

	s.mu.Lock()
	applied := s.phase == PhasePending
	if applied {
		switch {
		case cancelled:
			s.phase = PhaseCancelled
			s.errMsg, s.errCode = "已在授权页面取消或拒绝授权", CodeCancelled
			s.pendingLog = eventCancelled
		case fail != nil:
			s.failLocked(fail.Message, fail.Code)
		default:
			if verr := m.stageAuthLocked(s, auth); verr != nil {
				s.failLocked(verr.Message, verr.Code)
				fail = verr
			}
		}
	}
	s.busy = ""
	s.mu.Unlock()
	m.dropState(state, s.id)

	if !applied {
		// The session changed while the exchange ran (cancelled or expired);
		// report its current state.
		s.mu.Lock()
		e := phaseErrLocked(s)
		s.mu.Unlock()
		return View{}, e
	}
	if fail != nil {
		return View{}, fail
	}
	s.mu.Lock()
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// Callback is the public OAuth redirect target; it resolves the session by
// the one-time state alone. A provider failure is a successful call whose
// outcome carries Phase failed; the error return is only for state problems.
func (m *Manager) Callback(ctx context.Context, q url.Values) (CallbackOutcome, error) {
	m.sweep()
	p := ParseCallbackQuery(q)
	if p.State == "" {
		return CallbackOutcome{}, errUnknownState()
	}
	m.mu.Lock()
	id, ok := m.byState[p.State]
	m.mu.Unlock()
	if !ok {
		return CallbackOutcome{}, errUnknownState()
	}
	s := m.sessionByID(id)
	if s == nil {
		return CallbackOutcome{}, errUnknownState()
	}
	defer m.flushLog(s)

	s.mu.Lock()
	used, phase := s.stateUsed, s.phase
	s.mu.Unlock()
	if used {
		return CallbackOutcome{}, Errorf(CodeStateMismatch, "该授权回调已处理过")
	}
	if phase != PhasePending {
		return CallbackOutcome{}, Errorf(CodeConflict, "该会话不在等待授权的状态")
	}
	_, _ = m.completeOAuth(ctx, s, p)

	s.mu.Lock()
	outPhase, errMsg := s.phase, s.errMsg
	s.mu.Unlock()
	return CallbackOutcome{
		SessionID:    s.id,
		PlatformName: s.meta.Name,
		Phase:        outPhase,
		Message:      callbackMessage(outPhase, errMsg),
	}, nil
}

func errUnknownState() *Error {
	return Errorf(CodeStateMismatch, "授权请求不存在或已过期，请回到登录页面重新开始")
}

func callbackMessage(p Phase, errMsg string) string {
	switch p {
	case PhaseAuthorized:
		return "授权完成，请回到登录配置页面继续"
	case PhaseCancelled:
		if errMsg != "" {
			return errMsg
		}
		return "已在授权页面取消或拒绝授权"
	case PhaseFailed:
		if errMsg != "" {
			return errMsg
		}
		return "授权失败"
	}
	return "授权请求已处理，请回到登录页面查看"
}

// RefreshQR issues a new QR code for a pending qrcode session.
func (m *Manager) RefreshQR(ctx context.Context, owner Owner, id string) (View, error) {
	m.sweep()
	s, err := m.find(owner, id)
	if err != nil {
		return View{}, err
	}
	defer m.flushLog(s)

	s.mu.Lock()
	if s.method != MethodQR {
		s.mu.Unlock()
		return View{}, Errorf(CodeInvalidInput, "该会话不是扫码登录")
	}
	if s.phase != PhasePending {
		e := phaseErrLocked(s)
		s.mu.Unlock()
		return View{}, e
	}
	if s.busy != "" {
		s.mu.Unlock()
		return View{}, Errorf(CodeConflict, "正在查询扫码状态，请稍后再试")
	}
	if s.qrRefreshes >= m.cfg.MaxQRRefresh {
		s.mu.Unlock()
		return View{}, Errorf(CodeLimit, "二维码刷新次数已达上限，请重新开始登录")
	}
	app := s.app
	adapter := s.adapter.(QRAdapter)
	secrets := s.secretsLocked()
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(orBackground(ctx), m.cfg.CallTimeout)
	qr, qerr := adapter.StartQR(cctx, app)
	cancel()
	if qerr != nil {
		return View{}, toProviderErr(qerr, secrets...)
	}

	s.mu.Lock()
	if s.phase == PhasePending {
		s.setQRLocked(qr, m.cfg.MinPollEvery, m.now())
		s.qrRefreshes++
	}
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// Cancel abandons a session. A finished session is returned unchanged.
func (m *Manager) Cancel(owner Owner, id string) (View, error) {
	m.sweep()
	s, err := m.find(owner, id)
	if err != nil {
		return View{}, err
	}
	defer m.flushLog(s)

	s.mu.Lock()
	switch s.phase {
	case PhasePending, PhaseAuthorized:
		// An adapter call may be in flight; its result is discarded by the
		// pending-phase check when it lands.
		s.phase = PhaseCancelled
		s.errMsg, s.errCode = "已取消登录", CodeCancelled
		s.wipeAuthLocked()
		s.pendingLog = eventCancelled
		state := s.state
		s.mu.Unlock()
		m.dropState(state, s.id)
		s.mu.Lock()
		v := s.viewLocked(m.cfg)
		s.mu.Unlock()
		return v, nil
	case PhaseSaving:
		s.mu.Unlock()
		return View{}, Errorf(CodeConflict, "正在保存，无法取消")
	default:
		v := s.viewLocked(m.cfg)
		s.mu.Unlock()
		return v, nil
	}
}

// Save runs fn once with the staged credentials. Concurrent and repeated
// calls wait and return the same result.
func (m *Manager) Save(ctx context.Context, owner Owner, id string, fn SaveFunc) (View, error) {
	m.sweep()
	s, err := m.find(owner, id)
	if err != nil {
		return View{}, err
	}
	defer m.flushLog(s)

	s.mu.Lock()
	switch s.phase {
	case PhaseSaved, PhaseSaveFailed:
		v := s.viewLocked(m.cfg)
		s.mu.Unlock()
		return v, nil
	case PhaseSaving:
		done := s.saveDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		v := s.viewLocked(m.cfg)
		s.mu.Unlock()
		return v, nil
	case PhasePending:
		s.mu.Unlock()
		return View{}, Errorf(CodeInvalidInput, "尚未完成授权")
	case PhaseCancelled:
		s.mu.Unlock()
		return View{}, Errorf(CodeCancelled, "会话已取消，无法保存")
	case PhaseExpired:
		s.mu.Unlock()
		return View{}, Errorf(CodeExpired, "会话已过期，无法保存")
	case PhaseFailed:
		s.mu.Unlock()
		return View{}, Errorf(CodeConflict, "登录已失败，无法保存")
	}
	if fn == nil {
		s.mu.Unlock()
		return View{}, Errorf(CodeInternal, "缺少保存函数")
	}
	s.phase = PhaseSaving
	s.busy = busySaving
	done := make(chan struct{})
	s.saveDone = done
	req := SaveRequest{SessionID: s.id, Adapter: s.adapter, App: s.app, Auth: s.auth, Target: s.target}
	secrets := s.secretsLocked()
	s.mu.Unlock()

	res := m.runSaveFn(ctx, fn, req, secrets)

	s.mu.Lock()
	if res.Mode == "" {
		res.Mode = s.mode
	}
	res.Message = Scrub(res.Message, secrets...)
	res.Error = Scrub(res.Error, secrets...)
	s.result = &res
	if res.Retryable {
		s.phase = PhaseAuthorized
		s.expiresAt = m.now().Add(m.cfg.AuthorizedTTL)
	} else {
		if res.OK {
			s.phase = PhaseSaved
			s.pendingLog = eventSaved
		} else {
			s.phase = PhaseSaveFailed
		}
		s.wipeAuthLocked()
		s.expiresAt = m.now().Add(m.cfg.ResultTTL)
	}
	s.busy = ""
	s.mu.Unlock()
	close(done)

	s.mu.Lock()
	v := s.viewLocked(m.cfg)
	s.mu.Unlock()
	return v, nil
}

// runSaveFn calls fn on a context detached from the request (a disconnecting
// browser must not abort a half-done write) and converts a panic into a
// failed SaveResult.
func (m *Manager) runSaveFn(ctx context.Context, fn SaveFunc, req SaveRequest, secrets []string) (res SaveResult) {
	defer func() {
		if r := recover(); r != nil {
			res = SaveResult{
				OK:      false,
				Message: "保存时发生内部错误",
				Error:   truncateMessage(Scrub(fmt.Sprintf("%v", r), secrets...), maxProviderMessage),
			}
		}
	}()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cfg.SaveTimeout)
	defer cancel()
	return fn(sctx, req)
}

func (s *session) setQRLocked(qr *QRCode, minEvery time.Duration, now time.Time) {
	s.qr = qr
	s.qrStatus = QRWaiting
	s.qrMessage = qr.Message
	s.qrExpires = qr.ExpiresAt
	if s.qrExpires.IsZero() {
		s.qrExpires = now.Add(qrFallbackTTL)
	}
	s.qrEvery = maxDuration(qr.PollEvery, minEvery)
	// The first poll is allowed one interval after the code was issued.
	s.lastPoll = now
}

// secretsLocked lists every secret value that must be scrubbed from messages
// of this session. extra adds one-off secrets (a submitted code or form
// values).
func (s *session) secretsLocked(extra ...string) []string {
	out := make([]string, 0, 4+len(extra))
	if s.app.ClientSecret != "" {
		out = append(out, s.app.ClientSecret)
	}
	if s.pkce.Verifier != "" {
		out = append(out, s.pkce.Verifier)
	}
	if s.auth != nil {
		out = append(out, s.auth.Secrets...)
	}
	return append(out, extra...)
}

// formSecretsLocked adds the secret form values of one submission.
func (s *session) formSecretsLocked(values map[string]string) []string {
	var extra []string
	for _, f := range s.meta.FormFields {
		if f.Secret {
			extra = append(extra, values[f.Key])
		}
	}
	return s.secretsLocked(extra...)
}

// viewLocked builds the browser-safe view; no credential value ever reaches
// it: the auth summary was captured when the login succeeded, and provider
// messages were scrubbed before being stored.
func (s *session) viewLocked(cfg Config) View {
	v := View{
		ID:           s.id,
		Platform:     s.meta.ID,
		PlatformName: s.meta.Name,
		Driver:       s.meta.Driver,
		Method:       s.method,
		Mode:         s.mode,
		Target:       s.target,
		Phase:        s.phase,
		App:          AppView{Mode: s.app.Mode, Identity: s.app.Identity(), Scopes: s.meta.Scopes},
		Auth:         s.summary,
		Result:       s.result,
		Error:        s.errMsg,
		ErrorCode:    s.errCode,
		Root:         RootView{Field: s.meta.RootField, Default: s.meta.RootDefault, Label: s.meta.RootLabel, Help: s.meta.RootHelp},
		Notes:        s.meta.Notes,
		Verified:     s.meta.Verified,
		CreatedAt:    s.createdAt,
		ExpiresAt:    s.expiresAt,
	}
	switch s.method {
	case MethodOAuth:
		v.OAuth = &OAuthView{
			AuthorizeURL: s.authorizeURL,
			RedirectURI:  s.app.RedirectURI,
			OOB:          s.app.RedirectURI == "oob",
		}
	case MethodQR:
		qr := QRView{
			Status:     s.qrStatus,
			ExpiresAt:  s.qrExpires,
			Message:    s.qrMessage,
			CanRefresh: s.phase == PhasePending && s.qrRefreshes < cfg.MaxQRRefresh,
		}
		if s.qr != nil {
			qr.ImageURL = s.qr.ImageURL
			qr.Content = s.qr.Content
		}
		v.QR = &qr
	case MethodForm:
		v.FormLabel = s.meta.FormLabel
		v.Guide = s.guide
		if len(s.formFields) > 0 {
			v.Form = append([]Field(nil), s.formFields...)
		}
	}
	return v
}

func containsAppMode(modes []AppMode, mode AppMode) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

func formFieldsFor(fields []Field, reauth bool) []Field {
	out := make([]Field, 0, len(fields))
	for _, f := range fields {
		if reauth && f.CreateOnly {
			continue
		}
		out = append(out, f)
	}
	return out
}

// appInvalidErr returns an adapter error as-is when it is already an *Error,
// and wraps anything else as app_invalid (a broken adapter must not leak an
// English error to the page).
func appInvalidErr(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeAppInvalid, Message: err.Error()}
}

// toProviderErr scrubs and types a provider-call error.
func toProviderErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return scrubbedErr(err, secrets)
}

func scrubbedErr(err error, secrets []string) error {
	var e *Error
	if errors.As(err, &e) {
		c := *e
		c.Message = Scrub(c.Message, secrets...)
		return &c
	}
	return &Error{Code: CodeProvider, Message: Scrub(err.Error(), secrets...)}
}

func truncateMessage(msg string, maxRunes int) string {
	if utf8.RuneCountInString(msg) <= maxRunes {
		return msg
	}
	return string([]rune(msg)[:maxRunes])
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
