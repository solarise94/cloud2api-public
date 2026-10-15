package goapiclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/crc64"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// record is one observed peer request.
type record struct {
	Method   string
	Path     string
	Query    string
	Hop      string
	APIKey   string
	RangeHdr string
	Body     []byte
}

// peer is a configurable fake GoAPI /v1 peer with contract-faithful ingress:
// it enforces the hop cap and advertises relay_hop_limit like a real patched
// cloud2api does.
type peer struct {
	t *testing.T
	s *httptest.Server

	mu       sync.Mutex
	requests []record

	key         string
	meBody      string
	listBody    string
	statBody    string
	putBody     string
	putStatus   int
	filesBody   []byte
	filesStatus int
	redirectTo  string // when set, every endpoint 302s there
}

func newPeer(t *testing.T) *peer {
	t.Helper()
	p := &peer{t: t, key: "sk-c2a-test-key-000000"}
	p.meBody = `{"key":{"id":"k1","prefix":"sk-c2a-abcd","default_drive":"d1"},` +
		`"drives":[{"id":"d1","provider":"local","root":"/","perms":["list","read","write"],"health":"work"}],` +
		`"relay_hop_limit":4}`
	p.statBody = `{"path":"/","name":"","is_dir":true,"size":0,"modified":"2026-10-14T00:00:00Z","hashes":{}}`
	p.listBody = `{"items":[{"name":"hello.txt","path":"/hello.txt","is_dir":false,"size":5,"modified":"2026-10-14T00:00:00Z","hashes":{}}],"total":1,"page":1,"per_page":1000}`
	p.putStatus = 0 // computed per request unless scripted
	p.filesStatus = 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)
	p.s = httptest.NewServer(mux)
	t.Cleanup(p.s.Close)
	return p
}

func (p *peer) observe(r *http.Request) record {
	// Only multipart bodies are recorded (they are small in these tests);
	// raw PUT bodies are left for the handler so receipts are computed over
	// the real bytes.
	var body []byte
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 16<<20))
		// The handler reads the body again: replay what was recorded. Raw
		// bodies are left untouched for the handler.
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	rec := record{
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    r.URL.RawQuery,
		Hop:      r.Header.Get(HopHeader),
		APIKey:   r.Header.Get("apikey"),
		RangeHdr: r.Header.Get("Range"),
		Body:     body,
	}
	p.mu.Lock()
	p.requests = append(p.requests, rec)
	p.mu.Unlock()
	return rec
}

func (p *peer) writeJSON(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, s)
}

func (p *peer) handle(w http.ResponseWriter, r *http.Request) {
	p.observe(r)
	if p.redirectTo != "" {
		http.Redirect(w, r, p.redirectTo, http.StatusFound)
		return
	}
	// Contract ingress: exactly one well-formed hop header, at most MaxHops.
	hop, err := ParseHop(r.Header.Values(HopHeader))
	if err != nil {
		status := http.StatusBadRequest
		if err == ErrHopExceeded {
			status = http.StatusForbidden
		}
		p.writeJSON(w, status, `{"error":{"code":"hop_invalid","message":"invalid relay hop header"}}`)
		return
	}
	_ = hop
	if r.Header.Get("apikey") != p.key {
		p.writeJSON(w, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"missing or invalid API key"}}`)
		return
	}
	switch {
	case r.URL.Path == "/v1/me":
		p.writeJSON(w, http.StatusOK, p.meBody)
	case r.URL.Path == "/v1/drives/d1/list":
		p.writeJSON(w, http.StatusOK, p.listBody)
	case r.URL.Path == "/v1/drives/d1/stat":
		p.writeJSON(w, http.StatusOK, p.statBody)
	case r.URL.Path == "/v1/drives/d1/mkdir":
		p.writeJSON(w, http.StatusOK, `{"path":"/newdir","created":true}`)
	case r.URL.Path == "/v1/drives/d1/move":
		p.writeJSON(w, http.StatusOK, `{"from":"/a","to":"/b"}`)
	case r.URL.Path == "/v1/drives/d1/delete":
		p.writeJSON(w, http.StatusOK, `{"results":[{"path":"/hello.txt","status":"deleted"}]}`)
	case r.URL.Path == "/v1/drives/d1/files" && r.Method == http.MethodPut:
		p.handlePut(w, r)
	case r.URL.Path == "/v1/drives/d1/files" && r.Method == http.MethodGet:
		p.handleFilesGet(w, r)
	default:
		p.writeJSON(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"no route"}}`)
	}
}

// handlePut computes the receipt over exactly the bytes it received.
func (p *peer) handlePut(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") && r.ContentLength <= 0 {
		p.writeJSON(w, http.StatusLengthRequired, `{"error":{"code":"length_required","message":"raw-body uploads must carry a positive Content-Length"}}`)
		return
	}
	var data []byte
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			p.writeJSON(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"boundary"}}`)
			return
		}
		form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(64 << 20)
		if err != nil {
			p.writeJSON(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"form"}}`)
			return
		}
		defer func() { _ = form.RemoveAll() }()
		f, err := form.File["file"][0].Open()
		if err != nil {
			p.writeJSON(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"part"}}`)
			return
		}
		defer f.Close() //nolint:errcheck
		data, _ = io.ReadAll(f)
	} else {
		var err error
		data, err = io.ReadAll(r.Body)
		if err != nil {
			p.writeJSON(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"read failed"}}`)
			return
		}
	}
	if p.putStatus != 0 {
		p.writeJSON(w, p.putStatus, p.putBody)
		return
	}
	if p.putBody != "" {
		p.writeJSON(w, http.StatusOK, p.putBody)
		return
	}
	path := r.URL.Query().Get("path")
	crc := crc64.New(crc64Table)
	sha := sha256.Sum256(data)
	crc.Write(data) //nolint:errcheck
	receipt := map[string]any{
		"path":     path,
		"size":     len(data),
		"crc64":    strconv.FormatUint(crc.Sum64(), 10),
		"sha256":   hex.EncodeToString(sha[:]),
		"conflict": "created",
	}
	out, _ := json.Marshal(receipt)
	p.writeJSON(w, http.StatusOK, string(out))
}

func (p *peer) handleFilesGet(w http.ResponseWriter, _ *http.Request) {
	if p.filesStatus != 0 || p.filesBody != nil {
		status := p.filesStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(p.filesBody)
		return
	}
	p.writeJSON(w, http.StatusOK, "hello")
}

func (p *peer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *peer) last() record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

// deleteCalls counts delete endpoints hit (for the no-compensating-delete
// assertions).
func (p *peer) deleteCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.requests {
		if strings.HasSuffix(r.Path, "/delete") {
			n++
		}
	}
	return n
}

func testClient(t *testing.T, p *peer) *Client {
	t.Helper()
	c, err := New(p.s.URL, p.key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsBadEndpoint(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "http:///nohost", "http://user:pw@host", "   "} {
		if _, err := New(bad, "k"); err == nil {
			t.Fatalf("New(%q) must fail", bad)
		}
	}
	if _, err := New("https://ok.example", ""); err == nil {
		t.Fatal("New without key must fail")
	}
}

func TestMeAndCapabilityGate(t *testing.T) {
	p := newPeer(t)
	c := testClient(t, p)
	me, err := c.Me(t.Context())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if err := me.ValidateRelayCapability(); err != nil {
		t.Fatalf("patched peer must pass the capability gate: %v", err)
	}
	if !me.HasDrive("d1") || me.HasDrive("d2") {
		t.Fatal("HasDrive misbehaves")
	}
	// A legacy peer without the capability must be refused.
	p.meBody = strings.Replace(p.meBody, `,"relay_hop_limit":4}`, "}", 1)
	me, err = c.Me(t.Context())
	if err != nil {
		t.Fatalf("Me legacy: %v", err)
	}
	if err := me.ValidateRelayCapability(); err == nil {
		t.Fatal("unpatched peer (no relay_hop_limit) must fail the capability gate")
	}
	p.meBody = strings.TrimSuffix(p.meBody, "}") + `,"relay_hop_limit":2}`
	me, _ = c.Me(t.Context())
	if err := me.ValidateRelayCapability(); err == nil {
		t.Fatal("mismatched hop limit must fail the capability gate")
	}
}

func TestKeyOnlyInHeaderNeverInURL(t *testing.T) {
	p := newPeer(t)
	c := testClient(t, p)
	if _, err := c.Stat(t.Context(), "d1", "/"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if p.count() == 0 {
		t.Fatal("the peer must have observed the call")
	}
	for _, r := range p.requests {
		if strings.Contains(r.Query, p.key) || strings.Contains(r.Path, p.key) {
			t.Fatal("apikey must never travel in a URL")
		}
		if r.APIKey != p.key {
			t.Fatalf("peer requests must carry the apikey header (path %s got %q)", r.Path, r.APIKey)
		}
	}
}

func TestRedirectRefusedAndKeyNotLeaked(t *testing.T) {
	var targetMu sync.Mutex
	var targetReqs int
	var targetAuth []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetMu.Lock()
		targetReqs++
		targetAuth = append(targetAuth, r.Header.Get("apikey"), r.Header.Get("Authorization"))
		targetMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	p := newPeer(t)
	p.redirectTo = target.URL + "/steal"
	c := testClient(t, p)
	if _, err := c.Stat(t.Context(), "d1", "/"); err == nil {
		t.Fatal("a redirected metadata call must fail")
	} else if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect must surface as typed error, got %v", err)
	}
	if _, err := c.Download(t.Context(), "d1", "/x", ""); err == nil {
		t.Fatal("a redirected download must fail")
	}
	targetMu.Lock()
	defer targetMu.Unlock()
	if targetReqs != 0 {
		t.Fatalf("the redirect target must never be contacted, got %d requests", targetReqs)
	}
	for _, a := range targetAuth {
		if a != "" {
			t.Fatal("credential reached the redirect target")
		}
	}
}

func TestUploadReceiptRoundTripRawAndMultipart(t *testing.T) {
	p := newPeer(t)
	c := testClient(t, p)
	ctx := t.Context()

	// Raw body, known size.
	payload := []byte("hello relay")
	r, err := c.Upload(ctx, "d1", "/hello.txt", "hello.txt", "fail", int64(len(payload)), strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Upload raw: %v", err)
	}
	crc := crc64.New(crc64Table)
	crc.Write(payload) //nolint:errcheck
	sha := sha256.Sum256(payload)
	if err := VerifyReceipt(r, "/hello.txt", int64(len(payload)), crc.Sum64(), sha[:]); err != nil {
		t.Fatalf("receipt must verify: %v", err)
	}
	if last := p.last(); last.Hop != "" {
		t.Fatal("no hop header expected without relay context")
	}
	if !strings.Contains(p.last().Query, "on_conflict=fail") {
		t.Fatal("default upload must request on_conflict=fail")
	}

	// Multipart, zero-byte file.
	r, err = c.Upload(ctx, "d1", "/empty.txt", "empty.txt", "fail", 0, strings.NewReader(""))
	if err != nil {
		t.Fatalf("Upload zero-byte: %v", err)
	}
	emptySum := sha256.Sum256(nil)
	if err := VerifyReceipt(r, "/empty.txt", 0, crc64.Checksum(nil, crc64Table), emptySum[:]); err != nil {
		t.Fatalf("zero-byte receipt must verify: %v", err)
	}

	// Multipart with unknown size (-1).
	r, err = c.Upload(ctx, "d1", "/u.txt", "u.txt", "fail", -1, strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("Upload unknown size: %v", err)
	}
	if r.Size == nil || *r.Size != 3 {
		t.Fatalf("receipt size must be 3, got %v", r.Size)
	}

	// Overwrite flag is threaded through.
	if _, err = c.Upload(ctx, "d1", "/hello.txt", "hello.txt", "overwrite", int64(len(payload)), strings.NewReader(string(payload))); err != nil {
		t.Fatalf("Upload overwrite: %v", err)
	}
	if !strings.Contains(p.last().Query, "on_conflict=overwrite") {
		t.Fatal("overwrite conflict must be requested")
	}
}

func TestUploadNonconformingReceiptsFail(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"garbage 200", http.StatusOK, "ok"},
		{"missing hashes", http.StatusOK, `{"path":"/f","size":5}`},
		{"task echo is not a receipt", http.StatusOK, `{"code":0,"message":"","data":{"task":"t1"}}`},
		{"http 500 envelope", http.StatusInternalServerError, `{"error":{"code":"upstream_error","message":"boom"}}`},
		{"truncated json", http.StatusOK, `{"path":"/f","size":5,"crc64":"1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPeer(t)
			p.putStatus = tc.status
			p.putBody = tc.body
			c := testClient(t, p)
			if _, err := c.Upload(t.Context(), "d1", "/f", "f", "fail", 5, strings.NewReader("hello")); err == nil {
				t.Fatal("non-conforming receipt must fail")
			}
			if p.deleteCalls() != 0 {
				t.Fatal("the client must never compensate with a delete")
			}
		})
	}
}

func TestUploadSizeMismatchFails(t *testing.T) {
	p := newPeer(t)
	// The scripted receipt lies about the size the peer received.
	p.putBody = `{"path":"/f","size":4,"crc64":"` +
		strconv.FormatUint(crc64.Checksum([]byte("helo"), crc64Table), 10) + `","sha256":"` +
		sha256Hex("helo") + `","conflict":"created"}`
	c := testClient(t, p)
	r, err := c.Upload(t.Context(), "d1", "/f", "f", "fail", 4, strings.NewReader("helo"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	err = VerifyReceipt(r, "/f", 5, crc64.Checksum([]byte("hello"), crc64Table), sha256Sum("hello"))
	if err == nil {
		t.Fatal("mismatched receipt must fail verification")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("no compensating delete allowed")
	}
}

func TestUploadInterruptedFails(t *testing.T) {
	var once sync.Once
	p := newPeer(t)
	srv := p.s
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain a few bytes then sever the connection mid-upload.
		buf := make([]byte, 4)
		_, _ = io.ReadFull(r.Body, buf)
		once.Do(func() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
	})
	p.s = srv
	c := testClient(t, p)
	if _, err := c.Upload(t.Context(), "d1", "/f", "f", "fail", 100, strings.NewReader(strings.Repeat("x", 100))); err == nil {
		t.Fatal("an interrupted upload must fail (unconfirmed)")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("no compensating delete after disconnect")
	}
}

func TestHopPropagationOutbound(t *testing.T) {
	p := newPeer(t)
	c := testClient(t, p)
	ctx := WithHop(t.Context(), 2)
	if _, err := c.Stat(ctx, "d1", "/"); err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := p.last().Hop; got != "3" {
		t.Fatalf("peer saw hop %q, want 3", got)
	}
	// A hop above the cap is rejected by the peer ingress and surfaces here.
	p2 := newPeer(t)
	c2 := testClient(t, p2)
	if _, err := c2.Stat(WithHop(t.Context(), MaxHops+1), "d1", "/"); err == nil {
		t.Fatal("peer must refuse a hop above the cap")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected the peer's 403, got %v", err)
	}
}

// TestTwoHopChainBoundIsEnforced walks an actual HTTP chain: client -> fake
// peer B, whose handler performs its own goapiclient call into fake peer A
// (exactly what the real driver does inside OpenList). The counter grows per
// transit and the chain is cut at the cap.
func TestTwoHopChainBoundIsEnforced(t *testing.T) {
	a := newPeer(t)
	ca := testClient(t, a)

	b := newPeer(t)
	bMux := http.NewServeMux()
	bMux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		b.observe(r)
		if _, err := ParseHop(r.Header.Values(HopHeader)); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b.writeJSON(w, http.StatusOK, b.meBody)
	})
	bMux.HandleFunc("/v1/drives/d1/files", func(w http.ResponseWriter, r *http.Request) {
		rec := b.observe(r)
		hop, err := ParseHop(r.Header.Values(HopHeader))
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// Peer B acts as a driver: it calls A with its own context hop.
		resp, err := ca.Download(WithHop(r.Context(), hop), "d1", "/chain", "")
		if err != nil {
			b.writeJSON(w, http.StatusBadGateway, `{"error":{"code":"upstream_error","message":"inner chain failed"}}`)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		inner, _ := io.ReadAll(resp.Body)
		_ = rec
		b.writeJSON(w, http.StatusOK, string(inner))
	})
	b.s.Config.Handler = bMux

	cb := testClient(t, b)
	// One transit through B: the entry client marks hop 0, B's ingress reads
	// 1 on the wire and its own relay (the nested driver) sends 2 to A.
	if _, err := cb.Download(WithHop(t.Context(), 0), "d1", "/x", ""); err != nil {
		t.Fatalf("chain call: %v", err)
	}
	aHopA := ""
	aMu := func() []record { a.mu.Lock(); defer a.mu.Unlock(); return append([]record(nil), a.requests...) }()
	for _, r := range aMu {
		if r.Hop != "" {
			aHopA = r.Hop
		}
	}
	if aHopA != "2" {
		t.Fatalf("inner peer saw hop %q, want 2", aHopA)
	}

	// Bound: a chain already at the cap is refused at B's ingress (the wire
	// value would be MaxHops+1), surfacing as a failure at the entry client.
	if _, err := cb.Download(WithHop(t.Context(), MaxHops), "d1", "/x", ""); err == nil {
		t.Fatal("a chain at the hop cap must be cut")
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestStatusErrorEnvelope(t *testing.T) {
	p := newPeer(t)
	p.putStatus = http.StatusNotFound
	p.putBody = `{"error":{"code":"path_not_found","message":"no such path","retryable":false}}`
	c := testClient(t, p)
	_, err := c.Upload(t.Context(), "d1", "/no/such/dir/f", "f", "fail", 5, strings.NewReader("hello"))
	var pe *Error
	if err == nil {
		t.Fatal("expected error")
	}
	if e, ok := err.(*Error); ok {
		pe = e
	} else {
		t.Fatalf("want *Error, got %T", err)
	}
	if !pe.NotFound || pe.Code != "path_not_found" || pe.Status != http.StatusNotFound {
		t.Fatalf("typed error misdecoded: %+v", pe)
	}
}

func TestEndpointRejectsStructuredURLs(t *testing.T) {
	for _, bad := range []string{
		"https://ok.example?next=/admin", "https://ok.example#frag", "https://ok.example/path?#",
		"https://ok.example/%2e%2e/etc", "https://ok.example/a%2fb", "https://ok.example/x/../../y",
	} {
		if _, err := New(bad, "k"); err == nil {
			t.Fatalf("New(%q) must be rejected", bad)
		}
	}
}

func TestMeRejectsDuplicateAndEmptyDriveIDs(t *testing.T) {
	p := newPeer(t)
	p.meBody = `{"key":{"id":"k","default_drive":"d1"},"drives":[{"id":"d1"},{"id":"d1"}],"relay_hop_limit":4}`
	c := testClient(t, p)
	if _, err := c.Me(t.Context()); err == nil {
		t.Fatal("duplicate granted ids must be rejected")
	}
	p.meBody = `{"key":{"id":"k","default_drive":"d1"},"drives":[{"id":""},{"id":"d1"}],"relay_hop_limit":4}`
	if _, err := c.Me(t.Context()); err == nil {
		t.Fatal("an empty granted id must be rejected")
	}
	p.meBody = `{"key":{"id":"k","default_drive":"d1"},"drives":[{"id":"../up"}],"relay_hop_limit":4}`
	if _, err := c.Me(t.Context()); err != nil {
		t.Fatalf("me decode itself accepts the id: %v", err)
	}
	// Using it as a drive must be an error, not a panic or an escape.
	if _, err := c.Stat(t.Context(), "../up", "/"); err == nil {
		t.Fatal("a drive id with separators must be refused")
	}
}

func TestStatPathMismatchRejected(t *testing.T) {
	p := newPeer(t)
	p.statBody = `{"path":"/somewhere/else","name":"x","is_dir":true,"size":0,"modified":"","hashes":{}}`
	c := testClient(t, p)
	if _, err := c.Stat(t.Context(), "d1", "/"); err == nil {
		t.Fatal("a stat answer for another path must be rejected")
	}
}

func TestMkdirUntypedResponseRejected(t *testing.T) {
	p := newPeer(t)
	// A bare {} or an AList-style error envelope is not a typed answer.
	for _, body := range []string{`{}`, `{"code":200,"message":"success"}`} {
		seen := false
		p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.observe(r)
			if strings.HasSuffix(r.URL.Path, "/mkdir") && !seen {
				seen = true
				p.writeJSON(w, http.StatusOK, body)
				return
			}
			p.writeJSON(w, http.StatusOK, `{"error":{"code":"path_not_found","message":"stop"}}`)
		})
		c := testClient(t, p)
		if err := c.Mkdir(t.Context(), "d1", "/new"); err == nil {
			t.Fatalf("mkdir body %s must be rejected as untyped", body)
		}
	}
}

func TestReceiptRequiresHTTP200AndTypedFields(t *testing.T) {
	// 202 with a plausible receipt is a queued upload, never a confirmation.
	p := newPeer(t)
	p.putStatus = http.StatusAccepted
	p.putBody = `{"path":"/f","size":5,"crc64":"11051210869376104954","sha256":"` + sha256Hex("hello") + `","conflict":"created"}`
	c := testClient(t, p)
	if _, err := c.Upload(t.Context(), "d1", "/f", "f", "fail", 5, strings.NewReader("hello")); err == nil {
		t.Fatal("202 must not confirm an upload")
	}
	// Zero-size must be explicit: a receipt without the size field fails.
	p.putStatus = http.StatusOK
	p.putBody = `{"path":"/e","crc64":"0","sha256":"` + sha256Hex("") + `","conflict":"created"}`
	if _, err := c.Upload(t.Context(), "d1", "/e", "e", "fail", 0, strings.NewReader("")); err == nil {
		t.Fatal("a receipt without an explicit size must fail")
	}
	// Conflict outside the enum fails.
	p.putBody = `{"path":"/e","size":0,"crc64":"0","sha256":"` + sha256Hex("") + `","conflict":"exploded"}`
	if _, err := c.Upload(t.Context(), "d1", "/e", "e", "fail", 0, strings.NewReader("")); err == nil {
		t.Fatal("an unknown conflict value must fail")
	}
}

func TestErrorCodeScrubbedOfKey(t *testing.T) {
	p := newPeer(t)
	p.putStatus = http.StatusBadRequest
	p.putBody = `{"error":{"code":"sk-c2a-test-key-000000 leaked","message":"body mentions sk-c2a-test-key-000000 too"}}`
	c := testClient(t, p)
	_, err := c.Upload(t.Context(), "d1", "/f", "f", "fail", 5, strings.NewReader("hello"))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), p.key) {
		t.Fatal("peer-controlled error text must be scrubbed of the key")
	}
}

func TestUploadEarlyPeerResponseStaysUnconfirmed(t *testing.T) {
	// The peer answers 200 with a well-formed receipt and immediately closes
	// without reading the multipart body: the write side must fail and the
	// outcome stay unconfirmed.
	p := newPeer(t)
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.observe(r)
		w.Header().Set("Connection", "close")
		p.writeJSON(w, http.StatusOK, `{"path":"/big","size":0,"crc64":"0","sha256":"`+sha256Hex("")+`","conflict":"created"}`)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	c := testClient(t, p)
	_, err := c.Upload(t.Context(), "d1", "/big", "big", "fail", -1, strings.NewReader(strings.Repeat("x", 1<<20)))
	if err != nil && !strings.Contains(err.Error(), "not confirmed") && !strings.Contains(err.Error(), "consume") {
		t.Fatalf("early-response upload must fail as unconfirmed, got %v", err)
	}
	if err == nil {
		t.Fatal("early-response upload must not confirm")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("no compensating delete")
	}
}

func TestDownloadRejectsPeerIgnoringRange(t *testing.T) {
	p := newPeer(t)
	// Peer answers a ranged read with 200 (range ignored): fail closed.
	p.filesStatus = http.StatusOK
	p.filesBody = []byte("hello bytes")
	c := testClient(t, p)
	if _, err := c.Download(t.Context(), "d1", "/x", "bytes=2-5"); err == nil {
		t.Fatal("a 200 answer to a ranged read must fail closed")
	}
	// 206 with the wrong window is rejected.
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.observe(r)
		w.Header().Set("Content-Range", "bytes 0-5/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("hello "))
	})
	if _, err := c.Download(t.Context(), "d1", "/x", "bytes=2-5"); err == nil {
		t.Fatal("a 206 with the wrong window must be rejected")
	}
	// A correct 206 passes.
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.observe(r)
		w.Header().Set("Content-Range", "bytes 2-5/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("llo "))
	})
	resp, err := c.Download(t.Context(), "d1", "/x", "bytes=2-5")
	if err != nil {
		t.Fatalf("correct 206 must pass: %v", err)
	}
	_ = resp.Body.Close()
}

func TestListPrematureEndRejected(t *testing.T) {
	p := newPeer(t)
	page := 0
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.observe(r)
		if strings.HasSuffix(r.URL.Path, "/list") {
			page++
			// Declares 2 entries but sends an empty second page.
			body := `{"items":[{"name":"a","path":"/a","is_dir":false,"size":1,"modified":"","hashes":{}}],"total":2,"page":1,"per_page":1000}`
			if page > 1 {
				body = `{"items":[],"total":2,"page":2,"per_page":1000}`
			}
			p.writeJSON(w, http.StatusOK, body)
			return
		}
		p.writeJSON(w, http.StatusOK, `{"error":{"code":"path_not_found","message":"stop"}}`)
	})
	c := testClient(t, p)
	if _, err := c.ListAll(t.Context(), "d1", "/"); err == nil {
		t.Fatal("a listing that ends before its declared total must be rejected")
	}
}
