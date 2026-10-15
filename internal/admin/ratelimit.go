package admin

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// maxLoginFailures is the number of failed portal logins one client IP
	// may accumulate within loginFailureWindow before further attempts are
	// answered with 429 (v0.3.1 hardening, UPGRADE §5.3).
	maxLoginFailures = 5
	// loginFailureWindow is the sliding window failures are counted in.
	loginFailureWindow = time.Minute
)

// loginLimiter counts failed logins per client IP in a sliding window. A
// successful login clears the client's counter.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: make(map[string][]time.Time)}
}

// allowed reports whether the client may attempt another login right now.
func (l *loginLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.windowLocked(ip, time.Now())) < maxLoginFailures
}

// recordFailure stores one failed attempt for the client.
func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.failures[ip] = append(l.windowLocked(ip, now), now)
}

// reset clears the client's failure history (successful login).
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// retryAfter returns how long the client should wait before the next
// attempt; 0 when the client is not blocked.
func (l *loginLimiter) retryAfter(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	window := l.windowLocked(ip, time.Now())
	if len(window) < maxLoginFailures {
		return 0
	}
	wait := loginFailureWindow - time.Since(window[0])
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// windowLocked prunes expired entries and returns the live ones.
func (l *loginLimiter) windowLocked(ip string, now time.Time) []time.Time {
	hits := l.failures[ip]
	live := hits[:0]
	for _, t := range hits {
		if now.Sub(t) < loginFailureWindow {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = live
	}
	return live
}

// clientIP extracts the remote address host for rate-limit keying. When the
// address cannot be parsed the raw string is used, so no client escapes the
// limiter.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// isMutating reports whether the method changes server state and therefore
// needs cross-origin protection.
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sameOrigin enforces the v0.3.1 admin CSRF rules: a mutating /admin/api/*
// request with an Origin header must carry the request's own host:port
// (scheme-agnostic), and without Origin a present Sec-Fetch-Site must be
// same-origin or none.
func sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		site = strings.ToLower(strings.TrimSpace(site))
		return site == "same-origin" || site == "none"
	}
	return true
}

// hasRequestBody reports whether the request carries a body (and therefore
// must declare its media type).
func hasRequestBody(r *http.Request) bool {
	return r.ContentLength > 0 || len(r.TransferEncoding) > 0
}

// jsonContentType reports whether the Content-Type is application/json
// (parameters such as charset are allowed).
func jsonContentType(r *http.Request) bool {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if ct == "" {
		return false
	}
	mt := ct
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt == "application/json"
}

// guardAdminAPI applies the v0.3.1 hardening to mutating /admin/api/*
// requests: cross-origin rejection and the JSON content-type requirement
// for requests carrying a body. It writes the rejection response and returns
// false when the request must not proceed.
func (p *Portal) guardAdminAPI(w http.ResponseWriter, r *http.Request) bool {
	if !isMutating(r.Method) {
		return true
	}
	if !sameOrigin(r) {
		p.log.Warn("rejected cross-origin admin request", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		respondError(w, http.StatusForbidden, "cross-origin admin API requests are not allowed")
		return false
	}
	if hasRequestBody(r) && !jsonContentType(r) {
		respondError(w, http.StatusUnsupportedMediaType, "admin API requests with a body must use Content-Type: application/json")
		return false
	}
	return true
}
