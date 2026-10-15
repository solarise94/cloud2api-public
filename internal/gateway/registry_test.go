package gateway

import (
	"sync"
	"testing"

	"cloud2api/internal/config"
)

func staticAccounts() []config.Account {
	return []config.Account{
		{ID: "a1", Provider: "prov1", MountPath: "/m1", Enabled: true},
		{ID: "a2", Provider: "prov2", MountPath: "/m2/doc", Enabled: true},
	}
}

func TestRegistryStaticLoad(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	if _, ok := r.Get("a1"); !ok {
		t.Fatal("static account a1 missing")
	}
	if got := len(r.List()); got != 2 {
		t.Fatalf("List = %d accounts, want 2", got)
	}
	if !r.Static("a1") || r.Static("nope") {
		t.Errorf("Static flags wrong")
	}
	mounts := r.Mounts()
	// Longest first for prefix stripping.
	if len(mounts) != 2 || mounts[0] != "/m2/doc" || mounts[1] != "/m1" {
		t.Errorf("Mounts = %v, want [/m2/doc /m1]", mounts)
	}
}

func TestRegistryAddDynamic(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	err := r.Add(config.Account{ID: "dyn1", Provider: "local", MountPath: "/dyn", Enabled: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	a, ok := r.Get("dyn1")
	if !ok || !a.Enabled || a.MountPath != "/dyn" {
		t.Fatalf("Get(dyn1) = %+v, %v", a, ok)
	}
	if r.Static("dyn1") {
		t.Error("dyn1 must be dynamic")
	}
	// Mount list rebuilt with the new mount, longest first.
	mounts := r.Mounts()
	if len(mounts) != 3 || mounts[0] != "/m2/doc" || mounts[1] != "/dyn" || mounts[2] != "/m1" {
		t.Errorf("Mounts after add = %v", mounts)
	}
}

func TestRegistryAddConflicts(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	cases := []struct {
		name string
		acct config.Account
	}{
		{"duplicate id static", config.Account{ID: "a1", MountPath: "/x"}},
		{"duplicate mount static", config.Account{ID: "new", MountPath: "/m1"}},
		{"relative mount", config.Account{ID: "new", MountPath: "rel"}},
		{"root mount", config.Account{ID: "new", MountPath: "/"}},
		{"unclean mount", config.Account{ID: "new", MountPath: "/x/"}},
		{"id with slash", config.Account{ID: "a/b", MountPath: "/x"}},
		{"empty id", config.Account{ID: "", MountPath: "/x"}},
	}
	for _, tc := range cases {
		if err := r.Add(tc.acct); err == nil {
			t.Errorf("%s: Add(%+v) succeeded, want error", tc.name, tc.acct)
		}
	}
	// Conflict with an existing dynamic account.
	if err := r.Add(config.Account{ID: "dyn", MountPath: "/dyn"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(config.Account{ID: "other", MountPath: "/dyn"}); err == nil {
		t.Error("mount conflict with dynamic account not detected")
	}
	if err := r.Add(config.Account{ID: "dyn", MountPath: "/other"}); err == nil {
		t.Error("id conflict with dynamic account not detected")
	}
	if got := len(r.List()); got != 3 {
		t.Errorf("List = %d, want 3", got)
	}
}

func TestRegistrySetEnabled(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	if err := r.SetEnabled("a1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	a, _ := r.Get("a1")
	if a.Enabled {
		t.Error("a1 still enabled")
	}
	if err := r.SetEnabled("ghost", true); err == nil {
		t.Error("SetEnabled of unknown account succeeded")
	}
}

func TestRegistryRemoveProtectsStatic(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	if err := r.Remove("a1"); err == nil {
		t.Fatal("removing static account succeeded")
	}
	if _, ok := r.Get("a1"); !ok {
		t.Fatal("static account removed anyway")
	}
	if err := r.Remove("ghost"); err == nil {
		t.Fatal("removing unknown account succeeded")
	}
	if err := r.Add(config.Account{ID: "dyn", MountPath: "/dyn"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("dyn"); err != nil {
		t.Fatalf("Remove dynamic: %v", err)
	}
	if _, ok := r.Get("dyn"); ok {
		t.Error("dynamic account still present")
	}
	if got := r.Mounts(); len(got) != 2 || got[0] != "/m2/doc" {
		t.Errorf("Mounts after remove = %v", got)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewAccountRegistry(staticAccounts())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Add(config.Account{ID: "d", Provider: "local", MountPath: "/d"})
			_ = r.List()
			_ = r.Mounts()
			_, _ = r.Get("a1")
			_ = r.Remove("d")
		}(i)
	}
	wg.Wait()
}
