package agentapi

import (
	"context"
	"net/http"
	"testing"
)

type contextT = context.Context

// TestDriveOpRetriesAfterOpenListMountRename covers the §3.7.4 in-request
// retry: the health snapshot is still warm from before the rename, the first
// attempt resolves against the stale mount and fails with a storage-not-found
// class error, the gate discards it, one reconcile pass follows the new mount
// and the retried op succeeds.
func TestDriveOpRetriesAfterOpenListMountRename(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.entries["/m1/agents/x/after-rename.txt"] = &fakeEntry{
		name: "after-rename.txt", content: []byte("rename-retry-body"), modified: "2026-10-12T00:00:00Z",
	}
	h.fake.mu.Unlock()

	// Warm the 30s health cache while the old mount is still in place.
	rec, doc := h.do(http.MethodGet, "/v1/list?path=/", chrootPlain, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("warm-up list = %d %v", rec.Code, doc)
	}

	// The user renames the storage's mount in OpenList.
	h.fake.renameMount1("/m1-renamed")
	h.api.SetReconciler(func(ctx contextT) { _ = h.api.registry.SetMount("d1", "/m1-renamed") })

	rec, doc = h.do(http.MethodGet, "/v1/list?path=/", chrootPlain, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after rename = %d %v (want in-request retry to follow the mount)", rec.Code, doc)
	}
	items, _ := doc["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "after-rename.txt" {
		t.Fatalf("items = %v", doc)
	}

	// A POST op with a JSON body also retries (the body is replayed).
	h.fake.renameMount1("/m1-again")
	h.api.SetReconciler(func(ctx contextT) { _ = h.api.registry.SetMount("d1", "/m1-again") })
	rec, doc = h.do(http.MethodPost, "/v1/mkdir", chrootPlain,
		[]byte(`{"path":"/made-after-rename"}`), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mkdir after second rename = %d %v (JSON body must be replayed on retry)", rec.Code, doc)
	}
}
