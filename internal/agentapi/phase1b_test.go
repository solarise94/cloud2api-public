package agentapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud2api/internal/openlist"
	"cloud2api/internal/store"
)

// grantKey gives the chroot key an extra drive grant (d2 root /agents/x2).
func grantSecondDrive(t *testing.T, h *harness) {
	t.Helper()
	k, err := h.st.GetKeyWithGrants("chroot")
	if err != nil {
		t.Fatal(err)
	}
	k.Drives = append(k.Drives, store.KeyDrive{
		Drive: "d2", Root: "/agents/x2",
		Perms: []string{"list", "read", "write", "delete", "transfer", "link", "fetch", "purge"},
	})
	k.GlobalPerms = []string{"link", "transfer", "fetch", "delegate"}
	if err := h.st.UpdateKey(k); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	h.fake.entries["/m2/agents/x2"] = &fakeEntry{name: "x2", isDir: true, modified: "2026-10-12T00:00:00Z"}
	h.fake.mu.Unlock()
}

// put uploads via the /v1 API and seeds a file at visible path on the key's
// default drive.
func put(t *testing.T, h *harness, visible string, content string) {
	t.Helper()
	rec, doc := h.do("PUT", "/v1/files?path="+visible, chrootPlain, []byte(content),
		map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != 200 {
		t.Fatalf("put %s = %d %v", visible, rec.Code, doc)
	}
}

// ---- subset rules (§4.10.8) ----

func TestChildKeySubsetRules(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)

	create := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		return h.do("POST", "/v1/keys", chrootPlain, []byte(body), jsonCT())
	}

	// Without delegate: forbidden (legacy key has no delegate).
	rec, doc := h.do("POST", "/v1/keys", legacyPlain, []byte(`{"drives":[{"drive":"d1","root":"/","perms":["read"]}]}`), jsonCT())
	if rec.Code != 403 || errCode(doc) != "forbidden" {
		t.Fatalf("no delegate = %d %v", rec.Code, doc)
	}

	// Subset grant works.
	rec, doc = create(`{"id":"child-ok","drives":[{"drive":"d1","root":"/agents/x/sub","perms":["list","read"]}]}`)
	if rec.Code != 200 {
		t.Fatalf("subset create = %d %v", rec.Code, doc)
	}
	plaintext, _ := doc["plaintext"].(string)
	if !strings.HasPrefix(plaintext, "sk-c2a-") {
		t.Fatalf("plaintext = %q", plaintext)
	}
	if doc["key"].(map[string]any)["parent_id"] != "chroot" {
		t.Fatalf("parent = %v", doc["key"])
	}
	// The child key authenticates and sees only its sub-root.
	rec, _ = h.do("GET", "/v1/me", plaintext, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("child auth = %d", rec.Code)
	}
	// Child cannot escape its root.
	rec, doc = h.do("GET", "/v1/list?path=/../", plaintext, nil, nil)
	if rec.Code != 400 {
		t.Fatalf("child escape = %d %v", rec.Code, doc)
	}

	// Superset perms -> 403.
	rec, doc = create(`{"drives":[{"drive":"d1","root":"/agents/x","perms":["purge"]}]}`)
	if rec.Code != 403 || !strings.Contains(fmt.Sprint(doc), "exceeds the parent") {
		t.Fatalf("superset perms = %d %v", rec.Code, doc)
	}

	// Root outside the parent root -> 403.
	rec, doc = create(`{"drives":[{"drive":"d1","root":"/elsewhere","perms":["read"]}]}`)
	if rec.Code != 403 {
		t.Fatalf("outside root = %d %v", rec.Code, doc)
	}

	// Global perm not held by parent -> 403.
	rec, doc = create(`{"drives":[{"drive":"d1","root":"/agents/x","perms":["read"]}],"global_perms":["purge"]}`)
	if rec.Code != 403 {
		t.Fatalf("global superset = %d %v", rec.Code, doc)
	}

	// TTL cap.
	rec, doc = create(`{"drives":[{"drive":"d1","root":"/agents/x","perms":["read"]}],"ttl_seconds":604801}`)
	if rec.Code != 400 {
		t.Fatalf("ttl cap = %d %v", rec.Code, doc)
	}

	// Listing + revocation.
	rec, doc = h.do("GET", "/v1/keys", chrootPlain, nil, nil)
	if rec.Code != 200 || len(doc["keys"].([]any)) != 1 {
		t.Fatalf("keys list = %d %v", rec.Code, doc)
	}
	rec, _ = h.do("DELETE", "/v1/keys/child-ok", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("child revoke = %d", rec.Code)
	}
	rec, _ = h.do("GET", "/v1/me", plaintext, nil, nil)
	if rec.Code != 401 {
		t.Fatalf("revoked child still works: %d", rec.Code)
	}

	// Parent revocation cascades to descendants.
	rec, doc = create(`{"id":"child-a","drives":[{"drive":"d1","root":"/agents/x","perms":["read"]}]}`)
	if rec.Code != 200 {
		t.Fatalf("child-a = %d %v", rec.Code, doc)
	}
	childA := doc["plaintext"].(string)
	rec, doc = h.do("POST", "/v1/keys", childA, []byte(`{"id":"grandchild","drives":[{"drive":"d1","root":"/agents/x/deep","perms":["read"]}]}`), jsonCT())
	if rec.Code != 403 {
		t.Fatalf("grandchild without delegate = %d %v", rec.Code, doc)
	}
	// Revoking the parent kills the child subtree (keymgr cascade).
	if err := h.keys.Revoke("chroot"); err != nil {
		t.Fatal(err)
	}
	rec, _ = h.do("GET", "/v1/me", childA, nil, nil)
	if rec.Code != 401 {
		t.Fatalf("child survived parent revocation: %d", rec.Code)
	}
}

// ---- suggestions ----

func TestSuggestions(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/Q3-report.txt", "x")
	put(t, h, "/q3-final.pdf", "x")
	put(t, h, "/unrelated.bin", "x")

	rec, doc := h.do("GET", "/v1/stat?path=/q3-report.txt", chrootPlain, nil, nil)
	if rec.Code != 404 || errCode(doc) != "path_not_found" {
		t.Fatalf("stat = %d %v", rec.Code, doc)
	}
	e := doc["error"].(map[string]any)
	sugg, _ := e["suggestions"].([]any)
	if len(sugg) == 0 {
		t.Fatalf("no suggestions: %v", e)
	}
	found := map[string]bool{}
	for _, s := range sugg {
		found[strings.ToLower(fmt.Sprint(s))] = true
	}
	if !found["/q3-report.txt"] {
		t.Fatalf("suggestions missing case-differs match: %v", sugg)
	}
	if levenshtein("kitten", "sitting") != 3 {
		t.Fatalf("levenshtein sanity failed")
	}
}

// ---- dry runs ----

func TestDryRunDeleteAndMove(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/bulk/a.txt", "aaa")
	put(t, h, "/bulk/b.txt", "bbb")

	// delete dry_run
	rec, doc := h.do("POST", "/v1/delete", chrootPlain, []byte(`{"paths":["/bulk"],"dry_run":true}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("dry delete = %d %v", rec.Code, doc)
	}
	wa := doc["would_affect"].(map[string]any)
	if wa["files"] != float64(2) || wa["dirs"] != float64(1) || wa["bytes"] != float64(6) {
		t.Fatalf("would_affect = %v", wa)
	}
	// Nothing was deleted.
	rec, _ = h.do("GET", "/v1/stat?path=/bulk/a.txt", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("dry-run deleted something: %d", rec.Code)
	}

	// move dry_run
	rec, doc = h.do("POST", "/v1/move", chrootPlain, []byte(`{"from":"/bulk","to":"/moved","dry_run":true}`), jsonCT())
	if rec.Code != 200 || doc["dry_run"] != true {
		t.Fatalf("dry move = %d %v", rec.Code, doc)
	}
	rec, _ = h.do("GET", "/v1/stat?path=/bulk/a.txt", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("dry-run move deleted source: %d", rec.Code)
	}
}

// ---- transfers as jobs + long-poll wake + restart resume ----

// waitJobTerminal polls a job like a real agent (repeated ?wait=N long-polls)
// until it reaches a terminal state or the timeout expires.
func waitJobTerminal(t *testing.T, h *harness, key, id string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rec, doc := h.do("GET", "/v1/jobs/"+id+"?wait=5", key, nil, nil)
		if rec.Code != 200 {
			t.Fatalf("job get %s = %d %v", id, rec.Code, doc)
		}
		j := doc["job"].(map[string]any)
		switch j["state"] {
		case "succeeded", "failed", "canceled":
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish in %s: %v", id, timeout, j)
		}
	}
}

func TestTransferJobLifecycle(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)
	put(t, h, "/xfer.bin", "transfer-payload")

	// Dry run first.
	rec, doc := h.do("POST", "/v1/transfers", chrootPlain,
		[]byte(`{"from":{"drive":"d1","path":"/xfer.bin"},"to":{"drive":"d2","path":"/in/xfer.bin"},"dry_run":true}`), jsonCT())
	if rec.Code != 200 || doc["dry_run"] != true {
		t.Fatalf("transfer dry = %d %v", rec.Code, doc)
	}

	// Real transfer answers 202 with a job.
	rec, doc = h.do("POST", "/v1/transfers", chrootPlain,
		[]byte(`{"from":{"drive":"d1","path":"/xfer.bin"},"to":{"drive":"d2","path":"/in/xfer.bin"}}`), jsonCT())
	if rec.Code != 202 {
		t.Fatalf("transfer create = %d %v", rec.Code, doc)
	}
	job := doc["job"].(map[string]any)
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatalf("no job id: %v", job)
	}

	// Long-poll like a real agent until the job succeeds.
	start := time.Now()
	j := waitJobTerminal(t, h, chrootPlain, id, 30*time.Second)
	if j["state"] != "succeeded" {
		t.Fatalf("job state = %v (err=%v)", j["state"], j["error"])
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("long-poll took %s", elapsed)
	}
	// Content landed on d2.
	h.fake.mu.Lock()
	e, ok := h.fake.entries["/m2/agents/x2/in/xfer.bin"]
	h.fake.mu.Unlock()
	if !ok || string(e.content) != "transfer-payload" {
		t.Fatalf("d2 copy missing: %v", e)
	}

	// Jobs listing filters by state.
	rec, doc = h.do("GET", "/v1/jobs?state=succeeded", chrootPlain, nil, nil)
	if rec.Code != 200 || len(doc["jobs"].([]any)) != 1 {
		t.Fatalf("jobs list = %d %v", rec.Code, doc)
	}

	// Unknown / foreign job ids are 404s.
	rec, _ = h.do("GET", "/v1/jobs/job-nope", chrootPlain, nil, nil)
	if rec.Code != 404 {
		t.Fatalf("missing job = %d", rec.Code)
	}

	// Cancelling a terminal job is invalid.
	rec, _ = h.do("POST", "/v1/jobs/"+id+"/cancel", chrootPlain, nil, jsonCT())
	if rec.Code != 400 {
		t.Fatalf("cancel terminal = %d", rec.Code)
	}
}

func TestTransferJobCancellation(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)
	put(t, h, "/slow.bin", "slow")
	h.fake.mu.Lock()
	h.fake.taskDelay = 30 * time.Second // stays running
	h.fake.mu.Unlock()

	rec, doc := h.do("POST", "/v1/transfers", chrootPlain,
		[]byte(`{"from":{"drive":"d1","path":"/slow.bin"},"to":{"drive":"d2","path":"/slow-copy.bin"}}`), jsonCT())
	if rec.Code != 202 {
		t.Fatalf("create = %d %v", rec.Code, doc)
	}
	id := doc["job"].(map[string]any)["id"].(string)

	// Wait until the job is running with a task id recorded.
	var sawRunning bool
	for i := 0; i < 100; i++ {
		_, doc = h.do("GET", "/v1/jobs/"+id, chrootPlain, nil, nil)
		st := doc["job"].(map[string]any)["state"]
		if st == "running" {
			sawRunning = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawRunning {
		t.Fatal("job never reached running")
	}

	rec, doc = h.do("POST", "/v1/jobs/"+id+"/cancel", chrootPlain, nil, jsonCT())
	if rec.Code != 200 {
		t.Fatalf("cancel = %d %v", rec.Code, doc)
	}
	time.Sleep(300 * time.Millisecond) // let the runner observe the cancel
	_, doc = h.do("GET", "/v1/jobs/"+id, chrootPlain, nil, nil)
	if state := doc["job"].(map[string]any)["state"]; state != "canceled" {
		t.Fatalf("state after cancel = %v", doc["job"])
	}
}

// ---- fetch (offline download) ----

func TestFetchJob(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)

	rec, doc := h.do("POST", "/v1/fetch", chrootPlain,
		[]byte(`{"url":"http://127.0.0.1:8080/l/token/file.bin","drive":"d2","path":"/dl/file.bin"}`), jsonCT())
	if rec.Code != 202 {
		t.Fatalf("fetch create = %d %v", rec.Code, doc)
	}
	id := doc["job"].(map[string]any)["id"].(string)
	if j := waitJobTerminal(t, h, chrootPlain, id, 30*time.Second); j["state"] != "succeeded" {
		t.Fatalf("fetch job = %v", j)
	}
	// The URL's basename was renamed to the wanted target name.
	h.fake.mu.Lock()
	e, ok := h.fake.entries["/m2/agents/x2/dl/file.bin"]
	h.fake.mu.Unlock()
	if !ok || len(e.content) == 0 {
		t.Fatalf("fetched file missing: %v", e)
	}
}

// ---- batch ----

func TestBatchSyncAndJob(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)
	put(t, h, "/batch-src.txt", "batch")

	// Synchronous batch (mkdir + move within one drive).
	rec, doc := h.do("POST", "/v1/batch", chrootPlain, []byte(`{
		"ops":[
			{"op":"mkdir","path":"/b1"},
			{"op":"move","from":"/batch-src.txt","to":"/b1/batch-src.txt"}
		]}`), jsonCT())
	if rec.Code != 200 {
		t.Fatalf("sync batch = %d %v", rec.Code, doc)
	}
	results := doc["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", results)
	}
	if results[0].(map[string]any)["status"] != "ok" || results[1].(map[string]any)["status"] != "ok" {
		t.Fatalf("results = %v", results)
	}
	rec, _ = h.do("GET", "/v1/stat?path=/b1/batch-src.txt", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("batch move did not land: %d", rec.Code)
	}

	// Cross-drive copy turns the batch into a 202 job.
	rec, doc = h.do("POST", "/v1/batch", chrootPlain, []byte(`{
		"ops":[{"op":"copy","from":{"drive":"d1","path":"/b1/batch-src.txt"},"to":{"drive":"d2","path":"/copy/batch-src.txt"}}]}`), jsonCT())
	if rec.Code != 202 {
		t.Fatalf("cross-drive batch = %d %v", rec.Code, doc)
	}
	id := doc["job"].(map[string]any)["id"].(string)
	if j := waitJobTerminal(t, h, chrootPlain, id, 30*time.Second); j["state"] != "succeeded" {
		t.Fatalf("batch job = %v", j)
	}
	h.fake.mu.Lock()
	_, ok := h.fake.entries["/m2/agents/x2/copy/batch-src.txt"]
	h.fake.mu.Unlock()
	if !ok {
		t.Fatal("batch copy missing on d2")
	}

	// Batch dry run reports without doing.
	rec, doc = h.do("POST", "/v1/batch", chrootPlain, []byte(`{
		"ops":[{"op":"delete","paths":["/b1"]}],
		"dry_run":true}`), jsonCT())
	if rec.Code != 200 || doc["dry_run"] != true {
		t.Fatalf("batch dry = %d %v", rec.Code, doc)
	}
	rec, _ = h.do("GET", "/v1/stat?path=/b1/batch-src.txt", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatal("batch dry-run deleted a file")
	}

	// >100 ops rejected.
	var bigOps strings.Builder
	bigOps.WriteString(`{"ops":[`)
	for i := 0; i < 101; i++ {
		if i > 0 {
			bigOps.WriteString(",")
		}
		bigOps.WriteString(fmt.Sprintf(`{"op":"mkdir","path":"/z%d"}`, i))
	}
	bigOps.WriteString(`]}`)
	rec, _ = h.do("POST", "/v1/batch", chrootPlain, []byte(bigOps.String()), jsonCT())
	if rec.Code != 400 {
		t.Fatalf("101 ops = %d", rec.Code)
	}
}

// ---- selftest ----

func TestSelftest(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)
	rec, doc := h.do("POST", "/v1/selftest", chrootPlain, nil, jsonCT())
	if rec.Code != 200 {
		t.Fatalf("selftest = %d %v", rec.Code, doc)
	}
	if doc["all_ok"] != true {
		t.Fatalf("selftest = %v", doc)
	}
	drives := doc["drives"].([]any)
	if len(drives) != 2 {
		t.Fatalf("drives = %v", drives)
	}
	for _, d := range drives {
		dm := d.(map[string]any)
		if dm["ok"] != true {
			t.Fatalf("drive %v: %v", dm["drive"], dm["error"])
		}
	}
	// No residue: the selftest file is gone (deleted permanently).
	h.fake.mu.Lock()
	for p := range h.fake.entries {
		if strings.Contains(p, "selftest-") {
			t.Fatalf("selftest residue at %s", p)
		}
	}
	h.fake.mu.Unlock()
	// last_selftest_at is visible in /v1/me.
	rec, doc = h.do("GET", "/v1/me", chrootPlain, nil, nil)
	if rec.Code != 200 || doc["key"].(map[string]any)["last_selftest_at"] == nil {
		t.Fatalf("me last_selftest = %v", doc["key"])
	}
}

// ---- tree / search / activity ----

func TestTreeSearchActivity(t *testing.T) {
	h := newHarness(t)
	put(t, h, "/docs/2026/report-q3.txt", "r")
	put(t, h, "/docs/2026/notes.txt", "n")

	rec, doc := h.do("GET", "/v1/tree?path=/&depth=3", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("tree = %d %v", rec.Code, doc)
	}
	items := doc["items"].([]any)
	if len(items) < 3 {
		t.Fatalf("tree items = %v", items)
	}

	// Search uses the bounded walk (the fake answers "search not available").
	rec, doc = h.do("GET", "/v1/search?q=report&path=/", chrootPlain, nil, nil)
	if rec.Code != 200 || doc["engine"] != "walk" {
		t.Fatalf("search = %d %v", rec.Code, doc)
	}
	found := false
	for _, it := range doc["items"].([]any) {
		if strings.HasSuffix(fmt.Sprint(it.(map[string]any)["path"]), "report-q3.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("search items = %v", doc["items"])
	}

	rec, doc = h.do("GET", "/v1/activity?limit=10", chrootPlain, nil, nil)
	if rec.Code != 200 || len(doc["entries"].([]any)) == 0 {
		t.Fatalf("activity = %d %v", rec.Code, doc)
	}
	for _, e := range doc["entries"].([]any) {
		em := e.(map[string]any)
		if em["key_id"] != nil {
			t.Fatalf("activity leaks key_id: %v", em)
		}
		if p := fmt.Sprint(em["path"]); strings.Contains(p, "/m1") || strings.Contains(p, "agents/x") {
			t.Fatalf("activity leaks internal path: %v", em)
		}
	}
}

// ---- /v1 index ----

func TestIndexLinks(t *testing.T) {
	h := newHarness(t)
	rec, doc := h.do("GET", "/v1", chrootPlain, nil, nil)
	if rec.Code != 200 {
		t.Fatalf("index = %d", rec.Code)
	}
	if _, ok := doc["endpoints"].([]any); !ok {
		t.Fatalf("no endpoints: %v", doc)
	}
	sawJobs, sawTransfers := false, false
	sawSelftest := false
	for _, e := range doc["endpoints"].([]any) {
		em := e.(map[string]any)
		switch em["path"] {
		case "/v1/jobs/{id}":
			sawJobs = true
		case "/v1/selftest":
			// Discovery lists the basic surface only; selftest stays callable.
			sawSelftest = true
		case "/v1/transfers":
			sawTransfers = true
		}
	}
	if !sawJobs || !sawTransfers {
		t.Fatalf("index endpoints incomplete: jobs=%v transfers=%v", sawJobs, sawTransfers)
	}
	if sawSelftest {
		t.Error("index advertises /v1/selftest; discovery lists the basic surface only")
	}
}

// ---- concurrency limit ----

func TestConcurrentJobsLimit(t *testing.T) {
	h := newHarness(t)
	// admin key: give it transfer perm + concurrent_jobs=1 + a second drive
	// (and drop the tiny rpm limit that would trip the requests_per_minute
	// limiter instead of the job limiter).
	k, _ := h.st.GetKeyWithGrants("admin")
	k.Drives[0].Perms = append(k.Drives[0].Perms, "transfer", "fetch")
	k.Drives = append(k.Drives, store.KeyDrive{Drive: "d2", Root: "/", Perms: []string{"list", "read", "write", "delete", "transfer", "fetch"}})
	one := 1
	k.Limits.ConcurrentJobs = &one
	k.Limits.RequestsPerMinute = nil
	if err := h.st.UpdateKey(k); err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Reload(); err != nil {
		t.Fatal(err)
	}
	put2 := func(vis, content string) {
		rec, doc := h.do("PUT", "/v1/files?path="+vis, adminPlain, []byte(content),
			map[string]string{"Content-Type": "application/octet-stream"})
		if rec.Code != 200 {
			t.Fatalf("put %s = %d %v", vis, rec.Code, doc)
		}
	}
	put2("/a.bin", "a")
	put2("/b.bin", "b")
	h.fake.mu.Lock()
	h.fake.taskDelay = 10 * time.Second
	h.fake.mu.Unlock()

	mk := func(p string) (*httptest.ResponseRecorder, map[string]any) {
		return h.do("POST", "/v1/transfers", adminPlain,
			[]byte(fmt.Sprintf(`{"from":{"drive":"d1","path":%q},"to":{"drive":"d2","path":%q}}`, p, "/dst"+p)), jsonCT())
	}
	rec, doc := mk("/a.bin")
	if rec.Code != 202 {
		t.Fatalf("first = %d %v", rec.Code, doc)
	}
	// Wait until it counts as active.
	var active bool
	for i := 0; i < 100; i++ {
		if n, _ := h.st.ActiveJobCount("admin"); n >= 1 {
			active = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !active {
		t.Fatal("job never counted")
	}
	rec, doc = mk("/b.bin")
	if rec.Code != 429 || errCode(doc) != "rate_limited" {
		t.Fatalf("limit = %d %v", rec.Code, doc)
	}
	h.fake.mu.Lock()
	h.fake.taskDelay = 0
	h.fake.mu.Unlock()
}

// ---- restart resume ----

func TestJobsSurviveRestartTracking(t *testing.T) {
	h := newHarness(t)
	grantSecondDrive(t, h)
	put(t, h, "/resume.bin", "resume-payload")
	h.fake.mu.Lock()
	h.fake.taskDelay = 400 * time.Millisecond
	h.fake.mu.Unlock()

	rec, doc := h.do("POST", "/v1/transfers", chrootPlain,
		[]byte(`{"from":{"drive":"d1","path":"/resume.bin"},"to":{"drive":"d2","path":"/resume-copy.bin"}}`), jsonCT())
	if rec.Code != 202 {
		t.Fatalf("create = %d %v", rec.Code, doc)
	}
	id := doc["job"].(map[string]any)["id"].(string)
	// Wait for task ids to be persisted.
	var persisted bool
	for i := 0; i < 100; i++ {
		j, err := h.st.GetJob(id)
		if err == nil {
			res := jobResult(j)
			if ids, ok := res["task_ids"].([]any); ok && len(ids) > 0 {
				persisted = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !persisted {
		j, _ := h.st.GetJob(id)
		t.Fatalf("task ids never persisted; job state=%q err=%q result=%s", j.State, j.Error, j.Result)
	}

	// Simulate a restart: a fresh Server instance resumes active jobs.
	h2 := newHarnessRestarted(t, h)
	h2.api.StartJobs(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := h2.st.GetJob(id)
		if err == nil && j.State == store.JobSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	j, err := h2.st.GetJob(id)
	if err != nil || j.State != store.JobSucceeded {
		t.Fatalf("resumed job = %v err=%v", j, err)
	}
	h.fake.mu.Lock()
	e, ok := h.fake.entries["/m2/agents/x2/resume-copy.bin"]
	h.fake.mu.Unlock()
	if !ok || string(e.content) != "resume-payload" {
		t.Fatalf("resumed copy missing: %v", e)
	}
}

// newHarnessRestarted rebuilds a Server over the SAME store and fake OpenList
// (simulating a cloud2api restart while OpenList keeps its tasks).
func newHarnessRestarted(t *testing.T, h *harness) *harness {
	t.Helper()
	client, err := openlist.New(h.upURL, "token")
	if err != nil {
		t.Fatal(err)
	}
	api := New(h.keys, h.st, client, newRegistry(), nil)
	return &harness{t: t, handler: api.Handler(http.NotFoundHandler()), fake: h.fake, st: h.st, keys: h.keys, api: api}
}
