package openlist

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	for _, ep := range []string{"", "openlist:5244", "ftp://openlist:5244", "http://"} {
		if _, err := New(ep, "tok"); err == nil {
			t.Errorf("New(%q) succeeded, want error", ep)
		}
	}
	if _, err := New("http://openlist:5244", "tok"); err != nil {
		t.Errorf("New valid endpoint: %v", err)
	}
}

func TestNewRequestJoinsEndpointAndSetsAuth(t *testing.T) {
	c, err := New("http://openlist:5244/", "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	u := newRequestURL(t, c, "/api/fs/list")
	if u != "http://openlist:5244/api/fs/list" {
		t.Errorf("joined URL = %q", u)
	}
}

// newRequestURL builds a request via c.NewRequest and returns the final URL.
func newRequestURL(t *testing.T, c *Client, path string) string {
	t.Helper()
	req, err := c.NewRequest(context.Background(), http.MethodPost, mustURL(path), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "tok-1" {
		t.Errorf("Authorization = %q, want token", got)
	}
	return req.URL.String()
}

func mustURL(raw string) url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return *u
}

func TestDoSendsTokenAndBody(t *testing.T) {
	var gotAuth, gotPath, gotBody, gotCT string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	c, err := New(up.URL, "admin-token")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"path":"/x"}`)
	req, err := c.NewRequest(context.Background(), http.MethodPost, mustURL("/api/fs/list"), bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotAuth != "admin-token" {
		t.Errorf("upstream Authorization = %q", gotAuth)
	}
	if gotPath != "/api/fs/list" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("upstream Content-Type = %q", gotCT)
	}
	if gotBody != string(payload) {
		t.Errorf("upstream body = %q", gotBody)
	}
}

func TestDoPassesRedirectThrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	c, err := New(up.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	req, err := c.NewRequest(context.Background(), http.MethodGet, mustURL("/p/x"), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want redirect passed through", resp.StatusCode)
	}
}

func TestTimeouts(t *testing.T) {
	if DefaultTimeout != 120*time.Second {
		t.Errorf("DefaultTimeout = %v, want 120s", DefaultTimeout)
	}
	if RelaxedTimeout <= DefaultTimeout {
		t.Errorf("RelaxedTimeout = %v, must exceed DefaultTimeout", RelaxedTimeout)
	}
}

// --- token swap / self-heal (v0.3.1) ---

// selfHealUpstream fakes OpenList: it rejects any Authorization except the
// permanent settings token. By default it mimics the live instance, which
// answers HTTP 200 with a {"code":401} envelope; mode "http401" answers a
// plain HTTP 401 instead. /api/auth/login and
// /api/admin/setting/get?key=token implement the heal protocol.
type selfHealUpstream struct {
	mode           string // "" = envelope inside HTTP 200, "http401"
	mu             sync.Mutex
	authorizations []string // Authorization seen per data-plane request
	logins         int
	settingReads   int
}

func (u *selfHealUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/auth/login" && r.Method == http.MethodPost:
			u.logins++
			fmt.Fprintf(w, `{"code":200,"message":"success","data":{"token":"jwt-fresh"}}`)
		case r.URL.Path == "/api/admin/setting/get":
			u.settingReads++
			fmt.Fprintf(w, `{"code":200,"message":"success","data":{"key":"token","value":"openlist-permanent"}}`)
		default:
			auth := r.Header.Get("Authorization")
			u.authorizations = append(u.authorizations, auth)
			if auth != "openlist-permanent" {
				if u.mode == "http401" {
					w.WriteHeader(http.StatusUnauthorized)
				}
				fmt.Fprint(w, `{"code":401,"message":"token is invalidated","data":null}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"message":"success","data":{"path":"/"}}`)
		}
	})
}

func (u *selfHealUpstream) stats() (dataReqs, logins, settingReads int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.authorizations), u.logins, u.settingReads
}

func (u *selfHealUpstream) firstAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.authorizations) == 0 {
		return ""
	}
	return u.authorizations[0]
}

func newHealRequest(t *testing.T, c *Client) *http.Request {
	t.Helper()
	payload := []byte(`{"path":"/"}`)
	req, err := c.NewRequest(context.Background(), http.MethodPost, mustURL("/api/fs/list"), bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSelfHealSwapsTokenAndRetries(t *testing.T) {
	modes := []struct {
		name string
		mode string
	}{
		// The live OpenList answers HTTP 200 with a {"code":401} envelope.
		{"envelope-in-200", ""},
		{"plain-http-401", "http401"},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			up := &selfHealUpstream{mode: m.mode}
			ts := httptest.NewServer(up.handler())
			defer ts.Close()

			c, err := New(ts.URL, "stale-jwt")
			if err != nil {
				t.Fatal(err)
			}
			c.SetCredentials("admin", "pw")

			req := newHealRequest(t, c)
			resp, err := c.DoAutoHeal(req)
			if err != nil {
				t.Fatalf("DoAutoHeal: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 after heal+retry (body %s)", resp.StatusCode, readAll(t, resp.Body))
			}
			// Exactly one stale attempt, then one healed attempt.
			dataReqs, logins, settingReads := up.stats()
			if dataReqs != 2 {
				t.Errorf("upstream data-plane requests = %d, want 2 (stale + retry)", dataReqs)
			}
			if logins != 1 || settingReads != 1 {
				t.Errorf("logins = %d, setting reads = %d, want 1 and 1", logins, settingReads)
			}
			if got := c.Token(); got != "openlist-permanent" {
				t.Errorf("client token after heal = %q, want the permanent settings token", got)
			}
			if up.firstAuth() != "stale-jwt" {
				t.Errorf("first upstream auth = %q, want the stale token", up.firstAuth())
			}

			// Subsequent requests use the new token directly: no further logins.
			resp2, err := c.DoAutoHeal(newHealRequest(t, c))
			if err != nil {
				t.Fatal(err)
			}
			resp2.Body.Close()
			if _, logins, _ := up.stats(); logins != 1 {
				t.Errorf("logins = %d after second call, want 1 (token already healed)", logins)
			}
		})
	}
}

// readAll drains resp for an error message.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	return string(b)
}

func TestSelfHealDisabledWithoutCredentials(t *testing.T) {
	// Envelope form (HTTP 200 + code 401): passed through untouched, body
	// still readable by the caller thanks to the peek/restore.
	up := &selfHealUpstream{}
	ts := httptest.NewServer(up.handler())
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoAutoHeal(newHealRequest(t, c))
	if err != nil {
		t.Fatalf("DoAutoHeal: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (envelope form passes through)", resp.StatusCode)
	}
	if body := readAll(t, resp.Body); !strings.Contains(body, `"code":401`) {
		t.Errorf("body = %q, want the upstream 401 envelope verbatim", body)
	}
	if _, logins, _ := up.stats(); logins != 0 {
		t.Errorf("logins = %d, want 0 without credentials", logins)
	}
}

func TestSelfHealSingleFlight(t *testing.T) {
	up := &selfHealUpstream{}
	ts := httptest.NewServer(up.handler())
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.DoAutoHeal(newHealRequest(t, c))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	// One login heals the shared token; the other healers see the token
	// already replaced and skip their own login.
	if _, logins, _ := up.stats(); logins != 1 {
		t.Errorf("logins = %d, want 1 (single-flight heal)", logins)
	}
}

func TestSelfHealFailingLoginThrottles(t *testing.T) {
	// Login always fails (wrong password): the first heal fails, and a
	// second attempt within the cooldown fails fast without another login.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/login" {
			fmt.Fprint(w, `{"code":400,"message":"wrong password"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":401,"message":"token is invalidated","data":null}`)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "wrong")

	if _, err := c.DoAutoHeal(newHealRequest(t, c)); err == nil {
		t.Fatal("heal with wrong credentials must fail")
	}
	if _, err := c.DoAutoHeal(newHealRequest(t, c)); err == nil {
		t.Fatal("second heal within cooldown must fail")
	}
	if c.Token() != "stale-jwt" {
		t.Errorf("token must be unchanged after failed heal, got %q", c.Token())
	}
}

func TestSelfHealSkipsNonReplayableBody(t *testing.T) {
	// A streaming body (no GetBody) must not be retried: the rejection is
	// returned as-is.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":401,"message":"token is invalidated","data":null}`)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "stale-jwt")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	req, err := c.NewRequest(context.Background(), http.MethodPut, mustURL("/api/fs/put"), io.NopCloser(bytes.NewReader([]byte("x"))), -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.DoAutoHeal(req)
	if err != nil {
		t.Fatalf("DoAutoHeal: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 passed through for streaming bodies", resp.StatusCode)
	}
	if c.Token() != "stale-jwt" {
		t.Errorf("token must not change for non-replayable requests")
	}
}

func TestDoAutoHealPreservesSuccessfulResponses(t *testing.T) {
	// A successful JSON envelope must round-trip byte-identical through the
	// 401-peek logic, and a large non-JSON body must stream unbuffered.
	payload := []byte(`{"code":200,"message":"success","data":{"path":"/a","content":[{"name":"f"}]}}`)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			chunk := bytes.Repeat([]byte{0xA5}, 4096)
			for i := 0; i < 64; i++ { // 256 KiB, well past the peek window
				_, _ = w.Write(chunk)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("admin", "pw")

	resp, err := c.DoAutoHeal(newHealRequest(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp.Body); string(body) != string(payload) {
		t.Errorf("JSON body round-trip: got %q", body)
	}
	resp.Body.Close()

	req, err := c.NewRequest(context.Background(), http.MethodGet, mustURL("/big"), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = c.DoAutoHeal(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || n != 64*4096 {
		t.Errorf("streamed body: %d bytes, err %v; want %d", n, err, 64*4096)
	}
}

func TestSetTokenConcurrentUse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":200,"message":"success"}`)
	}))
	defer ts.Close()

	c, err := New(ts.URL, "tok-0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.SetToken(fmt.Sprintf("tok-%d", i))
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := c.NewRequest(context.Background(), http.MethodPost, mustURL("/api/fs/list"), bytes.NewReader([]byte(`{}`)), 2, nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	// After the storm the token must be one of the set values and readable.
	if tok := c.Token(); !strings.HasPrefix(tok, "tok-") {
		t.Errorf("token = %q, want one of the swapped values", tok)
	}
}

func TestNewRequestForwardsRelayHop(t *testing.T) {
	c, err := New("http://openlist:5244/", "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	// No hop on the context: no header.
	req, err := c.NewRequest(context.Background(), http.MethodGet, mustURL("/api/fs/get"), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(hopHeader); got != "" {
		t.Errorf("hop header %q present without a hop context", got)
	}
	if got := req.Header.Get("Authorization"); got != "tok-1" {
		t.Errorf("Authorization = %q, want token", got)
	}

	// WithHop on the context: the header rides to OpenList, where the GoAPI
	// driver's middleware publishes it for the driver to increment.
	req, err = c.NewRequest(WithHop(context.Background(), 3), http.MethodGet, mustURL("/api/fs/get"), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(hopHeader); got != "3" {
		t.Errorf("hop header = %q, want 3", got)
	}

	// Zero and negative hops are never sent.
	if WithHop(context.Background(), 0) == nil {
		t.Error("WithHop 0 must return a usable context")
	}
	req, err = c.NewRequest(WithHop(context.Background(), 0), http.MethodGet, mustURL("/api/fs/get"), nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(hopHeader); got != "" {
		t.Errorf("hop 0 must not be sent, got %q", got)
	}
}
