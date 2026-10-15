package gateway

import (
	"net/url"
	"strings"
	"testing"
)

func TestCleanLogicalPathAccepted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/a", "/a"},
		{"a", "/a"},
		{"/a/b/c", "/a/b/c"},
		{"a//b", "/a/b"},
		{"/a/./b", "/a/b"},
		{"/a/", "/a"},
		{"/文档/文件.txt", "/文档/文件.txt"},
		{"/a b/file name.txt", "/a b/file name.txt"},
		{"/a..b", "/a..b"}, // dots inside a segment are legal
		{"/...", "/..."},   // three dots is a legal name
		{"/file%20name", "/file%20name"},
	}
	for _, tc := range cases {
		got, err := CleanLogicalPath(tc.in)
		if err != nil {
			t.Errorf("CleanLogicalPath(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CleanLogicalPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanLogicalPathRejected(t *testing.T) {
	// §4.3 rule 1 (NUL / %00 / backslash), rule 2 (traversal, escape), plus
	// encoded dot-dot and hostile mixed forms (acceptance case A5).
	cases := []string{
		"\x00",
		"a\x00b",
		"%00",
		"a%00b",
		"%00%2e%2e",
		"\\",
		"a\\b",
		"\\..\\..\\etc",
		"..",
		"../",
		"/../",
		"/..",
		"a/..",
		"/a/../../b",
		"/a/../../../etc/passwd",
		"/baidu/../123pan",
		"%2e%2e",
		"%2E%2E",
		"/x/%2e%2e/y",
		"/x/%2E%2E%2Fy",
		"..%00",
		"/../../",
	}
	for _, in := range cases {
		if got, err := CleanLogicalPath(in); err == nil {
			t.Errorf("CleanLogicalPath(%q) = %q, want error", in, got)
		}
	}
}

// TestCleanLogicalPathSingleDecode verifies that a path that has been decoded
// exactly once (as net/http presents it) is judged on its decoded form: the
// encoded %2e%2e becomes ".." and is rejected, while a second decode is never
// performed so double-encoded forms stay inert.
func TestCleanLogicalPathSingleDecode(t *testing.T) {
	// What net/http hands us for the raw path "/api/x/%2e%2e/y":
	decoded := mustUnescape("/api/x/%2e%2e/y")
	if decoded != "/api/x/../y" {
		t.Fatalf("single decode of %%2e%%2e = %q", decoded)
	}
	if _, err := CleanLogicalPath(decoded); err == nil {
		t.Error("decoded percent-encoded dot-dot must be rejected")
	}
	// Double-encoded "%252e%252e" decodes once to the literal "%2e%2e",
	// which is rejected as an encoded traversal attempt rather than being
	// decoded a second time.
	once := mustUnescape("/x/%252e%252e")
	if once != "/x/%2e%2e" {
		t.Fatalf("single decode of %%252e%%252e = %q", once)
	}
	if _, err := CleanLogicalPath(once); err == nil {
		t.Error("literal percent-encoded dot-dot after one decode must be rejected, not decoded again")
	}
}

func mustUnescape(s string) string {
	d, err := url.PathUnescape(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestMountJoin(t *testing.T) {
	cases := []struct{ mount, logical, want string }{
		{"/baidu", "/", "/baidu"},
		{"/baidu", "/x", "/baidu/x"},
		{"/baidu", "/a/b/c.txt", "/baidu/a/b/c.txt"},
		{"/123pan", "/x", "/123pan/x"},
		{"/b", "/ox", "/b/ox"}, // prefix must include the separator
	}
	for _, tc := range cases {
		got, err := MountJoin(tc.mount, tc.logical)
		if err != nil {
			t.Errorf("MountJoin(%q, %q) error = %v", tc.mount, tc.logical, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MountJoin(%q, %q) = %q, want %q", tc.mount, tc.logical, got, tc.want)
		}
	}
}

// TestMountJoinPrefixBypass checks §4.3 rule 3's string prefix guard: the
// joined result must live under mount + "/", never merely share its prefix.
func TestMountJoinPrefixBypass(t *testing.T) {
	// "x" does not start with "/", so mount+logical = "/baidux" is not under
	// "/baidu/" and must be rejected (sibling-directory confusion).
	if got, err := MountJoin("/baidu", "x"); err == nil {
		t.Errorf("MountJoin(/baidu, x) = %q, want error", got)
	}
	// Even with a hand-crafted mount, ".." can never escape the mount.
	if got, err := MountJoin("/baidu", "/.."); err == nil {
		t.Errorf("MountJoin(/baidu, /..) = %q, want error", got)
	}
}

func TestCleanName(t *testing.T) {
	for _, ok := range []string{"a.txt", "b c.tar.gz", "文档", "..dots", "a+b", "100%", "file(1)"} {
		if got, err := CleanName(ok); err != nil || got != ok {
			t.Errorf("CleanName(%q) = %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{
		"", ".", "..", "a/b", "/", "\x00", "a\x00", "%00", "a\\b", "..\\",
		"%2e%2e", "%2E%2E", "x%2e%2e",
	} {
		if got, err := CleanName(bad); err == nil {
			t.Errorf("CleanName(%q) = %q, want error", bad, got)
		}
	}
}

func TestUnescapeOnce(t *testing.T) {
	cases := []struct{ in, want string }{
		{"%2Fetc%2Fpasswd", "/etc/passwd"},
		{"a+b", "a b"}, // query-escaped form decodes '+' to space
		{"plain.txt", "plain.txt"},
		{"100%zz", "100%zz"}, // invalid escape: taken verbatim, never re-decoded
		{"%252e", "%2e"},     // exactly one decode
	}
	for _, tc := range cases {
		if got := UnescapeOnce(tc.in); got != tc.want {
			t.Errorf("UnescapeOnce(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Decoding twice must never happen.
	in := "%252e%252e"
	once := UnescapeOnce(in)
	if strings.Contains(once, "..") {
		t.Errorf("UnescapeOnce(%q) = %q: decoded more than once", in, once)
	}
}
