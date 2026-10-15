package goapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// dpeer is a contract-faithful fake GoAPI peer: it advertises the relay hop
// capability, enforces the hop bound at ingress and records every request.
type dpeer struct {
	t *testing.T
	s *httptest.Server

	mu       sync.Mutex
	requests []drecord

	key        string
	drive      string
	meBody     string
	statBody   string
	statStatus int
	override   func(page int) (status int, body string) // list override
	page       int
	putStatus  int
	putBody    string
	redirect   string
}

type drecord struct {
	Method string
	Path   string
	Query  string
	Hop    string
	APIKey string
	Range  string
	Body   []byte
}

func newDriverPeer(t *testing.T) *dpeer {
	t.Helper()
	p := &dpeer{t: t, key: "sk-c2a-peer-key-123456", drive: "d1"}
	p.meBody = `{"key":{"id":"k","prefix":"sk-c2a-abcd","default_drive":"d1"},` +
		`"drives":[{"id":"d1","provider":"local","root":"/","perms":["list","read","write"],"health":"work"},` +
		`{"id":"d2","provider":"local","root":"/","perms":["list","read"],"health":"work"}],` +
		`"relay_hop_limit":4}`
	p.statBody = `{"path":"/","name":"","is_dir":true,"size":0,"modified":"2026-10-14T00:00:00Z","hashes":{}}`
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handle)
	p.s = httptest.NewServer(mux)
	t.Cleanup(p.s.Close)
	return p
}

const happyListBody = `{"items":[{"name":"hello.txt","path":"/hello.txt","is_dir":false,"size":11,"modified":"2026-10-14T00:00:00Z","hashes":{}}],"total":1,"page":1,"per_page":1000}`

func (p *dpeer) rec(r *http.Request) drecord {
	var body []byte
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 16<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	d := drecord{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Hop: r.Header.Get(goapiclient.HopHeader), APIKey: r.Header.Get("apikey"),
		Range: r.Header.Get("Range"), Body: body,
	}
	p.mu.Lock()
	p.requests = append(p.requests, d)
	p.mu.Unlock()
	return d
}

func (p *dpeer) out(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (p *dpeer) handle(w http.ResponseWriter, r *http.Request) {
	body0, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body = io.NopCloser(bytes.NewReader(body0))
	p.rec(r)
	if p.redirect != "" {
		http.Redirect(w, r, p.redirect, http.StatusFound)
		return
	}
	if _, err := goapiclient.ParseHop(r.Header.Values(goapiclient.HopHeader)); err != nil {
		status := http.StatusBadRequest
		if err == goapiclient.ErrHopExceeded {
			status = http.StatusForbidden
		}
		p.out(w, status, `{"error":{"code":"hop_invalid","message":"invalid relay hop header"}}`)
		return
	}
	if r.Header.Get("apikey") != p.key {
		p.out(w, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"no"}}`)
		return
	}
	pfx := "/v1/drives/" + p.drive
	switch {
	case r.URL.Path == "/v1/me":
		p.out(w, http.StatusOK, p.meBody)
	case r.URL.Path == pfx+"/stat":
		status := p.statStatus
		if status == 0 {
			status = http.StatusOK
		}
		p.out(w, status, p.statBody)
	case r.URL.Path == pfx+"/list":
		p.mu.Lock()
		page := p.page
		p.page++
		override := p.override
		p.mu.Unlock()
		if override != nil {
			status, body := override(page)
			p.out(w, status, body)
			return
		}
		p.out(w, http.StatusOK, happyListBody)
	case r.URL.Path == pfx+"/mkdir":
		p.out(w, http.StatusOK, `{"path":"/new","created":true}`)
	case r.URL.Path == pfx+"/move":
		var req struct{ From, To string }
		_ = json.Unmarshal(body0, &req)
		p.out(w, http.StatusOK, fmt.Sprintf(`{"from":%q,"to":%q}`, req.From, req.To))
	case r.URL.Path == pfx+"/delete":
		p.out(w, http.StatusOK, `{"results":[{"path":"/x","status":"deleted"}]}`)
	case r.URL.Path == pfx+"/files" && r.Method == http.MethodPut:
		p.put(w, r)
	case r.URL.Path == pfx+"/files" && r.Method == http.MethodGet:
		data := "hello bytes"
		if rg := r.Header.Get("Range"); rg != "" {
			// Honor bounded ranges with a proper 206 + Content-Range.
			spec := strings.TrimPrefix(rg, "bytes=")
			dash := strings.IndexByte(spec, '-')
			a, _ := strconv.Atoi(spec[:dash])
			b, _ := strconv.Atoi(spec[dash+1:])
			if b >= len(data) {
				b = len(data) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, data[a:b+1])
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, data)
	default:
		p.out(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"no route"}}`)
	}
}

func (p *dpeer) put(w http.ResponseWriter, r *http.Request) {
	var data []byte
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			p.out(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"boundary"}}`)
			return
		}
		form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(64 << 20)
		if err != nil {
			p.out(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"form"}}`)
			return
		}
		defer func() { _ = form.RemoveAll() }()
		f, err := form.File["file"][0].Open()
		if err != nil {
			p.out(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"part"}}`)
			return
		}
		defer f.Close() //nolint:errcheck
		data, _ = io.ReadAll(f)
	} else {
		var err error
		data, err = io.ReadAll(r.Body)
		if err != nil {
			p.out(w, http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"read"}}`)
			return
		}
	}
	p.mu.Lock()
	status, body := p.putStatus, p.putBody
	p.mu.Unlock()
	if status != 0 || body != "" {
		if status == 0 {
			status = http.StatusOK
		}
		p.out(w, status, body)
		return
	}
	crc := crc64.New(goapiclient.CRCTable())
	_, _ = crc.Write(data)
	receipt := fmt.Sprintf(`{"path":%q,"size":%d,"crc64":%q,"sha256":%q,"conflict":"created"}`,
		r.URL.Query().Get("path"), len(data), strconv.FormatUint(crc.Sum64(), 10), sha256Hex(data))
	p.out(w, http.StatusOK, receipt)
}

func (p *dpeer) setListOverride(f func(page int) (int, string)) {
	p.mu.Lock()
	p.override = f
	p.page = 0
	p.mu.Unlock()
}

func (p *dpeer) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.requests) }

func (p *dpeer) deleteCalls() int {
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

func (p *dpeer) all() []drecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]drecord(nil), p.requests...)
}

func newDriver(t *testing.T, p *dpeer, drive string) *GoAPI {
	t.Helper()
	d := &GoAPI{}
	d.RootFolderPath = "/"
	d.URL = p.s.URL
	d.APIKey = p.key
	d.Drive = drive
	if err := d.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return d
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// rootAt builds the embedded root path without promoted-field literals
// (the module language version predates them).
func rootAt(root string) driver.RootPath { return driver.RootPath{RootFolderPath: root} }

// TestInitCapabilityGate: a peer that does not advertise the relay hop
// capability is refused — its ingress would reset the hop counter and
// relay loops would be unbounded across it.
func TestInitCapabilityGate(t *testing.T) {
	p := newDriverPeer(t)
	p.meBody = strings.Replace(p.meBody, `,"relay_hop_limit":4}`, "}", 1)
	d := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: p.drive}}
	err := d.Init(t.Context())
	if err == nil || !strings.Contains(fmt.Sprint(err), "串联跳数保护") {
		t.Fatalf("legacy peer must be refused with the hop-capability error, got %v", err)
	}
}

func TestInitGrantAndRootChecks(t *testing.T) {
	p := newDriverPeer(t)
	mk := func(drive string) *GoAPI {
		d := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: drive}}
		return d
	}

	// Explicit drive outside the grants.
	if err := mk("dX").Init(t.Context()); err == nil || !strings.Contains(err.Error(), "granted drives") {
		t.Fatalf("ungranted drive must be refused, got %v", err)
	}
	// Default drive absent from grants with several drives: refuse.
	p.meBody = strings.Replace(p.meBody, `"default_drive":"d1"`, `"default_drive":"nope"`, 1)
	if err := mk("").Init(t.Context()); err == nil || !strings.Contains(err.Error(), "granted drives") {
		t.Fatalf("default drive outside grants must be refused, got %v", err)
	}
	p.meBody = strings.Replace(p.meBody, `"default_drive":"nope"`, `"default_drive":"d1"`, 1)

	// Root does not exist.
	p.statStatus = http.StatusNotFound
	p.statBody = `{"error":{"code":"path_not_found","message":"gone"}}`
	if err := mk("d1").Init(t.Context()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing root must be refused, got %v", err)
	}
	// Root is a file.
	p.statStatus = http.StatusOK
	p.statBody = `{"path":"/","name":"f","is_dir":false,"size":1,"modified":"","hashes":{}}`
	if err := mk("d1").Init(t.Context()); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file root must be refused, got %v", err)
	}
	p.statBody = `{"path":"/","name":"","is_dir":true,"size":0,"modified":"2026-10-14T00:00:00Z","hashes":{}}`

	// List permission missing.
	p.setListOverride(func(int) (int, string) {
		return http.StatusForbidden, `{"error":{"code":"forbidden","message":"no list"}}`
	})
	if err := mk("d1").Init(t.Context()); err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("list-less key must be refused, got %v", err)
	}
	p.setListOverride(nil)

	// Happy path resolves the granted default drive.
	newDriver(t, p, "")
}

func TestListValidatesEntriesAndBounds(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")

	cases := []struct {
		name string
		body string
	}{
		{"traversal name", `{"items":[{"name":"..","path":"/..","is_dir":true,"size":0,"modified":"","hashes":{}}],"total":1,"page":1,"per_page":1000}`},
		{"encoded separator", `{"items":[{"name":"a%2fb","path":"/a%2fb","is_dir":false,"size":1,"modified":"","hashes":{}}],"total":1,"page":1,"per_page":1000}`},
		{"name not base of path", `{"items":[{"name":"a","path":"/other/a","is_dir":false,"size":1,"modified":"","hashes":{}}],"total":1,"page":1,"per_page":1000}`},
		{"negative size", `{"items":[{"name":"a","path":"/a","is_dir":false,"size":-5,"modified":"","hashes":{}}],"total":1,"page":1,"per_page":1000}`},
		{"duplicate path", `{"items":[` +
			`{"name":"a","path":"/a","is_dir":false,"size":1,"modified":"","hashes":{}},` +
			`{"name":"a","path":"/a","is_dir":false,"size":1,"modified":"","hashes":{}}],"total":2,"page":1,"per_page":1000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.setListOverride(func(int) (int, string) { return http.StatusOK, tc.body })
			if _, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{}); err == nil {
				t.Fatal("hostile listing must fail the whole listing")
			}
		})
	}

	// Directory beyond the listing cap fails explicitly: every page keeps
	// delivering unique entries while total stays above the bound.
	p.setListOverride(func(reqPage int) (int, string) {
		var sb strings.Builder
		sb.WriteString(`{"items":[`)
		for i := 0; i < 1000; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			n := reqPage*1000 + i
			fmt.Fprintf(&sb, `{"name":"f%d","path":"/f%d","is_dir":false,"size":1,"modified":"","hashes":{}}`, n, n)
		}
		fmt.Fprintf(&sb, `],"total":100000,"page":%d,"per_page":1000}`, reqPage+1)
		return http.StatusOK, sb.String()
	})
	if _, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{}); err == nil ||
		!strings.Contains(err.Error(), "listing limit") {
		t.Fatalf("oversized directory must fail explicitly, got %v", err)
	}
}

func TestListHappyAndChroot(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")
	objs, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].GetName() != "hello.txt" || objs[0].GetSize() != 11 {
		t.Fatalf("unexpected listing %+v", objs)
	}
	// Outside the root: refused before any request is made.
	before := p.count()
	if _, err := d.List(t.Context(), &model.Object{Path: "/../etc"}, model.ListArgs{}); err == nil {
		t.Fatal("path outside the root must be refused")
	}
	if _, err := d.List(t.Context(), &model.Object{Path: "/%2e%2e/x"}, model.ListArgs{}); err == nil {
		t.Fatal("encoded traversal must be refused")
	}
	if p.count() != before {
		t.Fatal("refused paths must not reach the peer")
	}
}

func TestPutReceiptsAndNoCompensatingDelete(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")
	dir := &model.Object{Path: "/"}

	// Happy raw upload.
	fs := stream.FileStream{Obj: &model.Object{Name: "hello.txt", Size: 11}, Reader: strings.NewReader("hello bytes")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if p.deleteCalls() != 0 {
		t.Fatal("no delete on the success path")
	}

	// Receipt lying about the crc: unconfirmed failure, target untouched.
	p.putBody = `{"path":"/bad.txt","size":4,"crc64":"1","sha256":"` + sha256Hex([]byte("abcd")) + `","conflict":"created"}`
	fs = stream.FileStream{Obj: &model.Object{Name: "bad.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err == nil {
		t.Fatal("crc-mismatched receipt must fail")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("mismatched receipt must not trigger a compensating delete")
	}

	// Garbage 200 body: unconfirmed.
	p.putBody = "ok"
	fs = stream.FileStream{Obj: &model.Object{Name: "bad.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err == nil {
		t.Fatal("non-receipt 200 body must fail")
	}
	p.putBody = ""

	// Zero-byte upload goes through the multipart form and verifies against
	// a size-0 receipt.
	fs = stream.FileStream{Obj: &model.Object{Name: "empty.txt", Size: 0}, Reader: strings.NewReader("")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err != nil {
		t.Fatalf("zero-byte Put: %v", err)
	}
	multipart := false
	for _, r := range p.all() {
		if r.Method == http.MethodPut && strings.Contains(r.Query, "empty.txt") &&
			strings.Contains(string(r.Body), `form-data; name="file"`) {
			multipart = true
		}
	}
	if !multipart {
		t.Fatal("zero-byte upload must use the multipart form")
	}

	// Overwrite: the fs layer marked an existing destination, so the peer is
	// asked for on_conflict=overwrite.
	fs = stream.FileStream{Obj: &model.Object{Name: "hello.txt", Size: 11}, Reader: strings.NewReader("hello bytes")}
	fs.SetExist(&model.Object{Name: "hello.txt"})
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	overwrite := false
	for _, r := range p.all() {
		if strings.Contains(r.Query, "on_conflict=overwrite") {
			overwrite = true
		}
	}
	if !overwrite {
		t.Fatal("existing-destination upload must request on_conflict=overwrite")
	}

	// Peer 500: unconfirmed failure, no compensating delete.
	p.putStatus = http.StatusInternalServerError
	p.putBody = `{"error":{"code":"upstream_error","message":"disk full"}}`
	fs = stream.FileStream{Obj: &model.Object{Name: "bad.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err == nil {
		t.Fatal("peer 500 must fail the upload")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("failed upload must not delete anything")
	}

	// A 202 (queued upload) is never a confirmation, even with a plausible
	// receipt body.
	p.putStatus = http.StatusAccepted
	p.putBody = `{"path":"/bad.txt","size":4,"crc64":"1","sha256":"x","conflict":"created"}`
	fs = stream.FileStream{Obj: &model.Object{Name: "bad.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err == nil {
		t.Fatal("202 must not confirm an upload")
	}
	p.putStatus = 0
	p.putBody = ""

	// Interrupted upload: the peer severs the connection mid-body.
	srv := p.s
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4)
		_, _ = io.ReadFull(r.Body, buf)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
	fs = stream.FileStream{Obj: &model.Object{Name: "cut.txt", Size: 100}, Reader: strings.NewReader(strings.Repeat("x", 100))}
	if err := d.Put(t.Context(), dir, &fs, func(float64) {}); err == nil {
		t.Fatal("an interrupted upload must fail (unconfirmed)")
	}
	if p.deleteCalls() != 0 {
		t.Fatal("no compensating delete after a disconnect")
	}
}

func TestLinkProxyRangeNoRedirectNoKeyLeak(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")
	link, err := d.Link(t.Context(), &model.Object{Path: "/hello.txt", Size: 11}, model.LinkArgs{})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if link.URL != "" || link.Header != nil {
		t.Fatal("Link must expose neither an upstream URL nor headers (no key to the browser)")
	}
	rc, err := link.RangeReader.RangeRead(t.Context(), http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello bytes" {
		t.Fatalf("full read = %q", string(b))
	}
	rc2, err := link.RangeReader.RangeRead(t.Context(), http_range.Range{Start: 6, Length: 5})
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	b2, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if string(b2) != "bytes" {
		t.Fatalf("ranged read = %q", string(b2))
	}
	sawRange := false
	for _, r := range p.all() {
		if r.Range == "bytes=6-10" {
			sawRange = true
		}
		if strings.Contains(r.Query, p.key) || strings.Contains(r.Path, p.key) {
			t.Fatal("key must never appear in a URL")
		}
		if r.APIKey != "" && r.APIKey != p.key {
			t.Fatal("foreign credential observed")
		}
	}
	if !sawRange {
		t.Fatal("range header must be forwarded")
	}

	// Redirects are refused and never followed; the redirect target must not
	// see the key.
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_ = r.Header.Get("apikey")
	}))
	defer target.Close()
	p.redirect = target.URL + "/steal"
	_, err = link.RangeReader.RangeRead(t.Context(), http_range.Range{Start: 0, Length: -1})
	if err == nil || !strings.Contains(fmt.Sprint(err), "redirect") {
		t.Fatalf("redirected download must fail typed, got %v", err)
	}
	if targetHits != 0 {
		t.Fatal("the redirect target must never be contacted")
	}
}

// TestHopPropagationThroughDriver: the hop count from the OpenList request
// context is forwarded outbound; a chain at the cap is cut by the peer and
// the driver makes exactly one attempt (no retries).
func TestHopPropagationThroughDriver(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")
	before := p.count()
	if _, err := d.List(goapiclient.WithHop(t.Context(), 2), &model.Object{Path: "/"}, model.ListArgs{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := p.all()[before].Hop; got != "3" {
		t.Fatalf("peer saw hop %q, want 3", got)
	}
	before = p.count()
	if _, err := d.List(goapiclient.WithHop(t.Context(), goapiclient.MaxHops), &model.Object{Path: "/"}, model.ListArgs{}); err == nil {
		t.Fatal("a chain at the hop cap must be cut at the peer")
	}
	if p.count() != before+1 {
		t.Fatal("exactly one outbound attempt is made (no retries)")
	}
}

func TestMutationsAndRejects(t *testing.T) {
	p := newDriverPeer(t)
	d := newDriver(t, p, "d1")
	if err := d.MakeDir(t.Context(), &model.Object{Path: "/"}, "new"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := d.MakeDir(t.Context(), &model.Object{Path: "/"}, "a/b"); err == nil {
		t.Fatal("separator name must be rejected")
	}
	if err := d.Move(t.Context(), &model.Object{Name: "a", Path: "/a"}, &model.Object{Path: "/dst"}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := d.Rename(t.Context(), &model.Object{Name: "a", Path: "/a"}, "b"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := d.Remove(t.Context(), &model.Object{Name: "x", Path: "/x"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Per-path delete errors surface with the peer's scrubbed message.
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/delete") {
			p.rec(r)
			p.out(w, http.StatusOK, `{"results":[{"path":"/x","status":"error","error":"denied by policy"}]}`)
			return
		}
		p.handle(w, r)
	})
	err := d.Remove(t.Context(), &model.Object{Name: "x", Path: "/x"})
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("per-path delete error must surface, got %v", err)
	}
}

// TestTwoDriverChainOverHTTP walks an actual HTTP chain that mirrors the
// production topology: driver -> peer B, whose handler forwards every call
// into peer A with its inbound hop plus one — exactly what a nested GoAPI
// storage's driver does. The counter grows per transit and a chain at the
// cap is cut by A's ingress.
func TestTwoDriverChainOverHTTP(t *testing.T) {
	a := newDriverPeer(t)
	b := newDriverPeer(t)
	relay := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hop, perr := goapiclient.ParseHop(r.Header.Values(goapiclient.HopHeader))
		if perr != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, a.s.URL+r.URL.Path+"?"+r.URL.RawQuery, bytes.NewReader(body))
		req.Header.Set("apikey", a.key)
		if hop > 0 {
			req.Header.Set(goapiclient.HopHeader, strconv.Itoa(hop+1))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
	b.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.rec(r)
		if r.URL.Path == "/v1/me" {
			relay(w, r) // capability is forwarded too
			return
		}
		relay(w, r)
	})

	// The driver in front of B: ingress hop 0 -> B forwards 1 to A.
	dIn := &GoAPI{}
	dIn.RootFolderPath = "/"
	dIn.URL = b.s.URL
	dIn.APIKey = b.key
	dIn.Drive = b.drive
	if err := dIn.Init(goapiclient.WithHop(t.Context(), 0)); err != nil {
		t.Fatalf("chain Init: %v", err)
	}
	aSaw := ""
	for _, r := range a.all() {
		if r.Hop != "" {
			aSaw = r.Hop
		}
	}
	if aSaw != "2" {
		t.Fatalf("inner peer saw hop %q, want 2", aSaw)
	}

	// A chain already at the cap is cut: the nested outbound (5) exceeds the
	// cap and the whole call fails at the entry driver.
	dCap := &GoAPI{}
	dCap.RootFolderPath = "/"
	dCap.URL = b.s.URL
	dCap.APIKey = b.key
	dCap.Drive = b.drive
	if err := dCap.Init(goapiclient.WithHop(t.Context(), goapiclient.MaxHops)); err == nil {
		t.Fatal("a chain at the hop cap must be cut across real HTTP peers")
	}
}
