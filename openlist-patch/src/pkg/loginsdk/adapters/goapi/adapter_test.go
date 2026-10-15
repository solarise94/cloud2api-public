package goapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// apeer is a minimal fake GoAPI peer for Submit: /v1/me plus optional root
// probes, with the hop capability advertised like a real patched peer.
type apeer struct {
	t *testing.T
	s *httptest.Server

	mu       sync.Mutex
	paths    []string
	meBody   string
	listOK   bool
	statOK   bool
	statuses map[string]int
}

func newAdapterPeer(t *testing.T) *apeer {
	t.Helper()
	p := &apeer{t: t, statuses: map[string]int{}}
	p.meBody = `{"key":{"id":"k","prefix":"sk-c2a-abcd","default_drive":"d1"},` +
		`"drives":[{"id":"d1","provider":"local","root":"/","perms":["list","read","write"],"health":"work"},` +
		`{"id":"d2","provider":"local","root":"/","perms":["list","read"],"health":"work"}],` +
		`"relay_hop_limit":4}`
	p.listOK, p.statOK = true, true
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		status := p.statuses[r.URL.Path]
		p.mu.Unlock()
		switch {
		case status != 0:
			w.WriteHeader(status)
		case r.URL.Path == "/v1/me":
			_, _ = w.Write([]byte(p.meBody))
		case strings.HasSuffix(r.URL.Path, "/stat"):
			if p.statOK {
				_, _ = w.Write([]byte(`{"path":"/","name":"","is_dir":true,"size":0,"modified":"","hashes":{}}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/list"):
			if p.listOK {
				_, _ = w.Write([]byte(`{"items":[],"total":0,"page":1,"per_page":1000}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	p.s = httptest.NewServer(mux)
	t.Cleanup(p.s.Close)
	return p
}

func (p *apeer) hit(suffix string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range p.paths {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func submit(t *testing.T, p *apeer, values map[string]string, reauth bool) (*loginsdk.AuthConfig, error) {
	t.Helper()
	fa, ok := New().(loginsdk.FormAdapter)
	if !ok {
		t.Fatal("the goapi adapter must be a FormAdapter")
	}
	return fa.Submit(context.Background(), loginsdk.SubmitInput{Values: values, Reauth: reauth})
}

func TestAdapterMetaValid(t *testing.T) {
	if err := loginsdk.Validate(New()); err != nil {
		t.Fatalf("adapter meta must validate: %v", err)
	}
	m := New().Meta()
	if m.ID != "goapi" || m.Driver != "GoAPI" || m.Method != loginsdk.MethodForm {
		t.Fatalf("unexpected meta %+v", m)
	}
}

func TestSubmitHappy(t *testing.T) {
	p := newAdapterPeer(t)
	auth, err := submit(t, p, map[string]string{"url": p.s.URL + "/", "api_key": "sk-c2a-secret", "drive": ""}, false)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if auth.Driver != "GoAPI" || auth.CredentialType != "api_key" {
		t.Fatalf("unexpected auth %+v", auth)
	}
	if auth.Fields["url"] != p.s.URL || auth.Fields["api_key"] != "sk-c2a-secret" {
		t.Fatalf("fields must carry url+key verbatim: %+v", auth.Fields)
	}
	if auth.CreateFields["drive"] != "d1" {
		t.Fatalf("resolved default drive must be d1 (inside grants), got %v", auth.CreateFields)
	}
	if len(auth.Secrets) != 1 || auth.Secrets[0] != "sk-c2a-secret" {
		t.Fatal("the key must be declared a secret for scrubbing")
	}
	if strings.Contains(auth.Account, "sk-c2a-secret") {
		t.Fatal("the account label must not contain the key")
	}
	if !p.hit("/stat") || !p.hit("/list") {
		t.Fatal("Submit must probe root stat+list")
	}
	// The bridge validation must accept the produced config.
	if err := loginsdk.ValidateAuth(New(), loginsdk.App{Mode: loginsdk.AppNone}, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestSubmitExplicitDrive(t *testing.T) {
	p := newAdapterPeer(t)
	if _, err := submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k", "drive": "d2"}, false); err != nil {
		t.Fatalf("granted explicit drive must pass: %v", err)
	}
	_, err := submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k", "drive": "dX"}, false)
	if err == nil || !strings.Contains(err.Error(), "dX") || !strings.Contains(err.Error(), "授权列表") {
		t.Fatalf("ungranted drive must be refused, got %v", err)
	}
	// Default drive outside grants, several drives: refuse, ask explicitly.
	p.meBody = strings.Replace(p.meBody, `"default_drive":"d1"`, `"default_drive":"nope"`, 1)
	_, err = submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k"}, false)
	if err == nil || !strings.Contains(err.Error(), "请填写上游网盘") {
		t.Fatalf("ambiguous default must ask for an explicit drive, got %v", err)
	}
}

func TestSubmitCapabilityAndCredentialFailures(t *testing.T) {
	p := newAdapterPeer(t)
	// Legacy peer without hop capability: refuse with the version message.
	p.meBody = strings.Replace(p.meBody, `,"relay_hop_limit":4}`, "}", 1)
	_, err := submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k"}, false)
	if err == nil || !strings.Contains(err.Error(), "串联跳数保护") {
		t.Fatalf("unpatched peer must be refused, got %v", err)
	}
	p.meBody = strings.TrimSuffix(p.meBody, "}") + `,"relay_hop_limit":4}`

	// Key rejected: 401 surfaces as a provider error, not a network one.
	p.statuses["/v1/me"] = http.StatusUnauthorized
	_, err = submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k"}, false)
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("rejected key must say so, got %v", err)
	}
	delete(p.statuses, "/v1/me")

	// No drives granted.
	p.meBody = strings.Replace(p.meBody, `"drives":[{"id":"d1","provider":"local","root":"/","perms":["list","read","write"],"health":"work"},`+
		`{"id":"d2","provider":"local","root":"/","perms":["list","read"],"health":"work"}],`, `"drives":[],`, 1)
	_, err = submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k"}, false)
	if err == nil || !strings.Contains(err.Error(), "未被授予任何网盘") {
		t.Fatalf("empty grants must be refused, got %v", err)
	}

	// Invalid endpoint values.
	for _, bad := range []string{"", "ftp://x", "not a url"} {
		if _, err := submit(t, p, map[string]string{"url": bad, "api_key": "k"}, false); err == nil {
			t.Fatalf("endpoint %q must be rejected", bad)
		}
	}
}

func TestSubmitReauth(t *testing.T) {
	p := newAdapterPeer(t)
	auth, err := submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k2"}, true)
	if err != nil {
		t.Fatalf("reauth Submit: %v", err)
	}
	if auth.CreateFields != nil {
		t.Fatal("reauth must not touch create-only fields (drive)")
	}
	if auth.Fields["url"] != p.s.URL || auth.Fields["api_key"] != "k2" {
		t.Fatal("reauth fields must replace url+key")
	}
	// Reauth validates the key via /v1/me only; no root probes on the
	// stored (unknown here) drive.
	if p.hit("/stat") || p.hit("/list") {
		t.Fatal("reauth must not probe the drive root")
	}
	if err := loginsdk.ValidateAuth(New(), loginsdk.App{Mode: loginsdk.AppNone}, auth); err != nil {
		t.Fatalf("ValidateAuth: %v", err)
	}
}

func TestSubmitNoRootListPermission(t *testing.T) {
	p := newAdapterPeer(t)
	p.listOK = false
	_, err := submit(t, p, map[string]string{"url": p.s.URL, "api_key": "k"}, false)
	if err == nil {
		t.Fatal("a key without list permission must be refused at Submit")
	}
}
