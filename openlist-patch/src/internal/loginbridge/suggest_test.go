package loginbridge

import (
	"strconv"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// seedMount inserts a storage row with the given mount path.
func seedMount(t *testing.T, mount string) {
	t.Helper()
	if err := db.GetDb().Create(&model.Storage{MountPath: mount, Driver: "ConnectFake", Addition: "{}"}).Error; err != nil {
		t.Fatalf("seed mount %s: %v", mount, err)
	}
}

func TestSuggestMount(t *testing.T) {
	// Free name: the plain platform name.
	got, err := SuggestMount("建议盘")
	if err != nil {
		t.Fatalf("SuggestMount: %v", err)
	}
	if got != "/建议盘" {
		t.Fatalf("free suggestion = %q", got)
	}

	// Slashes and control characters are stripped, surrounding space trimmed.
	got, err = SuggestMount(" 建议/盘\tX ")
	if err != nil {
		t.Fatalf("SuggestMount dirty: %v", err)
	}
	if got != "/建议盘X" {
		t.Fatalf("cleaned suggestion = %q", got)
	}

	// Taken: first free suffix wins.
	seedMount(t, "/建议盘")
	seedMount(t, "/建议盘-2")
	t.Cleanup(func() {
		db.GetDb().Where("mount_path LIKE ?", "/建议盘%").Delete(&model.Storage{})
	})
	got, err = SuggestMount("建议盘")
	if err != nil {
		t.Fatalf("SuggestMount taken: %v", err)
	}
	if got != "/建议盘-3" {
		t.Fatalf("taken suggestion = %q, want /建议盘-3", got)
	}

	// A name that cleans to nothing usable (the bare root) suggests nothing.
	if got, err = SuggestMount("/"); err != nil || got != "" {
		t.Fatalf("empty name suggestion = %q err=%v, want empty", got, err)
	}
}

func TestSuggestMountExhausted(t *testing.T) {
	for i := 1; i <= 99; i++ {
		mount := "/全占盘"
		if i > 1 {
			mount += "-" + strconv.Itoa(i)
		}
		seedMount(t, mount)
	}
	t.Cleanup(func() {
		db.GetDb().Where("mount_path LIKE ?", "/全占盘%").Delete(&model.Storage{})
	})
	got, err := SuggestMount("全占盘")
	if err != nil {
		t.Fatalf("SuggestMount exhausted: %v", err)
	}
	if got != "" {
		t.Fatalf("exhausted suggestion = %q, want empty", got)
	}
}
