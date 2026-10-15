package goapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	stdpath "path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
)

// ---- fake-peer routing helpers (additive to the driver_test.go fake) ----

// routeStat lets a test answer stat requests per peer path; returning
// status 0 delegates to the generic fake handler.
func routeStat(t *testing.T, p *dpeer, reply func(path string) (int, string)) {
	t.Helper()
	base := p.s.Config.Handler
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stat") {
			if status, body := reply(r.URL.Query().Get("path")); status != 0 {
				p.rec(r)
				p.out(w, status, body)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
}

// routeList lets a test answer list requests per peer directory; returning
// status 0 delegates to the generic fake handler.
func routeList(t *testing.T, p *dpeer, reply func(dir string) (int, string)) {
	t.Helper()
	base := p.s.Config.Handler
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/list") {
			if status, body := reply(r.URL.Query().Get("path")); status != 0 {
				p.rec(r)
				p.out(w, status, body)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
}

// routeMutatingEcho makes the fake peer echo mkdir/delete requests for the
// exact requested paths (the generic fake answers with fixed paths, which
// the client contract rejects for any other path).
func routeMutatingEcho(t *testing.T, p *dpeer) {
	t.Helper()
	base := p.s.Config.Handler
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost &&
			(strings.HasSuffix(r.URL.Path, "/mkdir") || strings.HasSuffix(r.URL.Path, "/delete")) {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
			p.rec(r)
			if strings.HasSuffix(r.URL.Path, "/mkdir") {
				var req struct {
					Path string `json:"path"`
				}
				_ = json.Unmarshal(body, &req)
				p.out(w, http.StatusOK, fmt.Sprintf(`{"path":%q,"created":true}`, req.Path))
				return
			}
			var req struct {
				Paths []string `json:"paths"`
			}
			_ = json.Unmarshal(body, &req)
			results := make([]string, 0, len(req.Paths))
			for _, path := range req.Paths {
				results = append(results, fmt.Sprintf(`{"path":%q,"status":"deleted"}`, path))
			}
			p.out(w, http.StatusOK, `{"results":[`+strings.Join(results, ",")+`]}`)
			return
		}
		base.ServeHTTP(w, r)
	})
}

const notFoundBody = `{"error":{"code":"path_not_found","message":"gone"}}`

// wireReq is one request as it crossed the wire, with its decoded body.
// (The shared fake's record leaves JSON bodies empty, so mapping tests keep
// their own journal by wrapping the handler outermost.)
type wireReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

type wireLog struct {
	mu   sync.Mutex
	reqs []wireReq
}

func (l *wireLog) add(r wireReq) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
}

func (l *wireLog) all() []wireReq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]wireReq(nil), l.reqs...)
}

// noVirtualLeak: the reserved virtual names must never reach the peer.
func noVirtualLeak(t *testing.T, l *wireLog) {
	t.Helper()
	for _, r := range l.all() {
		wire := r.Path + "?" + r.Query + r.Body
		if strings.Contains(wire, ".c2a-trash") || strings.Contains(wire, ".relay") {
			t.Fatalf("reserved virtual name leaked to the peer: %+v", r)
		}
	}
}

// captureWire records every request with its body; install it last so it
// sees what the other routes hand on.
func captureWire(t *testing.T, p *dpeer) *wireLog {
	t.Helper()
	log := &wireLog{}
	base := p.s.Config.Handler
	p.s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if r.Body != nil {
			_, _ = io.Copy(&buf, r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
		}
		log.add(wireReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: buf.String()})
		base.ServeHTTP(w, r)
	})
	return log
}

// statBodyFor echoes a directory stat for the exact requested path, as the
// client contract requires.
func statBodyFor(path string) (int, string) {
	return http.StatusOK, fmt.Sprintf(
		`{"path":%q,"name":%q,"is_dir":true,"size":0,"modified":"2026-10-14T00:00:00Z","hashes":{}}`,
		path, stdpath.Base(path))
}

func entryJSON(name, path string, isDir bool, size int64) string {
	return fmt.Sprintf(`{"name":%q,"path":%q,"is_dir":%t,"size":%d,"modified":"2026-10-14T00:00:00Z","hashes":{}}`,
		name, path, isDir, size)
}

func listBody(items []string) string {
	return fmt.Sprintf(`{"items":[%s],"total":%d,"page":1,"per_page":1000}`, strings.Join(items, ","), len(items))
}

// identityDriver builds an initialized storage with a real row id (so the
// adoption probe runs and commits the namespace) and returns it together
// with its two backing alias names (without the leading slash).
func identityDriver(t *testing.T, p *dpeer, id uint) (*GoAPI, string, string) {
	t.Helper()
	routeStat(t, p, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	d := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: "d1"}}
	d.ID = id
	if err := d.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ns := d.Addition.SystemNS
	if !validNamespace(ns) {
		t.Fatalf("Init must establish a canonical namespace, got %q", ns)
	}
	base := ".goapi-system-" + ns + "-" + strconv.FormatUint(uint64(id), 10)
	return d, base + "-trash", base + "-relay"
}

// TestSystemNamespaceGeneratedAndRetained: Init generates a canonical
// random namespace once, keeps a retained one, and refuses a corrupted one.
func TestSystemNamespaceGeneratedAndRetained(t *testing.T) {
	p := newDriverPeer(t)
	d, _, _ := identityDriver(t, p, 5)
	ns := d.Addition.SystemNS
	other, err := newNamespace()
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	if other == ns || !validNamespace(other) {
		t.Fatalf("namespaces must be random and canonical, got %q vs %q", other, ns)
	}

	// A retained namespace survives a re-init unchanged and is accepted.
	d2 := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: "d1", SystemNS: ns}}
	d2.ID = 5
	if err := d2.Init(t.Context()); err != nil {
		t.Fatalf("re-init with retained namespace: %v", err)
	}
	if d2.Addition.SystemNS != ns {
		t.Fatalf("retained namespace changed: %q != %q", d2.Addition.SystemNS, ns)
	}

	// A corrupted namespace fails closed instead of being silently replaced.
	d3 := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: "d1", SystemNS: "ZZ" + ns[2:]}}
	if err := d3.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "not a canonical driver namespace") {
		t.Fatalf("corrupted namespace must be refused, got %v", err)
	}
}

// TestInitAdoptionProbeAndCollision: a freshly generated namespace probes
// its alias paths once and fails closed when they already exist (an
// unrelated user path must never be claimed); a retained namespace never
// probes and never claims.
func TestInitAdoptionProbeAndCollision(t *testing.T) {
	p := newDriverPeer(t)
	routeStat(t, p, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	d := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: "d1"}}
	d.ID = 5
	if err := d.Init(t.Context()); err != nil {
		t.Fatalf("Init with free alias paths: %v", err)
	}
	ns := d.Addition.SystemNS
	probed := map[string]bool{}
	for _, r := range p.all() {
		if strings.HasSuffix(r.Path, "/stat") && strings.Contains(r.Query, ".goapi-system-") {
			probed[r.Query] = true
		}
	}
	for _, alias := range []string{
		".goapi-system-" + ns + "-5-trash",
		".goapi-system-" + ns + "-5-relay",
	} {
		if !probed["path=%2F"+alias] {
			t.Fatalf("fresh namespace must probe %s, probed %v", alias, probed)
		}
	}

	// Collision: the alias path exists and cannot be ours — fail closed.
	pc := newDriverPeer(t)
	routeStat(t, pc, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			return statBodyFor(path)
		}
		return 0, ""
	})
	dc := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: pc.s.URL, APIKey: pc.key, Drive: "d1"}}
	dc.ID = 5
	if err := dc.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "already exists on the peer") {
		t.Fatalf("existing alias path must fail the init, got %v", err)
	}

	// A retained namespace is steady state: no probe, no claim, init OK even
	// though a same-named path exists (it is this storage's own directory).
	pr := newDriverPeer(t)
	routeStat(t, pr, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			return statBodyFor(path)
		}
		return 0, ""
	})
	dr := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: pr.s.URL, APIKey: pr.key, Drive: "d1", SystemNS: ns}}
	dr.ID = 5
	if err := dr.Init(t.Context()); err != nil {
		t.Fatalf("re-init with retained namespace must not probe, got %v", err)
	}
	for _, r := range pr.all() {
		if strings.HasSuffix(r.Path, "/stat") && strings.Contains(r.Query, ".goapi-system-") {
			t.Fatal("retained namespace must not probe the peer")
		}
	}
}

// TestInitFailedProbeDoesNotCommitNamespace: the fresh namespace candidate
// is committed only after both alias probes passed. A collision on either
// alias, an unverifiable peer answer or a missing row id leaves the
// addition empty, so the next init retries the adoption instead of claiming
// a previously refused or unverified alias; a retained namespace is never
// touched.
func TestInitFailedProbeDoesNotCommitNamespace(t *testing.T) {
	// Collision on the trash alias (the first probe).
	p := newDriverPeer(t)
	routeStat(t, p, func(path string) (int, string) {
		if strings.Contains(path, "-trash") {
			return statBodyFor(path)
		}
		if strings.Contains(path, ".goapi-system-") {
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	d := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p.s.URL, APIKey: p.key, Drive: "d1"}}
	d.ID = 5
	if err := d.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "already exists on the peer") {
		t.Fatalf("trash collision must fail the init, got %v", err)
	}
	if d.Addition.SystemNS != "" {
		t.Fatalf("failed adoption must not commit the namespace, got %q", d.Addition.SystemNS)
	}

	// Retry after the peer state is fixed: the adoption runs again and only
	// now commits.
	probes := 0
	routeStat(t, p, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			probes++
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	if err := d.Init(t.Context()); err != nil {
		t.Fatalf("retry after the collision is gone: %v", err)
	}
	if !validNamespace(d.Addition.SystemNS) {
		t.Fatalf("successful adoption must commit a canonical namespace, got %q", d.Addition.SystemNS)
	}
	if probes == 0 {
		t.Fatal("the retry must re-probe the alias paths")
	}

	// Collision on the relay alias only: the second probe gates the commit.
	p2 := newDriverPeer(t)
	routeStat(t, p2, func(path string) (int, string) {
		if strings.Contains(path, "-relay") {
			return statBodyFor(path)
		}
		if strings.Contains(path, ".goapi-system-") {
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	d2 := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p2.s.URL, APIKey: p2.key, Drive: "d1"}}
	d2.ID = 5
	if err := d2.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "already exists on the peer") {
		t.Fatalf("relay collision must fail the init, got %v", err)
	}
	if d2.Addition.SystemNS != "" {
		t.Fatalf("second-alias collision must not commit the namespace, got %q", d2.Addition.SystemNS)
	}

	// An unverifiable peer answer never commits either.
	p3 := newDriverPeer(t)
	routeStat(t, p3, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			return http.StatusInternalServerError, `{"error":{"code":"upstream_error","message":"disk full"}}`
		}
		return 0, ""
	})
	d3 := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p3.s.URL, APIKey: p3.key, Drive: "d1"}}
	d3.ID = 5
	if err := d3.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "cannot verify the system directory") {
		t.Fatalf("unverifiable probe must fail the init, got %v", err)
	}
	if d3.Addition.SystemNS != "" {
		t.Fatalf("network-failed adoption must not commit the namespace, got %q", d3.Addition.SystemNS)
	}

	// Without the row identity nothing is adopted at all: the candidate is
	// discarded, and a later init with an identity starts the adoption.
	p4 := newDriverPeer(t)
	probes4 := 0
	routeStat(t, p4, func(path string) (int, string) {
		if strings.Contains(path, ".goapi-system-") {
			probes4++
			return http.StatusNotFound, notFoundBody
		}
		return 0, ""
	})
	d4 := &GoAPI{Addition: Addition{RootPath: rootAt("/"), URL: p4.s.URL, APIKey: p4.key, Drive: "d1"}}
	if err := d4.Init(t.Context()); err != nil {
		t.Fatalf("init without identity: %v", err)
	}
	if d4.Addition.SystemNS != "" {
		t.Fatalf("id 0 must not commit the namespace, got %q", d4.Addition.SystemNS)
	}
	if probes4 != 0 {
		t.Fatal("id 0 must not probe")
	}
	d4.ID = 5
	if err := d4.Init(t.Context()); err != nil {
		t.Fatalf("init after identity appears: %v", err)
	}
	if !validNamespace(d4.Addition.SystemNS) || probes4 == 0 {
		t.Fatalf("init with identity must probe and commit, ns=%q probes=%d", d4.Addition.SystemNS, probes4)
	}
}

// TestInitRejectsReservedRoots: the configured root must never sit inside
// the reserved virtual system directories or the storage's own aliases.
func TestInitRejectsReservedRoots(t *testing.T) {
	p := newDriverPeer(t)
	// A contract-faithful peer: every stat echoes its path, every directory
	// lists as empty — so only the driver's own checks can fail here.
	routeStat(t, p, func(path string) (int, string) { return statBodyFor(path) })
	routeList(t, p, func(string) (int, string) { return http.StatusOK, listBody(nil) })
	ns, err := newNamespace()
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	mk := func(root string) *GoAPI {
		return &GoAPI{Addition: Addition{RootPath: rootAt(root), URL: p.s.URL, APIKey: p.key, Drive: "d1", SystemNS: ns}}
	}
	for _, root := range []string{"/.c2a-trash", "/.c2a-trash/sub", "/.relay", "/.relay/sub"} {
		if err := mk(root).Init(t.Context()); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("root %q must be refused as reserved, got %v", root, err)
		}
	}
	// Inside the storage's own backing alias (identity is known here).
	d := mk("/.goapi-system-" + ns + "-5-trash")
	d.ID = 5
	if err := d.Init(t.Context()); err == nil || !strings.Contains(err.Error(), "own system directories") {
		t.Fatalf("root inside the own alias must be refused, got %v", err)
	}
	// An ordinary nested root stays fine.
	ok := mk("/data")
	ok.ID = 5
	if err := ok.Init(t.Context()); err != nil {
		t.Fatalf("ordinary root with identity: %v", err)
	}
}

// TestTrashVirtualViewRoundTrip walks the full trash lifecycle over the
// wire: mkdir, trash-move, listing, restore and purge — every peer request
// carries the backing alias and none carries the virtual name, while the
// driver-facing paths stay the virtual ones.
func TestTrashVirtualViewRoundTrip(t *testing.T) {
	p := newDriverPeer(t)
	d, trashAlias, relayAlias := identityDriver(t, p, 7)
	ta := "/" + trashAlias
	other := ".goapi-system-" + strings.Repeat("cd", 16) + "-9-trash"
	routeMutatingEcho(t, p)
	// Per-directory listings: the trash bucket and the storage root.
	routeList(t, p, func(dir string) (int, string) {
		switch dir {
		case ta + "/2026-10-14":
			return http.StatusOK, listBody([]string{entryJSON("report.txt", ta+"/2026-10-14/report.txt", false, 6)})
		case "/":
			return http.StatusOK, listBody([]string{
				entryJSON("docs", "/docs", true, 0),
				entryJSON(trashAlias, ta, true, 0),
				entryJSON(relayAlias, "/"+relayAlias, true, 0),
				entryJSON(other, "/"+other, true, 0),
				entryJSON("hello.txt", "/hello.txt", false, 11),
			})
		}
		return 0, ""
	})
	wire := captureWire(t, p)

	// Native-real recursion: op.MakeDir creates each level, so the reserved
	// root itself is created first (root object + reserved name — nothing
	// pre-exists on the peer), then the day bucket inside it.
	if err := d.MakeDir(t.Context(), &model.Object{Path: "/"}, ".c2a-trash"); err != nil {
		t.Fatalf("MakeDir trash root: %v", err)
	}
	if err := d.MakeDir(t.Context(), &model.Object{Path: "/.c2a-trash"}, "2026-10-14"); err != nil {
		t.Fatalf("MakeDir trash bucket: %v", err)
	}
	// Trash move: /report.txt -> /.c2a-trash/2026-10-14.
	if err := d.Move(t.Context(), &model.Object{Name: "report.txt", Path: "/report.txt"},
		&model.Object{Path: "/.c2a-trash/2026-10-14"}); err != nil {
		t.Fatalf("trash Move: %v", err)
	}
	// Listing the trash returns the virtual path space.
	objs, err := d.List(t.Context(), &model.Object{Path: "/.c2a-trash/2026-10-14"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List trash: %v", err)
	}
	if len(objs) != 1 || objs[0].GetPath() != "/.c2a-trash/2026-10-14/report.txt" || objs[0].GetName() != "report.txt" {
		t.Fatalf("trash listing = %+v", objs)
	}
	// Root listing relabels only this storage's own aliases.
	rootObjs, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	want := map[string]string{
		"/docs":       "docs",
		"/.c2a-trash": ".c2a-trash",
		"/.relay":     ".relay",
		"/" + other:   other,
		"/hello.txt":  "hello.txt",
	}
	got := map[string]string{}
	for _, o := range rootObjs {
		got[o.GetPath()] = o.GetName()
	}
	if len(got) != len(want) {
		t.Fatalf("root listing = %+v", got)
	}
	for path, name := range want {
		if got[path] != name {
			t.Fatalf("root entry %s = %q, want %q", path, got[path], name)
		}
	}
	// Restore: move back out of the trash onto the (virtual) root.
	if err := d.Move(t.Context(), &model.Object{Name: "report.txt", Path: "/.c2a-trash/2026-10-14/report.txt"},
		&model.Object{Path: "/"}); err != nil {
		t.Fatalf("restore Move: %v", err)
	}
	// Purge: a plain remove inside the trash maps too.
	if err := d.Remove(t.Context(), &model.Object{Name: "report.txt", Path: "/.c2a-trash/2026-10-14/report.txt"}); err != nil {
		t.Fatalf("purge Remove: %v", err)
	}
	// Only the exact reserved first segment maps; ordinary moves are verbatim.
	if err := d.Move(t.Context(), &model.Object{Name: "keep", Path: "/keep"},
		&model.Object{Path: "/sub"}); err != nil {
		t.Fatalf("ordinary move: %v", err)
	}
	reqs := wire.all()
	last := reqs[len(reqs)-1]
	if !strings.Contains(last.Body, `"/sub/keep"`) {
		t.Fatalf("ordinary move must pass through verbatim, got %q", last.Body)
	}

	// Wire audit: the mapped paths went out and the virtual names never did.
	var sawAliasRootMkdir, sawMappedMkdir, sawMappedMove, sawMappedDelete, sawRestore bool
	for _, r := range wire.all() {
		switch {
		case strings.HasSuffix(r.Path, "/mkdir") && strings.Contains(r.Body, `"/`+trashAlias+`"`):
			sawAliasRootMkdir = true
		case strings.HasSuffix(r.Path, "/mkdir") && strings.Contains(r.Body, trashAlias+"/2026-10-14"):
			sawMappedMkdir = true
		case strings.HasSuffix(r.Path, "/move") && strings.Contains(r.Body, trashAlias+"/2026-10-14/report.txt"):
			switch {
			case strings.Contains(r.Body, `"to":"/report.txt"`):
				sawRestore = true
			case strings.Contains(r.Body, `"from":"/report.txt"`):
				sawMappedMove = true
			}
		case strings.HasSuffix(r.Path, "/delete") && strings.Contains(r.Body, trashAlias+"/2026-10-14/report.txt"):
			sawMappedDelete = true
		}
	}
	if !sawAliasRootMkdir || !sawMappedMkdir || !sawMappedMove || !sawMappedDelete || !sawRestore {
		t.Fatalf("wire audit incomplete: rootMkdir=%v bucketMkdir=%v move=%v delete=%v restore=%v",
			sawAliasRootMkdir, sawMappedMkdir, sawMappedMove, sawMappedDelete, sawRestore)
	}
	noVirtualLeak(t, wire)

	// The relay alias is a different, ordinary directory from the trash one.
	if trashAlias == relayAlias || strings.Contains(trashAlias+relayAlias, ".c2a-trash") {
		t.Fatal("aliases must be ordinary, distinct names")
	}
}

// TestRelayViewPutAndLink: uploads into /.relay land on the relay alias and
// the typed receipt is verified against the mapped peer path — a receipt
// echoing the virtual name can never confirm an upload. Downloads map too.
func TestRelayViewPutAndLink(t *testing.T) {
	p := newDriverPeer(t)
	d, _, relayAlias := identityDriver(t, p, 7)
	routeMutatingEcho(t, p)
	wire := captureWire(t, p)

	// Native-real: the relay root itself is created through the view first,
	// then the nested upload directory via the virtual path.
	if err := d.MakeDir(t.Context(), &model.Object{Path: "/"}, ".relay"); err != nil {
		t.Fatalf("MakeDir relay root: %v", err)
	}
	sawRelayRootMkdir := false
	for _, r := range wire.all() {
		if strings.HasSuffix(r.Path, "/mkdir") && strings.Contains(r.Body, `"/`+relayAlias+`"`) {
			sawRelayRootMkdir = true
		}
	}
	if !sawRelayRootMkdir {
		t.Fatal("relay root mkdir must target the backing alias")
	}

	// Happy upload: the receipt echoes the mapped peer path.
	fs := stream.FileStream{Obj: &model.Object{Name: "x.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), &model.Object{Path: "/.relay/abc"}, &fs, func(float64) {}); err != nil {
		t.Fatalf("relay Put: %v", err)
	}
	sawMappedPut := false
	for _, r := range wire.all() {
		if r.Method == http.MethodPut && strings.Contains(r.Query, relayAlias+"%2Fabc%2Fx.txt") {
			sawMappedPut = true
		}
	}
	if !sawMappedPut {
		t.Fatal("relay upload must target the backing alias")
	}
	noVirtualLeak(t, wire)

	// A receipt echoing the virtual path is not a confirmation.
	p.putBody = `{"path":"/.relay/abc/x.txt","size":4,"crc64":"` + strconv.FormatUint(crc64Of([]byte("abcd")), 10) +
		`","sha256":"` + sha256Hex([]byte("abcd")) + `","conflict":"created"}`
	fs = stream.FileStream{Obj: &model.Object{Name: "x.txt", Size: 4}, Reader: strings.NewReader("abcd")}
	if err := d.Put(t.Context(), &model.Object{Path: "/.relay/abc"}, &fs, func(float64) {}); err == nil ||
		!strings.Contains(err.Error(), "receipt path does not match") {
		t.Fatalf("virtual-path receipt must never confirm, got %v", err)
	}
	p.putBody = ""

	// Downloads through the virtual relay view read the alias path.
	link, err := d.Link(t.Context(), &model.Object{Path: "/.relay/abc/x.txt", Size: 11}, model.LinkArgs{})
	if err != nil {
		t.Fatalf("relay Link: %v", err)
	}
	rc, err := link.RangeReader.RangeRead(t.Context(), http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("relay read: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello bytes" {
		t.Fatalf("relay read = %q", string(b))
	}
	sawMappedGet := false
	for _, r := range wire.all() {
		if r.Method == http.MethodGet && strings.Contains(r.Query, relayAlias+"%2Fabc%2Fx.txt") {
			sawMappedGet = true
		}
	}
	if !sawMappedGet {
		t.Fatal("relay download must read the backing alias")
	}
	noVirtualLeak(t, wire)
}

// TestAliasDirectAddressingBlocked: the storage's own backing aliases are
// not addressable through the virtual view, in any operation, and refused
// before anything reaches the peer. Other storages' aliases are ordinary
// paths and stay reachable, verbatim and unlabeled.
func TestAliasDirectAddressingBlocked(t *testing.T) {
	p := newDriverPeer(t)
	d, trashAlias, relayAlias := identityDriver(t, p, 7)
	other := ".goapi-system-" + strings.Repeat("ef", 16) + "-9-trash"
	routeList(t, p, func(dir string) (int, string) {
		if dir == "/"+other {
			return http.StatusOK, listBody(nil)
		}
		return 0, ""
	})
	wire := captureWire(t, p)

	at := func(alias, sub string) *model.Object {
		return &model.Object{Name: strings.Trim(sub, "/"), Path: "/" + alias + sub}
	}
	mustRefuse := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "cannot be addressed directly") {
			t.Fatalf("%s must refuse the backing alias, got %v", what, err)
		}
	}
	for _, alias := range []string{trashAlias, relayAlias} {
		before := p.count()
		_, err := d.List(t.Context(), at(alias, ""), model.ListArgs{})
		mustRefuse("List", err)
		_, err = d.List(t.Context(), at(alias, "/deep/x"), model.ListArgs{})
		mustRefuse("List subtree", err)
		_, err = d.Link(t.Context(), at(alias, "/x"), model.LinkArgs{})
		mustRefuse("Link", err)
		err = d.Remove(t.Context(), at(alias, "/x"))
		mustRefuse("Remove", err)
		err = d.MakeDir(t.Context(), at(alias, ""), "y")
		mustRefuse("MakeDir", err)
		err = d.Move(t.Context(), &model.Object{Name: "a", Path: "/a"}, at(alias, ""))
		mustRefuse("Move into alias", err)
		err = d.Move(t.Context(), at(alias, "/x"), &model.Object{Path: "/"})
		mustRefuse("Move out of alias", err)
		err = d.Rename(t.Context(), at(alias, "/x"), "y")
		mustRefuse("Rename", err)
		fs := stream.FileStream{Obj: &model.Object{Name: "u.txt", Size: 1}, Reader: strings.NewReader("u")}
		err = d.Put(t.Context(), at(alias, ""), &fs, func(float64) {})
		mustRefuse("Put", err)
		// Joining the alias name at the root is the same direct addressing
		// (the parent "/" maps to itself, so only the final join blocks it).
		err = d.MakeDir(t.Context(), &model.Object{Path: "/"}, alias)
		mustRefuse("MakeDir alias name at root", err)
		fsAlias := &stream.FileStream{Obj: &model.Object{Name: alias, Size: 1}, Reader: strings.NewReader("u")}
		err = d.Put(t.Context(), &model.Object{Path: "/"}, fsAlias, func(float64) {})
		mustRefuse("Put alias name at root", err)
		err = d.Rename(t.Context(), &model.Object{Name: "hello.txt", Path: "/hello.txt"}, alias)
		mustRefuse("Rename to alias name", err)
		err = d.Move(t.Context(), &model.Object{Name: alias, Path: "/a"}, &model.Object{Path: "/"})
		mustRefuse("Move into root as alias name", err)
		if p.count() != before {
			t.Fatal("refused alias addressing must not reach the peer")
		}
	}

	// Another storage's alias is not ours: reachable as an ordinary path.
	if _, err := d.List(t.Context(), at(other, ""), model.ListArgs{}); err != nil {
		t.Fatalf("other storage alias must stay an ordinary path, got %v", err)
	}
	saw := false
	for _, r := range wire.all() {
		if strings.Contains(r.Query, other) {
			saw = true
		}
	}
	if !saw {
		t.Fatal("other storage alias request must pass through verbatim")
	}
	noVirtualLeak(t, wire)
}

// TestMappedPathsFailClosedWithoutIdentity: without an established identity
// (canonical namespace plus storage row id) every reserved-virtual-path
// operation fails closed before touching the peer, while ordinary paths
// keep working.
func TestMappedPathsFailClosedWithoutIdentity(t *testing.T) {
	p := newDriverPeer(t)
	d := &GoAPI{Addition: Addition{RootPath: rootAt("/")}}
	cli, err := goapiclient.New(p.s.URL, p.key)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	d.cli = cli
	d.drive = "d1"

	mustFailClosed := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "namespace is not established") {
			t.Fatalf("%s must fail closed without identity, got %v", what, err)
		}
	}
	before := p.count()
	_, err = d.List(t.Context(), &model.Object{Path: "/.c2a-trash"}, model.ListArgs{})
	mustFailClosed("List", err)
	_, err = d.Link(t.Context(), &model.Object{Path: "/.relay/x"}, model.LinkArgs{})
	mustFailClosed("Link", err)
	err = d.Remove(t.Context(), &model.Object{Path: "/.c2a-trash/x"})
	mustFailClosed("Remove", err)
	fs := stream.FileStream{Obj: &model.Object{Name: "u.txt", Size: 1}, Reader: strings.NewReader("u")}
	err = d.Put(t.Context(), &model.Object{Path: "/.relay"}, &fs, func(float64) {})
	mustFailClosed("Put", err)
	// Creating the reserved roots themselves at the top needs the mapping
	// too, so it fails closed as well.
	err = d.MakeDir(t.Context(), &model.Object{Path: "/"}, ".c2a-trash")
	mustFailClosed("MakeDir trash root", err)
	err = d.MakeDir(t.Context(), &model.Object{Path: "/"}, ".relay")
	mustFailClosed("MakeDir relay root", err)
	if p.count() != before {
		t.Fatal("fail-closed mapping must not reach the peer")
	}

	// Ordinary paths are unaffected by the missing identity.
	if _, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{}); err != nil {
		t.Fatalf("ordinary List without identity: %v", err)
	}
	if p.count() != before+1 {
		t.Fatal("ordinary List must reach the peer")
	}

	// A namespace without the storage row id is still not an identity.
	ns, err := newNamespace()
	if err != nil {
		t.Fatalf("newNamespace: %v", err)
	}
	d.Addition.SystemNS = ns
	_, err = d.List(t.Context(), &model.Object{Path: "/.c2a-trash"}, model.ListArgs{})
	if err == nil || !strings.Contains(err.Error(), "row identity is missing") {
		t.Fatalf("missing row id must fail closed, got %v", err)
	}
	if p.count() != before+1 {
		t.Fatal("fail-closed mapping must still not reach the peer")
	}
}

// TestTwoStoragesDoNotShareAliases: distinct storage rows get distinct
// backing aliases even when they share a namespace, and distinct namespaces
// keep them apart across OpenList instances that share one peer.
func TestTwoStoragesDoNotShareAliases(t *testing.T) {
	p := newDriverPeer(t)
	d1, trash1, relay1 := identityDriver(t, p, 1)
	d2, _, _ := identityDriver(t, p, 2)
	d2.Addition.SystemNS = d1.Addition.SystemNS // force the shared-namespace case
	base2 := ".goapi-system-" + d2.Addition.SystemNS + "-2"
	trash2, relay2 := base2+"-trash", base2+"-relay"

	if trash1 == trash2 || relay1 == relay2 || trash1 == relay1 || trash2 == relay2 {
		t.Fatalf("aliases must be pairwise distinct: %q %q %q %q", trash1, relay1, trash2, relay2)
	}
	// Same row id, fresh namespace (another OpenList instance): different
	// alias, so instances sharing one peer key never collide.
	d3, trash3, _ := identityDriver(t, p, 1)
	if d3.Addition.SystemNS == d1.Addition.SystemNS {
		t.Fatal("independent inits must generate independent namespaces")
	}
	if trash3 == trash1 {
		t.Fatal("same row id with a different namespace must not share the alias")
	}

	// Over the wire both storages write into their own alias only.
	routeMutatingEcho(t, p)
	wire := captureWire(t, p)
	if err := d1.MakeDir(t.Context(), &model.Object{Path: "/.c2a-trash"}, "bucket"); err != nil {
		t.Fatalf("d1 MakeDir: %v", err)
	}
	if err := d2.MakeDir(t.Context(), &model.Object{Path: "/.c2a-trash"}, "bucket"); err != nil {
		t.Fatalf("d2 MakeDir: %v", err)
	}
	saw1, saw2 := false, false
	for _, r := range wire.all() {
		if !strings.HasSuffix(r.Path, "/mkdir") {
			continue
		}
		if strings.Contains(r.Body, `"/`+trash1+`/bucket"`) {
			saw1 = true
		}
		if strings.Contains(r.Body, `"/`+trash2+`/bucket"`) {
			saw2 = true
		}
	}
	if !saw1 || !saw2 {
		t.Fatalf("each storage must use its own alias (d1=%v, d2=%v)", saw1, saw2)
	}
	noVirtualLeak(t, wire)
}

// TestListingConflictFailsClosed: if the peer state would make a relabeled
// alias collide with another entry (e.g. a peer-side directory that is
// itself named /.c2a-trash), the listing fails instead of lying.
func TestListingConflictFailsClosed(t *testing.T) {
	p := newDriverPeer(t)
	d, trashAlias, _ := identityDriver(t, p, 7)
	routeList(t, p, func(dir string) (int, string) {
		if dir == "/" {
			return http.StatusOK, listBody([]string{
				entryJSON(".c2a-trash", "/.c2a-trash", true, 0),
				entryJSON(trashAlias, "/"+trashAlias, true, 0),
			})
		}
		return 0, ""
	})
	_, err := d.List(t.Context(), &model.Object{Path: "/"}, model.ListArgs{})
	if err == nil || !strings.Contains(err.Error(), "conflicts with the reserved system view") {
		t.Fatalf("colliding listing must fail closed, got %v", err)
	}
}

// TestSystemViewBijection: the mapping is bijective inside the storage's
// view — forward then back is the identity for every path, back then
// forward is the identity for every peer path — and only the exact first
// segment maps.
func TestSystemViewBijection(t *testing.T) {
	ns := strings.Repeat("ab", 16)
	mk := func(root string) *GoAPI {
		d := &GoAPI{Addition: Addition{RootPath: rootAt(root), SystemNS: ns}}
		d.ID = 9
		return d
	}
	trashAlias := "/.goapi-system-" + ns + "-9-trash"
	relayAlias := "/.goapi-system-" + ns + "-9-relay"
	other := "/.goapi-system-" + strings.Repeat("cd", 16) + "-4-trash"

	d := mk("/")
	for _, p := range []string{
		"/",
		"/a",
		"/a/b/c",
		"/.c2a-trash",
		"/.c2a-trash/2026-10-14",
		"/.c2a-trash/2026-10-14/deep/name.txt",
		"/.relay",
		"/.relay/some-id/name.bin",
		"/.c2a-trashx",
		"/sub/.relay",
		other,
	} {
		mapped, err := d.mapToPeer(p)
		if err != nil {
			t.Fatalf("mapToPeer(%q): %v", p, err)
		}
		if back := d.mapFromPeer(mapped); back != p {
			t.Fatalf("round trip %q -> %q -> %q", p, mapped, back)
		}
	}
	// The alias side maps back to the virtual view and is stable too.
	for _, q := range []string{trashAlias, trashAlias + "/x", relayAlias, relayAlias + "/y/z", "/a", other} {
		virtual := d.mapFromPeer(q)
		again, err := d.mapToPeer(virtual)
		if err != nil {
			t.Fatalf("mapToPeer(%q): %v", virtual, err)
		}
		if again != q {
			t.Fatalf("reverse round trip %q -> %q -> %q", q, virtual, again)
		}
	}
	// Exact rewrites and non-rewrites.
	if got, _ := d.mapToPeer("/.c2a-trash/x"); got != trashAlias+"/x" {
		t.Fatalf("trash mapping = %q", got)
	}
	if got, _ := d.mapToPeer("/.relay"); got != relayAlias {
		t.Fatalf("relay mapping = %q", got)
	}
	if got, _ := d.mapToPeer("/.c2a-trashx"); got != "/.c2a-trashx" {
		t.Fatalf("lookalike must not map, got %q", got)
	}
	if got, _ := d.mapToPeer("/sub/.relay"); got != "/sub/.relay" {
		t.Fatalf("nested reserved name must not map, got %q", got)
	}
	// Own aliases are refused outright.
	for _, q := range []string{trashAlias, trashAlias + "/x", relayAlias, relayAlias + "/y"} {
		if _, err := d.mapToPeer(q); err == nil {
			t.Fatalf("alias %q must be refused", q)
		}
	}

	// The same bijection holds beneath a nested root.
	dr := mk("/data")
	for _, p := range []string{"/data/a", "/data/.c2a-trash/x", "/data/.relay/i/f", "/data/.relayx"} {
		mapped, err := dr.mapToPeer(p)
		if err != nil {
			t.Fatalf("mapToPeer(%q): %v", p, err)
		}
		if back := dr.mapFromPeer(mapped); back != p {
			t.Fatalf("rooted round trip %q -> %q -> %q", p, mapped, back)
		}
	}
	if got, _ := dr.mapToPeer("/data/.c2a-trash/x"); got != "/data"+trashAlias+"/x" {
		t.Fatalf("rooted trash mapping = %q", got)
	}
	if got := dr.mapFromPeer("/data" + trashAlias + "/x"); got != "/data/.c2a-trash/x" {
		t.Fatalf("rooted reverse = %q", got)
	}
}

// crc64Of computes the receipt CRC-64 of a body for this file's tests.
func crc64Of(b []byte) uint64 {
	c := crc64.New(goapiclient.CRCTable())
	_, _ = c.Write(b)
	return c.Sum64()
}
