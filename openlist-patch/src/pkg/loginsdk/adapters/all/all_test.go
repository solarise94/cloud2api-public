//go:build !loginsdk_example

package all

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// The default build bundles exactly these adapters, in this order.
func TestBundledRegistry(t *testing.T) {
	got := loginsdk.Adapters()
	want := []string{"baidu", "aliyun", "123pan", "onedrive", "googledrive", "goapi"}
	if len(got) != len(want) {
		t.Fatalf("registry has %d adapters, want %d: %v", len(got), len(want), ids(got))
	}
	seenIDs := map[string]bool{}
	seenDrivers := map[string]bool{}
	for i, a := range got {
		m := a.Meta()
		if m.ID != want[i] {
			t.Fatalf("adapter %d = %q, want %q", i, m.ID, want[i])
		}
		if err := loginsdk.Validate(a); err != nil {
			t.Fatalf("adapter %q: %v", m.ID, err)
		}
		if seenIDs[m.ID] {
			t.Fatalf("duplicate adapter id %q", m.ID)
		}
		if seenDrivers[m.Driver] {
			t.Fatalf("duplicate driver %q", m.Driver)
		}
		seenIDs[m.ID] = true
		seenDrivers[m.Driver] = true
	}
}

func ids(adapters []loginsdk.Adapter) []string {
	out := make([]string, 0, len(adapters))
	for _, a := range adapters {
		out = append(out, a.Meta().ID)
	}
	return out
}
