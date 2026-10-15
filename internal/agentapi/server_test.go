package agentapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud2api/internal/auth"
	"cloud2api/internal/config"
	"cloud2api/internal/gateway"
	"cloud2api/internal/keymgr"
	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// ---- fake OpenList (fs API + admin storage list + /d downloads) ----

type fakeEntry struct {
	name     string
	isDir    bool
	content  []byte
	modified string
}

// fakeStorage is one entry of the fake's /api/admin/storage/* model.
type fakeStorage struct {
	id       int64
	mount    string
	driver   string
	status   string // "" = work
	disabled bool
}

type fakeOL struct {
	mu      sync.Mutex
	entries map[string]*fakeEntry // absolute internal path -> entry
	nextID  int64
	puts    []string // internal paths uploaded

	// mount1 is the live mount path of storage 1; tests rename it to
	// simulate a user editing the storage in OpenList (Phase 1c).
	mount1 string

	// status1 overrides storage 1's status text ("" = work). Phase 2: token
	// failures classify as needs_reauth and the data plane answers 503 with
	// a re-login request hint.
	status1 string

	// storages is the admin storage list; storages[0] and [1] are the two
	// default drives (mount1/status1 override the first entry). Tests add or
	// delete entries beyond them to swap storages (replacement scenario).
	storages []fakeStorage
	nextSID  int64 // id allocator for storages added by a test

	// fsCalls records every /api/fs/* and /d/* path the fake served, for the
	// "no file access after a storage swap" assertions.
	fsCalls []string

	// fsHeaders records the request headers of every /api/fs/* call (relay
	// hop propagation assertions).
	fsHeaders []http.Header

	// failAdmin makes the admin storage endpoints answer code 500 (an
	// OpenList that is reachable but cannot verify storages).
	failAdmin bool

	// putReject makes /api/fs/put answer a 500 business error (upload
	// receipts must record failed). putErrMsg overrides the message.
	putReject    bool
	putErrMsg    string
	putGarbage   bool // answer 200 with a non-JSON body (no success envelope)
	putSleep     time.Duration
	putAsyncTask bool // answer with a queued task nested under data.task (fork shape)
	putStarted   chan<- struct{}
	putRelease   <-chan struct{}

	// task bookkeeping (fs/copy, fs/move, offline download)
	tasks      map[string]*fakeTask // id -> task
	taskOrder  []string
	taskSeq    int
	taskDelay  time.Duration // how long tasks stay running
	olRequests []string      // captured api paths

	// pTruncateTo, when >= 0, makes the /d download endpoint send only that
	// many body bytes (simulating a truncated or short upstream). With
	// pChunked the response omits Content-Length (chunked); otherwise the full
	// Content-Length is declared and the handled short-writes.
	pTruncateTo int
	pChunked    bool

	// dRedirect makes the /d download endpoint answer a 302 to a direct-link
	// URL (served at /direct/...) instead of proxying the bytes, mirroring a
	// storage without MustProxy. DoDownload must follow it server-side.
	dRedirect bool

	// pHits counts requests the fake answered on the restricted /p download
	// route; every cloud2api flow must leave it at zero.
	pHits int

	// directHits counts /direct/* requests served (the 302 targets of a
	// dRedirect /d), proving the helper followed the redirect server-side.
	directHits int

	// dQueries records the raw query string of every /d request, for the
	// upstream query whitelist assertions.
	dQueries []string
}

type fakeTask struct {
	id      string
	kind    string // copy | move | offline_download
	state   int    // 1 running, 2 succeeded, 4 canceled, 7 failed
	err     string
	srcDir  string
	dstDir  string
	names   []string
	url     string
	started time.Time
}

func newFakeOL() *fakeOL {
	return &fakeOL{
		entries: map[string]*fakeEntry{}, mount1: "/m1", nextID: 5,
		storages: []fakeStorage{
			{id: 1, mount: "/m1", driver: "Local"},
			{id: 2, mount: "/m2", driver: "Local"},
		},
		nextSID: 21, tasks: map[string]*fakeTask{}, pTruncateTo: -1,
	}
}

// storageSnapshot returns the live admin storage list (mount1/status1 are the
// source of truth for storage 1).
func (f *fakeOL) storageSnapshot() []fakeStorage {
	out := append([]fakeStorage{}, f.storages...)
	if len(out) > 0 {
		out[0].mount = f.mount1
		out[0].status = f.status1
	}
	return out
}

// addStorage registers a new storage (like an admin creating one in OpenList)
// and returns its id.
func (f *fakeOL) addStorage(mount, driver string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSID++
	f.storages = append(f.storages, fakeStorage{id: f.nextSID, mount: mount, driver: driver})
	return f.nextSID
}

// deleteStorage removes a storage by id (like an admin deleting it in
// OpenList). The default storages 1 and 2 back the harness drives; swap tests
// delete storages they added themselves.
func (f *fakeOL) deleteStorage(id int64) {
	kept := f.storages[:0]
	for _, st := range f.storages {
		if st.id != id {
			kept = append(kept, st)
		}
	}
	f.storages = kept
}

// fsCallsUnder returns the fs/​p calls whose path falls under mount.
func (f *fakeOL) fsCallsUnder(mount string) []string {
	var out []string
	for _, p := range f.fsCalls {
		if p == mount || strings.HasPrefix(p, mount+"/") {
			out = append(out, p)
		}
	}
	return out
}

// renameStorage renames any storage's mount and re-prefixes its entries
// (general form of renameMount1 for swap-test storages).
func (f *fakeOL) renameStorage(id int64, from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.storages {
		if f.storages[i].id == id {
			f.storages[i].mount = to
		}
	}
	entries := make(map[string]*fakeEntry, len(f.entries))
	for p, e := range f.entries {
		if p == from || strings.HasPrefix(p, from+"/") {
			entries[strings.Replace(p, from, to, 1)] = e
		} else {
			entries[p] = e
		}
	}
	f.entries = entries
}

// renameMount1 renames storage 1's mount and re-prefixes its entries, like a
// user editing the storage in OpenList would.
func (f *fakeOL) renameMount1(to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mount1 = to
	entries := make(map[string]*fakeEntry, len(f.entries))
	for p, e := range f.entries {
		if p == "/m1" || strings.HasPrefix(p, "/m1/") {
			entries[strings.Replace(p, "/m1", to, 1)] = e
		} else {
			entries[p] = e
		}
	}
	f.entries = entries
}

// storageMissing reports whether an internal path belongs to storage 1's
// namespace but no longer matches its live mount.
func (f *fakeOL) storageMissing(internal string) bool {
	if f.mount1 == "/m1" {
		return false
	}
	return internal == "/m1" || strings.HasPrefix(internal, "/m1/")
}

func (f *fakeOL) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/put" && r.Method == http.MethodPut {
			f.mu.Lock()
			started, release := f.putStarted, f.putRelease
			f.mu.Unlock()
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
			}
			// Waiting must not block unrelated fake upstream requests on f.mu.
			if release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.olRequests = append(f.olRequests, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/fs/") || strings.HasPrefix(r.URL.Path, "/d/") {
			f.fsCalls = append(f.fsCalls, r.URL.Path)
			f.fsHeaders = append(f.fsHeaders, r.Header.Clone())
		}
		switch {
		case r.URL.Path == "/api/admin/storage/list" && r.Method == http.MethodGet:
			if f.failAdmin {
				writeOL(w, 500, "admin api broken", nil)
				return
			}
			// Paged like the real admin API: page/per_page honoured, total
			// reported with every page.
			page, perPage := 1, 200
			if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
				page = v
			}
			if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
				perPage = v
			}
			snap := f.storageSnapshot()
			lo := (page - 1) * perPage
			hi := lo + perPage
			if lo > len(snap) {
				lo = len(snap)
			}
			if hi > len(snap) {
				hi = len(snap)
			}
			content := []map[string]any{}
			for _, st := range snap[lo:hi] {
				content = append(content, map[string]any{
					"id": st.id, "mount_path": st.mount, "driver": st.driver,
					"status": st.statusText(), "disabled": st.disabled, "enabled": !st.disabled,
				})
			}
			writeOL(w, 200, "success", map[string]any{"content": content, "total": len(snap)})
		case r.URL.Path == "/api/admin/storage/get" && r.Method == http.MethodGet:
			if f.failAdmin {
				writeOL(w, 500, "admin api broken", nil)
				return
			}
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			for _, st := range f.storageSnapshot() {
				if st.id == id {
					writeOL(w, 200, "success", map[string]any{
						"id": st.id, "mount_path": st.mount, "driver": st.driver,
						"status": st.statusText(), "disabled": st.disabled, "enabled": !st.disabled,
					})
					return
				}
			}
			// The live instance answers this shape for a deleted id.
			writeOL(w, 500, "record not found", nil)
		case r.URL.Path == "/api/fs/get" && r.Method == http.MethodPost:
			var body struct {
				Path string `json:"path"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if f.storageMissing(path.Clean(body.Path)) {
				writeOL(w, 500, "failed get storage: storage not found; rawPath: "+body.Path, nil)
				return
			}
			e, ok := f.entries[path.Clean(body.Path)]
			if !ok {
				writeOL(w, 500, "failed get fs: object not found", nil)
				return
			}
			writeOL(w, 200, "success", map[string]any{
				"name": e.name, "size": len(e.content), "is_dir": e.isDir,
				"modified": e.modified, "sign": "sig:0",
			})
		case r.URL.Path == "/api/fs/list" && r.Method == http.MethodPost:
			var body struct {
				Path    string `json:"path"`
				Page    int    `json:"page"`
				PerPage int    `json:"per_page"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if f.storageMissing(path.Clean(body.Path)) {
				writeOL(w, 500, "failed get storage: storage not found; rawPath: "+body.Path, nil)
				return
			}
			dir := path.Clean(body.Path)
			var content []map[string]any
			for p, e := range f.entries {
				if path.Dir(p) == dir {
					content = append(content, map[string]any{
						"name": e.name, "size": len(e.content), "is_dir": e.isDir,
						"modified": e.modified, "sign": "sig:0",
					})
				}
			}
			sort.Slice(content, func(i, j int) bool {
				return content[i]["name"].(string) < content[j]["name"].(string)
			})
			if content == nil {
				content = []map[string]any{}
			}
			writeOL(w, 200, "success", map[string]any{"content": content, "total": len(content)})
		case r.URL.Path == "/api/fs/mkdir" && r.Method == http.MethodPost:
			var body struct {
				Path string `json:"path"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			p := path.Clean(body.Path)
			// create every ancestor like the real driver does
			for cur := p; cur != "/" && cur != "."; cur = path.Dir(cur) {
				if _, exists := f.entries[cur]; !exists {
					f.entries[cur] = &fakeEntry{name: path.Base(cur), isDir: true, modified: "2026-10-12T00:00:00Z"}
				}
			}
			writeOL(w, 200, "success", nil)
		case r.URL.Path == "/api/fs/remove" && r.Method == http.MethodPost:
			var body struct {
				Dir   string   `json:"dir"`
				Names []string `json:"names"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, n := range body.Names {
				delete(f.entries, path.Join(path.Clean(body.Dir), n))
			}
			writeOL(w, 200, "success", nil)
		case r.URL.Path == "/api/fs/move" && r.Method == http.MethodPost:
			var body struct {
				SrcDir string   `json:"src_dir"`
				DstDir string   `json:"dst_dir"`
				Names  []string `json:"names"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, n := range body.Names {
				src := path.Join(path.Clean(body.SrcDir), n)
				e, ok := f.entries[src]
				if !ok {
					writeOL(w, 500, "failed move: object not found", nil)
					return
				}
				delete(f.entries, src)
				dst := path.Join(path.Clean(body.DstDir), n)
				e.name = path.Base(dst)
				f.entries[dst] = e
			}
			writeOL(w, 200, "success", nil)
		case r.URL.Path == "/api/fs/rename" && r.Method == http.MethodPost:
			var body struct {
				Path string `json:"path"`
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			e, ok := f.entries[path.Clean(body.Path)]
			if !ok {
				writeOL(w, 500, "failed rename: object not found", nil)
				return
			}
			delete(f.entries, path.Clean(body.Path))
			dst := path.Join(path.Dir(path.Clean(body.Path)), body.Name)
			e.name = body.Name
			f.entries[dst] = e
			writeOL(w, 200, "success", nil)
		case r.URL.Path == "/api/fs/put" && r.Method == http.MethodPut:
			fp, _ := url.QueryUnescape(r.Header.Get("File-Path"))
			content, _ := io.ReadAll(r.Body)
			p := path.Clean(fp)
			f.entries[p] = &fakeEntry{name: path.Base(p), content: content, modified: "2026-10-12T00:00:00Z"}
			f.puts = append(f.puts, p)
			if f.putSleep > 0 {
				time.Sleep(f.putSleep)
			}
			if f.putGarbage {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(200)
				w.Write([]byte("<html>upstream error page, not json</html>"))
				return
			}
			if f.putReject {
				msg := f.putErrMsg
				if msg == "" {
					msg = "put rejected by upstream"
				}
				writeOL(w, 500, msg, nil)
				return
			}
			if f.putAsyncTask {
				// The patched fork's shape: common.SuccessResp nesting the
				// task info under data.task (no top-level task_id).
				t := f.submitTask("upload", path.Dir(p), "", nil, "")
				writeOL(w, 200, "success", map[string]any{"task": t.view()})
				return
			}
			writeOL(w, 200, "success", nil)
		case strings.HasPrefix(r.URL.Path, "/p/") && r.Method == http.MethodGet:
			// A storage can reject /p while permitting /d redirects.
			// Match that production failure without requiring a real drive.
			f.pHits++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":403,"message":"proxy download is disabled"}`))

		case strings.HasPrefix(r.URL.Path, "/d/") && r.Method == http.MethodGet:
			// /d is sign-gated like the live instance: only the server-fetched
			// sign (plus a whitelisted type selector) may appear.
			f.dQueries = append(f.dQueries, r.URL.RawQuery)
			if r.URL.Query().Get("sign") != "sig:0" {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("invalid or missing sign"))
				return
			}
			for k := range r.URL.Query() {
				if k != "sign" && k != "type" {
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte("unexpected query parameter " + k))
					return
				}
			}
			p := path.Clean(strings.TrimPrefix(r.URL.Path, "/d"))
			e, ok := f.entries[p]
			if !ok {
				writeOL(w, 500, "object not found", nil)
				return
			}
			if f.dRedirect {
				// A storage without MustProxy: /d answers a 302 to the
				// direct link, which DoDownload follows server-side.
				http.Redirect(w, r, "/direct"+p, http.StatusFound)
				return
			}
			f.serveDownloadBody(w, r, e)

		case strings.HasPrefix(r.URL.Path, "/direct/") && r.Method == http.MethodGet:
			// The direct-link target of a /d 302.
			f.directHits++
			p := path.Clean(strings.TrimPrefix(r.URL.Path, "/direct"))
			e, ok := f.entries[p]
			if !ok {
				http.Error(w, "gone", http.StatusNotFound)
				return
			}
			f.serveDownloadBody(w, r, e)

		// ---- task-backed operations (Phase 1b) ----

		case (r.URL.Path == "/api/fs/copy" || r.URL.Path == "/api/fs/move") && r.Method == http.MethodPost:
			var body struct {
				SrcDir string   `json:"src_dir"`
				DstDir string   `json:"dst_dir"`
				Names  []string `json:"names"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, n := range body.Names {
				if _, ok := f.entries[path.Join(path.Clean(body.SrcDir), n)]; !ok {
					writeOL(w, 403, fmt.Sprintf("file [%s] not found", n), nil)
					return
				}
				if _, ok := f.entries[path.Join(path.Clean(body.DstDir), n)]; ok {
					writeOL(w, 403, fmt.Sprintf("file [%s] exists", n), nil)
					return
				}
			}
			task := f.submitTask(strings.TrimPrefix(r.URL.Path, "/api/fs/"), body.SrcDir, body.DstDir, body.Names, "")
			writeOL(w, 200, "success", map[string]any{"tasks": []any{task.view()}})

		case r.URL.Path == "/api/fs/add_offline_download" && r.Method == http.MethodPost:
			var body struct {
				Path string   `json:"path"`
				URLs []string `json:"urls"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			u := body.URLs[0]
			name := path.Base(strings.Split(u, "?")[0])
			if _, ok := f.entries[path.Join(path.Clean(body.Path), name)]; ok {
				writeOL(w, 403, fmt.Sprintf("file [%s] exists", name), nil)
				return
			}
			task := f.submitTask("offline_download", path.Clean(body.Path), path.Clean(body.Path), nil, u)
			writeOL(w, 200, "success", map[string]any{"tasks": []any{task.view()}})

		case strings.HasPrefix(r.URL.Path, "/api/task/") && strings.Contains(r.URL.Path, "/undone") && r.Method == http.MethodGet:
			writeOL(w, 200, "success", f.taskViews(false))

		case strings.HasPrefix(r.URL.Path, "/api/task/") && strings.Contains(r.URL.Path, "/done") && r.Method == http.MethodGet:
			f.advance() // time passes between polls
			writeOL(w, 200, "success", f.taskViews(true))

		case strings.HasPrefix(r.URL.Path, "/api/task/") && strings.Contains(r.URL.Path, "/cancel") && r.Method == http.MethodPost:
			tid := r.URL.Query().Get("tid")
			if t, ok := f.tasks[tid]; ok && t.state == 1 {
				t.state = 4
				t.err = "context canceled"
			}
			writeOL(w, 200, "success", nil)

		case r.URL.Path == "/api/fs/search" && r.Method == http.MethodPost:
			writeOL(w, 404, "search not available", nil)

		default:
			writeOL(w, 404, "fake: unknown "+r.URL.Path, nil)
		}
	})
}

// statusText is the status the fake reports for a storage ("" → work).
func (st fakeStorage) statusText() string {
	if st.status == "" {
		return "work"
	}
	return st.status
}

// serveDownloadBody streams one fake entry with the range/truncation/chunked
// semantics shared by the fake's /d (proxy) and /direct (redirect target)
// download endpoints.
func (f *fakeOL) serveDownloadBody(w http.ResponseWriter, r *http.Request, e *fakeEntry) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	// Simulated revalidation hit: any If-None-Match outside a Range request
	// answers a 304 carrying the entity tag.
	if r.Header.Get("If-None-Match") != "" && r.Header.Get("Range") == "" {
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	status := http.StatusOK
	body := e.content
	if rng := r.Header.Get("Range"); rng != "" {
		if start, end, ok := parseFakeRange(rng, int64(len(e.content))); ok {
			body = e.content[start : end+1]
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(e.content)))
		} else {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(e.content)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	n := len(body)
	if f.pTruncateTo >= 0 && f.pTruncateTo < n {
		n = f.pTruncateTo
	}
	// A truncated upstream still declares the true length (the short
	// write is the failure being simulated); chunked mode omits
	// Content-Length and flushes so the framing is genuinely chunked.
	if !f.pChunked {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if f.pChunked {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	w.Write(body[:n])
}

func (f *fakeOL) submitTask(kind, srcDir, dstDir string, names []string, url string) *fakeTask {
	f.taskSeq++
	id := fmt.Sprintf("t%d", f.taskSeq)
	t := &fakeTask{
		id: id, kind: kind, state: 1,
		srcDir: path.Clean(srcDir), dstDir: path.Clean(dstDir),
		names: names, url: url, started: time.Now(),
	}
	f.tasks[id] = t
	f.taskOrder = append(f.taskOrder, id)
	return t
}

// advance moves running tasks past their delay, applying their effect.
func (f *fakeOL) advance() {
	for _, id := range f.taskOrder {
		t := f.tasks[id]
		if t.state != 1 || time.Since(t.started) < f.taskDelay {
			continue
		}
		switch t.kind {
		case "copy":
			for _, n := range t.names {
				src := path.Join(t.srcDir, n)
				dst := path.Join(t.dstDir, n)
				if e, ok := f.entries[src]; ok {
					content := make([]byte, len(e.content))
					copy(content, e.content)
					f.entries[dst] = &fakeEntry{name: n, isDir: e.isDir, content: content, modified: e.modified}
				}
			}
			t.state = 2
		case "move":
			for _, n := range t.names {
				src := path.Join(t.srcDir, n)
				dst := path.Join(t.dstDir, n)
				if e, ok := f.entries[src]; ok {
					delete(f.entries, src)
					f.entries[dst] = e
				}
			}
			t.state = 2
		case "offline_download":
			name := path.Base(strings.Split(t.url, "?")[0])
			content := []byte("offline-download-of " + t.url)
			f.entries[path.Join(t.dstDir, name)] = &fakeEntry{name: name, content: content, modified: "2026-10-12T00:00:00Z"}
			t.state = 2
		case "upload":
			// The put handler already stored the bytes; the queued task just
			// confirms them asynchronously.
			t.state = 2
		}
	}
}

func (f *fakeOL) taskViews(done bool) []map[string]any {
	out := []map[string]any{}
	for _, id := range f.taskOrder {
		t := f.tasks[id]
		terminal := t.state != 1 && t.state != 0
		if terminal != done {
			continue
		}
		out = append(out, t.view())
	}
	return out
}

func (t *fakeTask) view() map[string]any {
	progress := 100.0
	if t.state == 1 {
		progress = 42.0
	}
	return map[string]any{
		"id": t.id, "name": t.kind, "state": t.state, "progress": progress, "error": t.err,
	}
}

// parseFakeRange parses a single "bytes=a-b" client range for the fake /d
// download endpoint (open-ended "-b"/"a-" forms supported).
func parseFakeRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		if v, err := strconv.ParseInt(b, 10, 64); err == nil && v < end {
			end = v
		}
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

func writeOL(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	if data != nil {
		_ = enc.Encode(map[string]any{"code": code, "message": msg, "data": data})
	} else {
		_ = enc.Encode(map[string]any{"code": code, "message": msg})
	}
}

// ---- harness ----

const (
	chrootPlain = "sk-c2a-ab12cd34-chrootsecretchrootsecret12"
	legacyPlain = "sk-legacy-plain-key"
	adminPlain  = "sk-c2a-zz99zz88-adminsecretadminsecret34"
)

type harness struct {
	t       *testing.T
	handler http.Handler
	fake    *fakeOL
	st      *store.Store
	keys    *keymgr.Manager
	api     *Server
	upURL   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeOL()
	up := httptest.NewServer(fake.handler())
	t.Cleanup(up.Close)

	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	for _, d := range []store.Drive{
		{ID: "d1", Provider: "baidu", Driver: "Local", MountPath: "/m1", Enabled: true, Static: true, CreatedAt: time.Now()},
		{ID: "d2", Provider: "123pan", Driver: "Local", MountPath: "/m2", Enabled: true, Static: true, CreatedAt: time.Now()},
	} {
		if err := st.InsertDrive(d); err != nil {
			t.Fatal(err)
		}
	}

	client, err := openlist.New(up.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := keymgr.New(st)
	if err != nil {
		t.Fatal(err)
	}

	// chroot key: root /agents/x on d1.
	mk := func(id, plain string, drives []store.KeyDrive, global []string, mut ...func(*store.APIKey)) {
		k := store.APIKey{
			ID: id, Hash: hashOf(plain), CreatedAt: time.Now().UTC(),
			Drives: drives, GlobalPerms: global,
			Trash: store.TrashSettings{Enabled: true, Days: 7},
		}
		if pub, _, ok := splitNew(plain); ok {
			k.PublicID = pub
		}
		for _, m := range mut {
			m(&k)
		}
		if err := st.InsertKey(k); err != nil {
			t.Fatal(err)
		}
	}
	mk("chroot", chrootPlain, []store.KeyDrive{{
		Drive: "d1", Root: "/agents/x",
		Perms: []string{"list", "read", "write", "delete", "link"},
	}}, []string{"link"})
	mk("legacy", legacyPlain, []store.KeyDrive{{
		Drive: "d1", Root: "/",
		Perms: []string{"list", "read", "write", "delete", "transfer"},
	}}, []string{"list", "read", "write", "delete", "transfer"})
	mk("admin", adminPlain, []store.KeyDrive{{
		Drive: "d1", Root: "/",
		Perms: []string{"list", "read", "write", "delete"},
	}}, nil, func(k *store.APIKey) {
		n := 2
		k.Limits = store.Limits{RequestsPerMinute: &n}
	})
	if err := keys.Reload(); err != nil {
		t.Fatal(err)
	}

	api := New(keys, st, client, newRegistry(), nil)
	return &harness{t: t, handler: api.Handler(http.NotFoundHandler()), fake: fake, st: st, keys: keys, api: api, upURL: up.URL}
}

// newRegistry mounts the two test drives.
func newRegistry() *gateway.AccountRegistry {
	return gateway.NewAccountRegistry([]config.Account{
		{ID: "d1", Provider: "baidu", MountPath: "/m1", Enabled: true},
		{ID: "d2", Provider: "123pan", MountPath: "/m2", Enabled: true},
	})
}

func hashOf(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func splitNew(plain string) (string, string, bool) {
	return auth.SplitNewFormat(plain)
}

func (h *harness) do(method, target, key string, body []byte, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if key != "" {
		req.Header.Set("apikey", key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

func errCode(doc map[string]any) string {
	if doc == nil {
		return ""
	}
	if e, ok := doc["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

// ---- tests ----

func TestCRC64Vector(t *testing.T) {
	c := crc64.New(crc64Table)
	c.Write([]byte("123456789"))
	if got := c.Sum64(); got != 11051210869376104954 {
		t.Fatalf("CRC64(\"123456789\") = %d, want 11051210869376104954", got)
	}
}

func TestV1Auth(t *testing.T) {
	h := newHarness(t)
	// no key
	rec, doc := h.do("GET", "/v1/me", "", nil, nil)
	if rec.Code != 401 || errCode(doc) != "unauthorized" {
		t.Fatalf("no key = %d %v", rec.Code, doc)
	}
	// wrong key
	rec, _ = h.do("GET", "/v1/me", "sk-c2a-ab12cd34-wrong", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("wrong key = %d", rec.Code)
	}
	// apikey header
	rec, doc = h.do("GET", "/v1/me", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("apikey header = %d %v", rec.Code, doc)
	}
	// Bearer also accepted
	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+chrootPlain)
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("bearer = %d", rec2.Code)
	}
	// query parameter must NOT authenticate
	rec, _ = h.do("GET", "/v1/me?apikey="+chrootPlain, "", nil, nil)
	if rec.Code != 401 {
		t.Fatalf("query key = %d, want 401", rec.Code)
	}
	// revoked key -> 401
	if err := h.keys.Revoke("chroot"); err != nil {
		t.Fatal(err)
	}
	rec, doc = h.do("GET", "/v1/me", chrootPlain, nil, nil)
	if rec.Code != 401 || errCode(doc) != "unauthorized" {
		t.Fatalf("revoked = %d %v", rec.Code, doc)
	}
}

func TestV1MeShape(t *testing.T) {
	h := newHarness(t)
	rec, doc := h.do("GET", "/v1/me", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("me = %d %v", rec.Code, doc)
	}
	key := doc["key"].(map[string]any)
	if key["id"] != "chroot" || key["default_drive"] != "d1" {
		t.Errorf("key = %v", key)
	}
	drives := doc["drives"].([]any)
	if len(drives) != 1 {
		t.Fatalf("drives = %v", drives)
	}
	d := drives[0].(map[string]any)
	if d["id"] != "d1" || d["root"] != "/agents/x" || d["health"] != "healthy" || d["default"] != true {
		t.Errorf("drive = %v", d)
	}
	if _, ok := doc["policy_text"].(string); !ok {
		t.Error("policy_text missing")
	}
	if _, ok := doc["next_steps"].([]any); !ok {
		t.Error("next_steps missing")
	}
	// /v1/drives returns the same list
	rec, doc = h.do("GET", "/v1/drives", chrootPlain, nil, nil)
	if rec.Code != 200 || len(doc["drives"].([]any)) != 1 {
		t.Fatalf("drives = %d %v", rec.Code, doc)
	}
}

func TestChrootEnforcement(t *testing.T) {
	h := newHarness(t)
	seed := map[string]*fakeEntry{
		"/m1/agents/x/file.txt": {name: "file.txt", content: []byte("hi"), modified: "2026-10-12T00:00:00Z"},
		"/m1/secret.txt":        {name: "secret.txt", content: []byte("s"), modified: "2026-10-12T00:00:00Z"},
	}
	for p, e := range seed {
		h.fake.entries[p] = e
	}

	// Inside the root works.
	rec, doc := h.do("GET", "/v1/list?path=/", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("list root = %d %v", rec.Code, doc)
	}
	items := doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "file.txt" {
		t.Fatalf("items = %v", items)
	}
	if p := items[0].(map[string]any)["path"]; p != "/file.txt" {
		t.Errorf("visible path = %v, want /file.txt", p)
	}

	// Escapes: traversal, absolute outside root, encoded traversal.
	for _, q := range []string{
		"/v1/list?path=/../",
		"/v1/list?path=/../../secret.txt",
		"/v1/list?path=%2e%2e",
		"/v1/stat?path=/../../secret.txt",
		"/v1/files?path=/../../secret.txt",
		"/v1/stat?path=/..%2f..%2fsecret.txt",
	} {
		rec, doc := h.do("GET", q, chrootPlain, nil, nil)
		if rec.Code != 400 || errCode(doc) != "path_invalid" {
			t.Errorf("%s = %d %v, want 400 path_invalid", q, rec.Code, errCode(doc))
		}
	}
	// The trash directory is blocked from direct file ops.
	rec, doc = h.do("GET", "/v1/stat?path=/.c2a-trash/x", chrootPlain, nil, nil)
	if rec.Code != 400 || errCode(doc) != "path_invalid" {
		t.Errorf("trash stat = %d %v", rec.Code, doc)
	}
	// Unknown drive.
	rec, doc = h.do("GET", "/v1/drives/ghost/list?path=/", chrootPlain, nil, nil)
	if rec.Code != 404 || errCode(doc) != "drive_not_found" {
		t.Errorf("ghost drive = %d %v", rec.Code, doc)
	}
	// Drive not granted (d2 exists but not granted to the chroot key).
	rec, doc = h.do("GET", "/v1/drives/d2/list?path=/", chrootPlain, nil, nil)
	if rec.Code != 403 || errCode(doc) != "forbidden" {
		t.Errorf("ungranted drive = %d %v", rec.Code, doc)
	}
}

func TestUploadDownloadChecksums(t *testing.T) {
	h := newHarness(t)
	content := []byte("123456789upload-payload")
	crc := crc64.New(crc64Table)
	crc.Write(content)
	wantCRC := fmt.Sprintf("%d", crc.Sum64())
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(content))

	hdr := map[string]string{"Content-Type": "application/octet-stream"}
	rec, doc := h.do("PUT", "/v1/files?path=/up/test.bin", chrootPlain, content, hdr)
	if rec.Code != 200 {
		t.Fatalf("upload = %d %v", rec.Code, doc)
	}
	if doc["crc64"] != wantCRC || doc["sha256"] != wantSHA || doc["size"] != float64(len(content)) {
		t.Fatalf("checksums = %v", doc)
	}
	if doc["path"] != "/up/test.bin" || doc["conflict"] != "created" {
		t.Fatalf("upload response = %v", doc)
	}

	// The upload record drives stat.
	rec, doc = h.do("GET", "/v1/stat?path=/up/test.bin", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("stat = %d %v", rec.Code, doc)
	}
	hashes := doc["hashes"].(map[string]any)
	if hashes["crc64"] != wantCRC || hashes["sha256"] != wantSHA || doc["hashes_source"] != "upload_record" {
		t.Fatalf("stat hashes = %v %v", hashes, doc["hashes_source"])
	}

	// Download verifies content and carries checksum headers.
	rec, _ = h.do("GET", "/v1/files?path=/up/test.bin", chrootPlain, nil, nil)
	if rec.Code != 200 || rec.Body.String() != string(content) {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-C2A-SHA256") != wantSHA || rec.Header().Get("X-C2A-CRC64") != wantCRC {
		t.Fatalf("download headers = %v", rec.Header())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "test.bin") {
		t.Errorf("disposition = %q", cd)
	}

	// Raw upload without Content-Length is rejected.
	req := httptest.NewRequest("PUT", "/v1/files?path=/up/nolen.bin", nil)
	req.Header.Set("apikey", chrootPlain)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 411 {
		t.Fatalf("chunked upload = %d %s, want 411", rec2.Code, rec2.Body.String())
	}
}

func TestUploadOnConflict(t *testing.T) {
	h := newHarness(t)
	seed := []byte("v1")
	h.do("PUT", "/v1/files?path=/f.txt", chrootPlain, seed, map[string]string{"Content-Type": "application/octet-stream"})

	// fail (default)
	rec, doc := h.do("PUT", "/v1/files?path=/f.txt", chrootPlain, []byte("v2"), map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != 409 || errCode(doc) != "already_exists" {
		t.Fatalf("fail = %d %v", rec.Code, doc)
	}
	// overwrite
	rec, doc = h.do("PUT", "/v1/files?path=/f.txt&on_conflict=overwrite", chrootPlain, []byte("v2"), map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != 200 || doc["conflict"] != "overwritten" {
		t.Fatalf("overwrite = %d %v", rec.Code, doc)
	}
	// rename -> "f (1).txt"
	rec, doc = h.do("PUT", "/v1/files?path=/f.txt&on_conflict=rename", chrootPlain, []byte("v3"), map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != 200 || doc["path"] != "/f (1).txt" || doc["conflict"] != "renamed" {
		t.Fatalf("rename = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	_, has1 := h.fake.entries["/m1/agents/x/f (1).txt"]
	h.fake.mu.Unlock()
	if !has1 {
		t.Error("renamed upload missing upstream")
	}
}

func TestUploadExpectedChecksumMismatch(t *testing.T) {
	h := newHarness(t)
	hdr := map[string]string{
		"Content-Type":     "application/octet-stream",
		"X-Expected-CRC64": "999",
	}
	rec, doc := h.do("PUT", "/v1/files?path=/bad.bin", chrootPlain, []byte("data"), hdr)
	if rec.Code != 422 || errCode(doc) != "checksum_mismatch" {
		t.Fatalf("mismatch = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	_, still := h.fake.entries["/m1/agents/x/bad.bin"]
	h.fake.mu.Unlock()
	if still {
		t.Error("mismatched upload was not deleted")
	}
}

func TestMkdirAndIdempotentDelete(t *testing.T) {
	h := newHarness(t)
	rec, doc := h.do("POST", "/v1/mkdir", chrootPlain, []byte(`{"path":"/a/b/c"}`), jsonCT())
	if rec.Code != 200 || doc["created"] != true {
		t.Fatalf("mkdir = %d %v", rec.Code, doc)
	}
	// idempotent
	rec, doc = h.do("POST", "/v1/mkdir", chrootPlain, []byte(`{"path":"/a/b/c"}`), jsonCT())
	if rec.Code != 200 || doc["created"] != false {
		t.Fatalf("mkdir twice = %d %v", rec.Code, doc)
	}

	h.do("PUT", "/v1/files?path=/a/b/c/x.txt", chrootPlain, []byte("x"), map[string]string{"Content-Type": "application/octet-stream"})
	// delete missing -> already_absent (idempotent delete, §4.10.2)
	rec, doc = h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/nope.txt"]}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("delete missing = %d %v", rec.Code, doc)
	}
	results := doc["results"].([]any)
	if results[0].(map[string]any)["status"] != "already_absent" {
		t.Fatalf("already_absent = %v", results)
	}
}

func jsonCT() map[string]string { return map[string]string{"Content-Type": "application/json"} }

func TestIdempotencyReplay(t *testing.T) {
	h := newHarness(t)
	idem := map[string]string{"Content-Type": "application/octet-stream", "Idempotency-Key": "op-1"}
	rec, doc := h.do("PUT", "/v1/files?path=/idem.bin", chrootPlain, []byte("once"), idem)
	if rec.Code != 200 || rec.Header().Get("X-Idempotent-Replay") != "" {
		t.Fatalf("first = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	puts := len(h.fake.puts)
	h.fake.mu.Unlock()

	// Same key replays the stored response without a second upstream write.
	rec, doc = h.do("PUT", "/v1/files?path=/idem.bin", chrootPlain, []byte("twice"), idem)
	if rec.Code != 200 || rec.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay = %d %v replay-hdr=%q", rec.Code, doc, rec.Header().Get("X-Idempotent-Replay"))
	}
	h.fake.mu.Lock()
	puts2 := len(h.fake.puts)
	h.fake.mu.Unlock()
	if puts2 != puts {
		t.Errorf("replay reached upstream (%d -> %d puts)", puts, puts2)
	}
	if doc["sha256"] == nil || doc["path"] != "/idem.bin" {
		t.Fatalf("replay body = %v", doc)
	}

	// Same idem key on a different route is a conflict.
	rec, doc = h.do("POST", "/v1/mkdir", chrootPlain, []byte(`{"path":"/x"}`), map[string]string{"Content-Type": "application/json", "Idempotency-Key": "op-1"})
	if rec.Code != 409 || errCode(doc) != "idempotency_mismatch" {
		t.Fatalf("route mismatch = %d %v", rec.Code, doc)
	}
}

func TestDeleteTrashRestorePurge(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/v1/files?path=/docs/report.txt", chrootPlain, []byte("report"), map[string]string{"Content-Type": "application/octet-stream"})

	// delete -> trash
	rec, doc := h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/docs/report.txt"]}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("delete = %d %v", rec.Code, doc)
	}
	res := doc["results"].([]any)[0].(map[string]any)
	if res["status"] != "trashed" {
		t.Fatalf("result = %v", res)
	}
	trashID := int64(res["trash_id"].(float64))
	date := time.Now().UTC().Format("2006-01-02")
	wantTrash := "/agents/x/.c2a-trash/" + date + "/docs/report.txt"
	h.fake.mu.Lock()
	_, moved := h.fake.entries["/m1"+wantTrash]
	_, origGone := h.fake.entries["/m1/agents/x/docs/report.txt"]
	h.fake.mu.Unlock()
	if !moved || origGone {
		t.Fatalf("trash move failed: moved=%v origGone=%v", moved, origGone)
	}

	// trash hidden from list
	_, doc = h.do("GET", "/v1/list?path=/", chrootPlain, nil, nil)
	for _, it := range doc["items"].([]any) {
		if it.(map[string]any)["name"] == ".c2a-trash" {
			t.Error(".c2a-trash visible in listing")
		}
	}

	// trash list shows it
	_, doc = h.do("GET", "/v1/trash", chrootPlain, nil, nil)
	items := doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["path"] != "/docs/report.txt" {
		t.Fatalf("trash = %v", items)
	}

	// restore
	rec, doc = h.do("POST", "/v1/trash/restore", chrootPlain, []byte(fmt.Sprintf(`{"id":%d}`, trashID)), jsonCT())
	if rec.Code != 200 || doc["restored"] != true {
		t.Fatalf("restore = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	_, back := h.fake.entries["/m1/agents/x/docs/report.txt"]
	h.fake.mu.Unlock()
	if !back {
		t.Error("restore did not move the file back")
	}

	// trash disabled -> delete is permanent (legacy key has trash on; use patch via store)
	// (covered by keymgr-level behavior; here check purge instead)
	h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/docs/report.txt"]}`), jsonCT())
	// purge needs the purge permission: chroot key lacks it.
	rec2, doc2 := h.do("POST", "/v1/trash/purge", chrootPlain, []byte(`{"id":2}`), jsonCT())
	if rec2.Code != 403 || errCode(doc2) != "forbidden" {
		t.Fatalf("purge without permission = %d %v", rec2.Code, doc2)
	}
}

func TestDeleteTrashSuffixCollision(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/v1/files?path=/dup/a.txt", chrootPlain, []byte("1"), map[string]string{"Content-Type": "application/octet-stream"})
	h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/dup/a.txt"]}`), jsonCT())
	h.do("POST", "/v1/trash/restore", chrootPlain, []byte(`{"id":1}`), jsonCT())
	// Trash already holds a.txt from the previous cycle; deleting the
	// restored file again must pick a suffixed trash name without
	// colliding.
	h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/dup/a.txt"]}`), jsonCT())
	h.do("PUT", "/v1/files?path=/dup/a.txt", chrootPlain, []byte("2"), map[string]string{"Content-Type": "application/octet-stream"})
	rec, doc := h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/dup/a.txt"]}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("second-cycle delete = %d %v", rec.Code, doc)
	}
	res := doc["results"].([]any)[0].(map[string]any)
	if res["status"] != "trashed" {
		t.Fatalf("result = %v", res)
	}
}

func TestMove(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/v1/files?path=/mv/a.txt", chrootPlain, []byte("a"), map[string]string{"Content-Type": "application/octet-stream"})
	h.do("POST", "/v1/mkdir", chrootPlain, []byte(`{"path":"/dst"}`), jsonCT())

	rec, doc := h.do("POST", "/v1/move", chrootPlain, []byte(`{"from":"/mv/a.txt","to":"/dst/a.txt"}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("move = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	_, moved := h.fake.entries["/m1/agents/x/dst/a.txt"]
	h.fake.mu.Unlock()
	if !moved {
		t.Error("move did not relocate the file")
	}

	// rename in place
	rec, doc = h.do("POST", "/v1/move", chrootPlain, []byte(`{"from":"/dst/a.txt","to":"/dst/b.txt"}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("rename = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	_, renamed := h.fake.entries["/m1/agents/x/dst/b.txt"]
	h.fake.mu.Unlock()
	if !renamed {
		t.Error("rename failed")
	}

	// on_conflict fail
	h.do("POST", "/v1/move", chrootPlain, []byte(`{"from":"/dst/b.txt","to":"/dst/b.txt"}`), jsonCT()) // identical is invalid
	h.do("PUT", "/v1/files?path=/dst/c.txt", chrootPlain, []byte("c"), map[string]string{"Content-Type": "application/octet-stream"})
	h.do("PUT", "/v1/files?path=/dst/d.txt", chrootPlain, []byte("d"), map[string]string{"Content-Type": "application/octet-stream"})
	rec, doc = h.do("POST", "/v1/move", chrootPlain, []byte(`{"from":"/dst/c.txt","to":"/dst/d.txt"}`), jsonCT())
	if rec.Code != 409 || errCode(doc) != "already_exists" {
		t.Fatalf("conflict move = %d %v", rec.Code, doc)
	}
}

func TestLinks(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/v1/files?path=/share/file.txt", chrootPlain, []byte("shared"), map[string]string{"Content-Type": "application/octet-stream"})

	rec, doc := h.do("POST", "/v1/links", chrootPlain, []byte(`{"path":"/share/file.txt","expires_in":3600}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("link = %d %v", rec.Code, doc)
	}
	linkURL, _ := doc["url"].(string)
	if linkURL == "" || !strings.Contains(linkURL, "/l/") {
		t.Fatalf("url = %v", linkURL)
	}

	// Download without a key.
	req := httptest.NewRequest("GET", strings.TrimPrefix(linkURL, "http://example.com"), nil)
	rec2 := httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 200 || rec2.Body.String() != "shared" {
		t.Fatalf("link download = %d %q", rec2.Code, rec2.Body.String())
	}

	// Expired link -> 410.
	rec, doc = h.do("POST", "/v1/links", chrootPlain, []byte(`{"path":"/share/file.txt","expires_in":1}`), jsonCT())
	expURL, _ := doc["url"].(string)
	time.Sleep(1100 * time.Millisecond)
	req = httptest.NewRequest("GET", strings.TrimPrefix(expURL, "http://example.com"), nil)
	rec2 = httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 410 || errCode(mustJSON(rec2.Body.String())) != "link_expired" {
		t.Fatalf("expired link = %d %v", rec2.Code, rec2.Body.String())
	}

	// >7d is rejected.
	rec, doc = h.do("POST", "/v1/links", chrootPlain, []byte(`{"path":"/share/file.txt","expires_in":604801}`), jsonCT())
	if rec.Code != 400 || errCode(doc) != "invalid_request" {
		t.Fatalf("7d cap = %d %v", rec.Code, doc)
	}

	// Revoking the key kills its links.
	rec, doc = h.do("POST", "/v1/links", chrootPlain, []byte(`{"path":"/share/file.txt","expires_in":3600}`), jsonCT())
	liveURL, _ := doc["url"].(string)
	if err := h.keys.Revoke("chroot"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", strings.TrimPrefix(liveURL, "http://example.com"), nil)
	rec2 = httptest.NewRecorder()
	h.handler.ServeHTTP(rec2, req)
	if rec2.Code != 410 {
		t.Fatalf("revoked-key link = %d, want 410", rec2.Code)
	}
}

func mustJSON(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t)
	// admin key has requests_per_minute=2; third request within the window
	// answers 429 with Retry-After.
	var last *httptest.ResponseRecorder
	var lastDoc map[string]any
	for i := 0; i < 3; i++ {
		last, lastDoc = h.do("GET", "/v1/me", adminPlain, nil, nil)
	}
	if last.Code != 429 || errCode(lastDoc) != "rate_limited" {
		t.Fatalf("rate limit = %d %v", last.Code, lastDoc)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
	// unlimited keys are unaffected
	rec, _ := h.do("GET", "/v1/me", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("unlimited key throttled: %d", rec.Code)
	}
}

func TestDefaultDriveAndLegacyKey(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.entries["/m1/hello.txt"] = &fakeEntry{name: "hello.txt", content: []byte("hi"), modified: "2026-10-12T00:00:00Z"}
	h.fake.mu.Unlock()

	// Legacy-format static key authenticates and uses the default alias
	// (first granted drive).
	rec, doc := h.do("GET", "/v1/list?path=/", legacyPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("legacy list = %d %v", rec.Code, doc)
	}
	items := doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "hello.txt" {
		t.Fatalf("legacy items = %v", items)
	}
}

func TestAuditWritten(t *testing.T) {
	h := newHarness(t)
	h.do("GET", "/v1/me", chrootPlain, nil, nil)
	h.do("GET", "/v1/list?path=/", chrootPlain, nil, nil)
	entries, _, err := h.st.QueryAudit(store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("audit entries = %d", len(entries))
	}
	sawList := false
	for _, e := range entries {
		if e.KeyID != "chroot" {
			t.Errorf("audit key_id = %q (credential leak?)", e.KeyID)
		}
		if e.Op == "list" {
			sawList = true
			if e.Status != 200 || e.Drive != "d1" {
				t.Errorf("list entry = %+v", e)
			}
		}
	}
	if !sawList {
		t.Error("no list entry in audit log")
	}
}

func TestHealthEndpoints(t *testing.T) {
	h := newHarness(t)
	rec, _ := h.do("GET", "/healthz", "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
	rec, _ = h.do("GET", "/readyz", "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("readyz = %d %s", rec.Code, rec.Body.String())
	}
}

// ---- Phase 2: needs_reauth preflight (UPGRADE §3.5) ----

// TestNeedsReauthDriveReturnsUnavailableWithHint proves the /v1 behavior of a
// drive whose OpenList storage reports a token-class failure: 503
// drive_unavailable with the OpenList-credentials hint, instead of leaking
// the upstream error.
func TestNeedsReauthDriveReturnsUnavailableWithHint(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.status1 = "failed to refresh token: invalid refresh token"
	h.fake.mu.Unlock()

	rec, body := h.do(http.MethodGet, "/v1/drives/d1/list?path=/", chrootPlain, nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body %v", rec.Code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != CodeDriveUnavailable {
		t.Fatalf("code = %v", errObj["code"])
	}
	if hint, _ := errObj["hint"].(string); !strings.Contains(hint, "administrator") || !strings.Contains(hint, "OpenList") {
		t.Fatalf("hint = %v", errObj["hint"])
	}

	// /v1/me surfaces the drive as needs_reauth with a next_step.
	_, me := h.do(http.MethodGet, "/v1/me", chrootPlain, nil, nil)
	drives, _ := me["drives"].([]any)
	if len(drives) == 0 {
		t.Fatal("no drives in /v1/me")
	}
	d1 := drives[0].(map[string]any)
	if d1["health"] != "needs_reauth" {
		t.Fatalf("health = %v", d1["health"])
	}
	steps, _ := me["next_steps"].([]any)
	found := false
	for _, s := range steps {
		if str, ok := s.(string); ok && strings.Contains(str, "stored credentials") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no reauth next_step: %v", steps)
	}

	// A non-token error status is NOT converted to a reauth 503: the request
	// proceeds and surfaces the upstream error as usual.
	h.fake.mu.Lock()
	h.fake.status1 = "some other failure"
	h.fake.mu.Unlock()
	h.api.healthCache.invalidate()
	rec, _ = h.do(http.MethodGet, "/v1/drives/d1/list?path=/", chrootPlain, nil, nil)
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("non-token failure answered as drive_unavailable")
	}
}
