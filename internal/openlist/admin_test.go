package openlist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// pagedFake fakes /api/admin/storage/list (page/per_page honoured) and
// /api/admin/storage/get. mutate hooks let a test bend the responses.
type pagedFake struct {
	storages    []StorageInfo
	perPage     int // page size the fake enforces (0 = per_page honoured)
	listHook    func(page int, nStorages int) (content []StorageInfo, total int, fail bool)
	getResponse func(id int64) (int, string, any)
}

func (f *pagedFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/admin/storage/list":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			if page < 1 {
				page = 1
			}
			if f.listHook != nil {
				content, total, fail := f.listHook(page, len(f.storages))
				if fail {
					fmt.Fprint(w, `{"code":500,"message":"broken"}`)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 200, "message": "success",
					"data": map[string]any{"content": content, "total": total},
				})
				return
			}
			size := f.perPage
			if size <= 0 {
				size = perPage
			}
			lo := (page - 1) * size
			hi := lo + size
			if lo > len(f.storages) {
				lo = len(f.storages)
			}
			if hi > len(f.storages) {
				hi = len(f.storages)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200, "message": "success",
				"data": map[string]any{"content": f.storages[lo:hi], "total": len(f.storages)},
			})
		case r.URL.Path == "/api/admin/storage/get":
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if f.getResponse != nil {
				code, msg, data := f.getResponse(id)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "data": data})
				return
			}
			for _, st := range f.storages {
				if st.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": st})
					return
				}
			}
			// The live instance answers this shape for a deleted id.
			fmt.Fprint(w, `{"code":500,"message":"record not found","data":null}`)
		default:
			fmt.Fprint(w, `{"code":404,"message":"no such endpoint"}`)
		}
	})
}

func mkStorages(n int) []StorageInfo {
	out := make([]StorageInfo, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, StorageInfo{ID: int64(i), MountPath: "/m" + strconv.Itoa(i), Driver: "Local", Status: "work"})
	}
	return out
}

func newPagedClient(t *testing.T, f *pagedFake) *Client {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	c, err := New(ts.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListStoragesPagesUntilTotal(t *testing.T) {
	f := &pagedFake{storages: mkStorages(7), perPage: 3} // 3 pages: 3+3+1
	c := newPagedClient(t, f)
	list, err := c.ListStorages(context.Background())
	if err != nil {
		t.Fatalf("ListStorages: %v", err)
	}
	if len(list) != 7 {
		t.Fatalf("collected %d storages, want 7", len(list))
	}
	for i, st := range list {
		if st.ID != int64(i+1) {
			t.Fatalf("list[%d].ID = %d, want %d", i, st.ID, i+1)
		}
	}
}

func TestListStoragesSinglePage(t *testing.T) {
	c := newPagedClient(t, &pagedFake{storages: mkStorages(2)})
	list, err := c.ListStorages(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("ListStorages = %v, %v", list, err)
	}
	// Empty instance: zero pages, empty (non-nil-safe) result, no error.
	c2 := newPagedClient(t, &pagedFake{})
	list, err = c2.ListStorages(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("empty ListStorages = %v, %v", list, err)
	}
}

func TestListStoragesShortPageFails(t *testing.T) {
	// Page 2 comes back empty although total says 7.
	f := &pagedFake{perPage: 3}
	c := newPagedClient(t, f)
	f.listHook = func(page, nStorages int) ([]StorageInfo, int, bool) {
		if page == 1 {
			return mkStorages(3), 7, false
		}
		return nil, 7, false // empty page, short of total
	}
	if list, err := c.ListStorages(context.Background()); err == nil {
		t.Fatalf("short page must fail, got %v", list)
	}
}

func TestListStoragesTotalChangeFails(t *testing.T) {
	f := &pagedFake{perPage: 3}
	c := newPagedClient(t, f)
	f.listHook = func(page, nStorages int) ([]StorageInfo, int, bool) {
		all := mkStorages(7)
		if page == 1 {
			return all[:3], 7, false
		}
		// The total shrank mid-read (a storage was deleted concurrently).
		return all[3:6], 6, false
	}
	if _, err := c.ListStorages(context.Background()); err == nil {
		t.Fatal("total change mid-read must fail")
	}
}

func TestListStoragesDuplicateIDFails(t *testing.T) {
	f := &pagedFake{}
	c := newPagedClient(t, f)
	dup := append(mkStorages(2), StorageInfo{ID: 1, MountPath: "/other", Driver: "Local"})
	f.listHook = func(page, nStorages int) ([]StorageInfo, int, bool) {
		return dup, len(dup), false
	}
	if _, err := c.ListStorages(context.Background()); err == nil {
		t.Fatal("repeated storage id must fail")
	}
}

func TestListStoragesUpstreamErrorFails(t *testing.T) {
	f := &pagedFake{}
	c := newPagedClient(t, f)
	f.listHook = func(page, nStorages int) ([]StorageInfo, int, bool) {
		return nil, 0, true
	}
	if _, err := c.ListStorages(context.Background()); err == nil {
		t.Fatal("upstream error must fail the listing")
	}
}

func TestGetStorageFoundAndNotFound(t *testing.T) {
	f := &pagedFake{storages: mkStorages(2)}
	c := newPagedClient(t, f)
	st, err := c.GetStorage(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetStorage(2): %v", err)
	}
	if st.MountPath != "/m2" || st.Driver != "Local" {
		t.Fatalf("GetStorage(2) = %+v", st)
	}
	// Deleted id: the live instance's record-not-found maps to the sentinel.
	_, err = c.GetStorage(context.Background(), 42)
	if !errors.Is(err, ErrStorageNotFound) {
		t.Fatalf("GetStorage(42) err = %v, want ErrStorageNotFound", err)
	}
	// Another error shape stays an ordinary error.
	f.getResponse = func(id int64) (int, string, any) {
		return 500, "database locked", nil
	}
	if _, err := c.GetStorage(context.Background(), 1); err == nil || errors.Is(err, ErrStorageNotFound) {
		t.Fatalf("ordinary failure must not be the sentinel: %v", err)
	}
}
