package goapiclient

import (
	"strings"
	"testing"
)

func TestParseHop(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    int
		wantErr error
	}{
		{name: "absent", values: nil, want: 0},
		{name: "zero", values: []string{"0"}, want: 0},
		{name: "max", values: []string{"4"}, want: 4},
		{name: "space padded", values: []string{" 2 "}, wantErr: ErrHopMalformed},
		{name: "plus sign", values: []string{"+1"}, wantErr: ErrHopMalformed},
		{name: "leading zero", values: []string{"01"}, wantErr: ErrHopMalformed},
		{name: "float", values: []string{"1.5"}, wantErr: ErrHopMalformed},
		{name: "multiple", values: []string{"1", "2"}, wantErr: ErrHopMalformed},
		{name: "not a number", values: []string{"x"}, wantErr: ErrHopMalformed},
		{name: "negative", values: []string{"-1"}, wantErr: ErrHopMalformed},
		{name: "empty", values: []string{""}, wantErr: ErrHopMalformed},
		{name: "above cap", values: []string{"5"}, wantErr: ErrHopExceeded},
		{name: "far above cap", values: []string{"999"}, wantErr: ErrHopExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseHop(tc.values)
			if tc.wantErr != nil {
				if err != tc.wantErr {
					t.Fatalf("ParseHop(%v) error = %v, want %v", tc.values, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHop(%v) unexpected error %v", tc.values, err)
			}
			if got != tc.want {
				t.Fatalf("ParseHop(%v) = %d, want %d", tc.values, got, tc.want)
			}
		})
	}
}

func TestHopContext(t *testing.T) {
	ctx := t.Context()
	if h, marked := HopFromContext(ctx); h != 0 || marked {
		t.Fatalf("empty context must read as unmarked 0, got %d %v", h, marked)
	}
	if hop, send := OutboundHop(ctx); send || hop != 0 {
		t.Fatal("an unmarked context sends no hop header")
	}
	if h, marked := HopFromContext(WithHop(ctx, 0)); h != 0 || !marked {
		t.Fatal("WithHop 0 must mark the context as a zero-hop transit")
	}
	if hop, send := OutboundHop(WithHop(ctx, 0)); !send || hop != 1 {
		t.Fatal("outbound from a zero-hop transit must be 1")
	}
	if h, _ := HopFromContext(WithHop(ctx, 3)); h != 3 {
		t.Fatal("WithHop must round-trip")
	}
	if hop, _ := OutboundHop(WithHop(ctx, 3)); hop != 4 {
		t.Fatal("outbound hop must be ingress+1")
	}
	if _, marked := HopFromContext(nil); marked {
		t.Fatal("nil context must be unmarked")
	}
}

func TestCleanRel(t *testing.T) {
	ok := map[string]string{
		"":           "",
		"/":          "",
		"/a":         "a",
		"/a/b/c":     "a/b/c",
		"a//b":       "a/b",
		"/名称/文件.txt": "名称/文件.txt",
	}
	for in, want := range ok {
		got, err := CleanRel(in)
		if err != nil {
			t.Fatalf("CleanRel(%q) unexpected error %v", in, err)
		}
		if got != want {
			t.Fatalf("CleanRel(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{
		"..", "/..", "/a/../..", "a/..",
		"a/b", // fine actually — keep list below for real rejects only
	}
	_ = bad
	rejects := []string{
		"..", "/..", "../x", "a/../..",
		"a\\b",
		"a%2fb", "a%2Fb", "a%5cb", "a%252fb", "a%00b",
		"a\x00b",
		"a\nb",
		".", "./",
		strings.Repeat("x", maxNameBytes+1),
	}
	for _, in := range rejects {
		if _, err := CleanRel(in); err == nil {
			t.Fatalf("CleanRel(%q) must be rejected", in)
		}
	}
}

func TestRemoteJoin(t *testing.T) {
	cases := []struct{ root, rel, want string }{
		{"/", "/a/b", "/a/b"},
		{"", "a", "/a"},
		{"/docs", "/a", "/docs/a"},
		{"/docs/", "a/b", "/docs/a/b"},
	}
	for _, tc := range cases {
		got, err := RemoteJoin(tc.root, tc.rel)
		if err != nil {
			t.Fatalf("RemoteJoin(%q,%q) unexpected error %v", tc.root, tc.rel, err)
		}
		if got != tc.want {
			t.Fatalf("RemoteJoin(%q,%q) = %q, want %q", tc.root, tc.rel, got, tc.want)
		}
	}
	if _, err := RemoteJoin("/docs", "../etc"); err == nil {
		t.Fatal("RemoteJoin must reject traversal in rel")
	}
	if _, err := RemoteJoin("..", "a"); err == nil {
		t.Fatal("RemoteJoin must reject traversal in root")
	}
}

func TestWithinRoot(t *testing.T) {
	ok := []struct{ root, p, want string }{
		{"/", "/a", "/a"},
		{"/", "/", "/"},
		{"/docs", "/", "/docs"},
		{"/docs", "/docs/a/b", "/docs/a/b"},
		{"/docs", "/docs", "/docs"},
	}
	for _, tc := range ok {
		got, err := WithinRoot(tc.root, tc.p)
		if err != nil || got != tc.want {
			t.Fatalf("WithinRoot(%q,%q) = %q,%v want %q", tc.root, tc.p, got, err, tc.want)
		}
	}
	rejects := [][2]string{
		{"/docs", "/etc/passwd"},
		{"/docs", "/docsattack/x"},
		{"/docs", "/do"},
	}
	for _, tc := range rejects {
		if got, err := WithinRoot(tc[0], tc[1]); err == nil {
			t.Fatalf("WithinRoot(%q,%q) = %q, must be rejected", tc[0], tc[1], got)
		}
	}
	if _, err := WithinRoot("/docs", "/docs/%2f.."); err == nil {
		t.Fatal("WithinRoot must reject encoded separators")
	}
}

func TestCheckEntry(t *testing.T) {
	if err := CheckEntry("/", "a", "/a", false, 1); err != nil {
		t.Fatalf("root entry must pass: %v", err)
	}
	if err := CheckEntry("/docs", "b", "/docs/b", true, 0); err != nil {
		t.Fatalf("nested entry must pass: %v", err)
	}
	bad := []struct {
		dir, name, path string
		isDir           bool
		size            int64
	}{
		{"/", "..", "/..", true, 0},
		{"/", "a/b", "/a/b", false, 0},
		{"/", "a", "/b", false, 0},
		{"/", "a", "", false, 0},
		{"/docs", "a", "/other/a", false, 0},
		{"/", "a%2fb", "/a%2fb", false, 0},
		{"/", "x", "/x", false, -1},
	}
	for _, tc := range bad {
		if err := CheckEntry(tc.dir, tc.name, tc.path, tc.isDir, tc.size); err == nil {
			t.Fatalf("CheckEntry(%q,%q,%q,%v,%d) must be rejected", tc.dir, tc.name, tc.path, tc.isDir, tc.size)
		}
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"a", "报告 final.pdf", "v1.2.tar.gz", "看/不行?不", "看\\不行?不"} {
		_ = ok
	}
	for _, good := range []string{"a", "报告 final.pdf", "v1.2.tar.gz"} {
		if err := CheckName(good); err != nil {
			t.Fatalf("CheckName(%q) unexpected error %v", good, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a\\b", "%2e", "%25%32f"} {
		if err := CheckName(bad); err == nil {
			t.Fatalf("CheckName(%q) must be rejected", bad)
		}
	}
}
