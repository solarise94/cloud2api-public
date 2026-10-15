package goapiclient

import (
	"io"
	"strings"
	"testing"
)

func TestRangeWindowReaderHasNormalEOF(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantError     bool
	}{
		{"exact", "abc", false},
		{"short", "ab", true},
		{"long", "abcd", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &exactLenReader{rc: io.NopCloser(strings.NewReader(tc.content)), remaining: 3}
			body, err := io.ReadAll(r)
			_ = r.Close()
			if (err != nil) != tc.wantError {
				t.Fatalf("window reader returned %q with error %v, want error %v", body, err, tc.wantError)
			}
			if !tc.wantError && string(body) != "abc" {
				t.Fatalf("exact window = %q", body)
			}
		})
	}
}

func TestOpenEndedRangeReturnsVerifiableWindow(t *testing.T) {
	window, err := checkContentRange("bytes 2-4/5", "bytes=2-")
	if err != nil || window != 3 {
		t.Fatalf("open-ended window = %d, %v; want 3", window, err)
	}
	if _, err := checkContentRange("bytes 0-18446744073709551615/*", "bytes=0-"); err == nil {
		t.Fatal("overflowing range window was accepted")
	}
}
