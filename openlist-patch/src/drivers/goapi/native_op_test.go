package goapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	stdpath "path"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/goapiclient"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// The whole test binary shares one in-memory database (as upstream's op
// tests do); native tests use their own mount paths.
func init() {
	conf.Conf = conf.DefaultConfig("data")
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: conf.Conf.Database.TablePrefix},
	})
	if err != nil {
		panic("failed to connect database: " + err.Error())
	}
	db.Init(dB)
}

// nativePeer is a stateful in-memory GoAPI backend: real directory and file
// semantics (stat 404 until created, listings from actual state, moves move
// bytes), so native op flows run against something that behaves like the
// peer instead of pre-populated fixtures.
type nativePeer struct {
	t *testing.T
	p *dpeer

	mu    sync.Mutex
	dirs  map[string]bool
	files map[string][]byte
	hops  []nativeHop
}

type nativeHop struct {
	method, path, hop string
}

func newNativePeer(t *testing.T) *nativePeer {
	t.Helper()
	p := newDriverPeer(t)
	n := &nativePeer{t: t, p: p, dirs: map[string]bool{"/": true}, files: map[string][]byte{}}
	p.s.Config.Handler = http.HandlerFunc(n.handle)
	return n
}

func (n *nativePeer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/me" {
		n.p.handle(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	n.mu.Lock()
	n.hops = append(n.hops, nativeHop{method: r.Method, path: r.URL.Path, hop: r.Header.Get(goapiclient.HopHeader)})
	n.mu.Unlock()
	// The real peer cleans request paths (the driver's Init probes the root
	// as "/<root>/"), so the backend keys state by the cleaned path but
	// echoes every path exactly as it was asked (the client contract).
	clean := func(p string) string {
		p = stdpath.Clean("/" + p)
		if p == "/" {
			return p
		}
		return strings.TrimSuffix(p, "/")
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/stat"):
		n.stat(w, r.URL.Query().Get("path"), clean(r.URL.Query().Get("path")))
	case strings.HasSuffix(r.URL.Path, "/list"):
		n.list(w, clean(r.URL.Query().Get("path")))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mkdir"):
		var req struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(body, &req)
		n.mu.Lock()
		n.dirs[clean(req.Path)] = true
		n.mu.Unlock()
		n.p.out(w, http.StatusOK, fmt.Sprintf(`{"path":%q,"created":true}`, req.Path))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/move"):
		var req struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		_ = json.Unmarshal(body, &req)
		from, to := clean(req.From), clean(req.To)
		n.mu.Lock()
		if data, ok := n.files[from]; ok {
			delete(n.files, from)
			n.files[to] = data
		}
		if n.dirs[from] {
			delete(n.dirs, from)
			n.dirs[to] = true
		}
		n.mu.Unlock()
		n.p.out(w, http.StatusOK, fmt.Sprintf(`{"from":%q,"to":%q}`, req.From, req.To))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
		var req struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(body, &req)
		n.mu.Lock()
		for _, raw := range req.Paths {
			path := clean(raw)
			delete(n.files, path)
			for d := range n.dirs {
				if d == path || strings.HasPrefix(d, path+"/") {
					delete(n.dirs, d)
				}
			}
		}
		n.mu.Unlock()
		results := make([]string, 0, len(req.Paths))
		for _, path := range req.Paths {
			results = append(results, fmt.Sprintf(`{"path":%q,"status":"deleted"}`, path))
		}
		n.p.out(w, http.StatusOK, `{"results":[`+strings.Join(results, ",")+`]}`)
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/files"):
		data, _ := io.ReadAll(r.Body)
		path := clean(r.URL.Query().Get("path"))
		n.mu.Lock()
		n.files[path] = data
		n.mu.Unlock()
		crc := crc64Of(data)
		n.p.out(w, http.StatusOK, fmt.Sprintf(
			`{"path":%q,"size":%d,"crc64":%q,"sha256":%q,"conflict":"created"}`,
			r.URL.Query().Get("path"), len(data), strconv.FormatUint(crc, 10), sha256Hex(data)))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files"):
		n.mu.Lock()
		data, ok := n.files[clean(r.URL.Query().Get("path"))]
		n.mu.Unlock()
		if !ok {
			n.p.out(w, http.StatusNotFound, notFoundBody)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	default:
		n.p.out(w, http.StatusNotFound, notFoundBody)
	}
}

func (n *nativePeer) stat(w http.ResponseWriter, raw, path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, isDir := n.dirs[path]
	data, isFile := n.files[path]
	if !isDir && !isFile {
		n.p.out(w, http.StatusNotFound, notFoundBody)
		return
	}
	n.p.out(w, http.StatusOK, fmt.Sprintf(
		`{"path":%q,"name":%q,"is_dir":%t,"size":%d,"modified":"2026-10-14T00:00:00Z","hashes":{}}`,
		raw, stdpath.Base(path), isDir, len(data)))
}

func (n *nativePeer) list(w http.ResponseWriter, dir string) {
	// dir is already normalized by the handler.
	n.mu.Lock()
	defer n.mu.Unlock()
	items := make([]string, 0, 4)
	for path := range n.dirs {
		if path != "/" && stdpath.Dir(path) == dir {
			items = append(items, entryJSON(stdpath.Base(path), path, true, 0))
		}
	}
	for path, data := range n.files {
		if stdpath.Dir(path) == dir {
			items = append(items, entryJSON(stdpath.Base(path), path, false, int64(len(data))))
		}
	}
	n.p.out(w, http.StatusOK, listBody(items))
}

func (n *nativePeer) sawHop(method, suffix, hop string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, h := range n.hops {
		if h.method == method && strings.HasSuffix(h.path, suffix) && h.hop == hop {
			return true
		}
	}
	return false
}

func (n *nativePeer) forceDir(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dirs[path] = true
}

func (n *nativePeer) hasDir(path string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dirs[path]
}

func (n *nativePeer) fileBytes(path string) []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.files[path]
}

// createNativeStorage runs the real op.CreateStorage against the driver and
// returns the concrete driver instance (for identity) and its alias names.
func createNativeStorage(t *testing.T, n *nativePeer, mount, root string) (*GoAPI, string, string) {
	t.Helper()
	addition, err := json.Marshal(map[string]any{
		"url":              n.p.s.URL,
		"api_key":          n.p.key,
		"drive":            "d1",
		"root_folder_path": root,
	})
	if err != nil {
		t.Fatalf("addition: %v", err)
	}
	st := model.Storage{
		MountPath:       mount,
		Driver:          "GoAPI",
		Addition:        string(addition),
		CacheExpiration: 30,
	}
	if _, err := op.CreateStorage(n.t.Context(), st); err != nil {
		t.Fatalf("CreateStorage(%s): %v", mount, err)
	}
	storage, err := op.GetStorageByMountPath(mount)
	if err != nil {
		t.Fatalf("GetStorageByMountPath: %v", err)
	}
	g, ok := storage.(*GoAPI)
	if !ok {
		t.Fatalf("mounted driver is %T, want *GoAPI", storage)
	}
	ns := g.Addition.SystemNS
	if !validNamespace(ns) {
		t.Fatalf("native init must commit a canonical namespace, got %q", ns)
	}
	base := ".goapi-system-" + ns + "-" + strconv.FormatUint(uint64(g.Storage.ID), 10)
	return g, base + "-trash", base + "-relay"
}

// TestNativeOpSystemView runs real op-layer flows against the driver: the
// reserved root is created through native op.MakeDir (nothing pre-populated),
// found through op.Get while the root directory cache is stale (the exact
// production failure), nested creation recurses through it, uploads, trash
// moves, restore and readback all resolve through the virtual view.
func TestNativeOpSystemView(t *testing.T) {
	n := newNativePeer(t)

	// Storage with root "/": virtual view and peer space coincide.
	g, trashAlias, relayAlias := createNativeStorage(t, n, "/goapi-native", "/")

	putFile := func(path, name, content string) {
		t.Helper()
		fs := &stream.FileStream{Obj: &model.Object{Name: name, Size: int64(len(content))}, Reader: strings.NewReader(content)}
		if err := op.Put(n.t.Context(), g, path, fs, func(float64) {}); err != nil {
			t.Fatalf("op.Put %s/%s: %v", path, name, err)
		}
	}

	// Seed a regular file and warm the root directory cache: from here on
	// the cached root listing does NOT know anything created afterwards.
	putFile("/", "hello.txt", "hello bytes")
	if _, err := op.List(n.t.Context(), g, "/", model.ListArgs{}); err != nil {
		t.Fatalf("op.List root: %v", err)
	}

	// The reserved root appears on the peer out-of-band — the lab scenario:
	// this storage's own alias from an earlier life of the same row, while
	// the cached root listing predates it. No op call created it here, so
	// there is no refresh trigger: only the driver's stat (driver.Getter)
	// can resolve the name, anything else answers object-not-found.
	n.forceDir("/" + trashAlias)
	obj, err := op.GetUnwrap(n.t.Context(), g, "/.c2a-trash")
	if err != nil {
		t.Fatalf("op.Get /.c2a-trash with stale root cache: %v", err)
	}
	if obj.GetName() != ".c2a-trash" || obj.GetPath() != "/.c2a-trash" || !obj.IsDir() {
		t.Fatalf("virtual root object = %+v", obj)
	}
	// A refreshed listing presents the alias under its virtual name.
	objs, err := op.List(n.t.Context(), g, "/", model.ListArgs{Refresh: true})
	if err != nil {
		t.Fatalf("op.List refresh: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.GetName() == ".c2a-trash" && o.GetPath() == "/.c2a-trash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("refreshed root listing must relabel the alias, got %+v", objs)
	}

	// The reserved system root is created by native op.MakeDir with the root
	// object and the reserved name — it must land on the backing alias.
	if err := op.MakeDir(n.t.Context(), g, "/.relay"); err != nil {
		t.Fatalf("op.MakeDir /.relay: %v", err)
	}
	if !n.hasDir("/" + relayAlias) {
		t.Fatal("native mkdir must create the backing alias on the peer")
	}
	// op.Get right after the creation: the stale root cache has no such
	// entry, so only the driver's Getter can find it.
	relayObj, err := op.GetUnwrap(n.t.Context(), g, "/.relay")
	if err != nil {
		t.Fatalf("op.Get /.relay after creation: %v", err)
	}
	if relayObj.GetName() != ".relay" || relayObj.GetPath() != "/.relay" || !relayObj.IsDir() {
		t.Fatalf("virtual relay root object = %+v", relayObj)
	}

	// Nested creation recurses through the virtual root.
	if err := op.MakeDir(n.t.Context(), g, "/.c2a-trash/2026-10-14"); err != nil {
		t.Fatalf("op.MakeDir trash bucket: %v", err)
	}

	// Upload into the virtual view and read it back through op.Get.
	putFile("/.c2a-trash/2026-10-14", "report.txt", "native trash bytes")
	got, err := op.GetUnwrap(n.t.Context(), g, "/.c2a-trash/2026-10-14/report.txt")
	if err != nil {
		t.Fatalf("op.Get trashed file: %v", err)
	}
	if got.GetName() != "report.txt" || got.GetPath() != "/.c2a-trash/2026-10-14/report.txt" || got.GetSize() != int64(len("native trash bytes")) {
		t.Fatalf("trashed file object = %+v", got)
	}

	// Trash move of a second regular file, then the restore path of the
	// gateway: move it back out and read the bytes again.
	putFile("/", "report.txt", "restore me")
	if _, err := op.Get(n.t.Context(), g, "/report.txt"); err != nil {
		t.Fatalf("op.Get source: %v", err)
	}
	if err := op.Move(n.t.Context(), g, "/report.txt", "/.c2a-trash/2026-10-14"); err != nil {
		t.Fatalf("op.Move into trash: %v", err)
	}
	if _, err := op.GetUnwrap(n.t.Context(), g, "/report.txt"); !errs.IsObjectNotFound(err) {
		t.Fatalf("moved source must be object-not-found, got %v", err)
	}
	if err := op.Move(n.t.Context(), g, "/.c2a-trash/2026-10-14/report.txt", "/"); err != nil {
		t.Fatalf("op.Move restore: %v", err)
	}
	restored, err := op.GetUnwrap(n.t.Context(), g, "/report.txt")
	if err != nil {
		t.Fatalf("op.Get restored: %v", err)
	}
	if restored.GetName() != "report.txt" {
		t.Fatalf("restored object = %+v", restored)
	}
	link, err := g.Link(n.t.Context(), &model.Object{Path: restored.GetPath(), Size: restored.GetSize()}, model.LinkArgs{})
	if err != nil {
		t.Fatalf("Link restored: %v", err)
	}
	rc, err := link.RangeReader.RangeRead(n.t.Context(), http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "restore me" {
		t.Fatalf("readback = %q", string(data))
	}

	// The physical backing alias is not addressable through the view.
	if _, err := op.GetUnwrap(n.t.Context(), g, "/"+trashAlias); err == nil ||
		!strings.Contains(err.Error(), "cannot be addressed directly") {
		t.Fatalf("native Get of the alias must be refused, got %v", err)
	}
	if err := op.MakeDir(n.t.Context(), g, "/"+trashAlias); err == nil ||
		!strings.Contains(err.Error(), "cannot be addressed directly") {
		t.Fatalf("native mkdir of the alias must be refused, got %v", err)
	}

	// A missing regular path maps to object-not-found for the op layer.
	if _, err := op.Get(n.t.Context(), g, "/missing.txt"); !errs.IsObjectNotFound(err) {
		t.Fatalf("missing path must map to object not found, got %v", err)
	}

	// Hop context propagates through native op calls into the driver: probe
	// a path whose parent directory was never listed, so no cached entry can
	// serve it — only the Getter (and its stat) can resolve it.
	putFile("/.c2a-trash/2026-10-14", "hop.txt", "hop")
	if _, err := op.Get(goapiclient.WithHop(n.t.Context(), 2), g, "/.c2a-trash/2026-10-14/hop.txt"); err != nil {
		t.Fatalf("op.Get with hop: %v", err)
	}
	if !n.sawHop(http.MethodGet, "/stat", "3") {
		t.Fatal("the Getter stat must carry the propagated hop")
	}
}

// TestNativeOpSystemViewNestedRoot: the same virtual view beneath a
// configured root — everything stays inside <root> and the alias is private
// to this storage.
func TestNativeOpSystemViewNestedRoot(t *testing.T) {
	n := newNativePeer(t)
	n.mu.Lock()
	n.dirs["/data"] = true
	n.mu.Unlock()

	g, trashAlias, _ := createNativeStorage(t, n, "/goapi-native-data", "/data")

	if err := op.MakeDir(n.t.Context(), g, "/.c2a-trash"); err != nil {
		t.Fatalf("op.MakeDir /.c2a-trash (nested root): %v", err)
	}
	if !n.hasDir("/data/" + trashAlias) {
		t.Fatal("nested-root mkdir must create the alias inside the configured root")
	}
	obj, err := op.GetUnwrap(n.t.Context(), g, "/.c2a-trash")
	if err != nil {
		t.Fatalf("op.Get nested virtual root: %v", err)
	}
	if obj.GetPath() != "/data/.c2a-trash" || obj.GetName() != ".c2a-trash" {
		t.Fatalf("nested virtual root object = %+v", obj)
	}
	fs := &stream.FileStream{Obj: &model.Object{Name: "f.txt", Size: 2}, Reader: strings.NewReader("ok")}
	if err := op.Put(n.t.Context(), g, "/.c2a-trash", fs, func(float64) {}); err != nil {
		t.Fatalf("op.Put into nested virtual root: %v", err)
	}
	if got, err := op.GetUnwrap(n.t.Context(), g, "/.c2a-trash/f.txt"); err != nil || got.GetPath() != "/data/.c2a-trash/f.txt" {
		t.Fatalf("nested virtual file = %+v, %v", got, err)
	}
}
