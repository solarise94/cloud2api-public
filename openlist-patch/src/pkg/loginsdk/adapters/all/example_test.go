//go:build loginsdk_example

package all

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/pkg/loginsdk"
)

// With the example tag the registry lists the bundled adapters plus the
// example, registered last.
func TestExampleRegisteredLast(t *testing.T) {
	got := loginsdk.Adapters()
	want := []string{"baidu", "aliyun", "123pan", "onedrive", "googledrive", "goapi", "example-openlist"}
	if len(got) != len(want) {
		t.Fatalf("registry has %d adapters, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.Meta().ID != want[i] {
			t.Fatalf("adapter %d = %q, want %q", i, a.Meta().ID, want[i])
		}
		if err := loginsdk.Validate(a); err != nil {
			t.Fatalf("adapter %q: %v", a.Meta().ID, err)
		}
	}
}
