package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripMountPrefixes(t *testing.T) {
	mounts := []string{"/123pan", "/baidu", "/aliyun"} // longest first
	cases := []struct{ in, want string }{
		{"/baidu/x", "/x"},
		{"/baidu", "/"},
		{"/baidu/a/b/c.txt", "/a/b/c.txt"},
		{"/123pan/y", "/y"},
		{"/aliyun", "/"},
		// A name that merely starts with the mount text is untouched.
		{"/baidux", "/baidux"},
		{"/x/baidu/y", "/x/baidu/y"}, // directory named like the mount: kept
		// A directory named like the mount deeper in the path survives: only
		// the leading internal prefix is stripped.
		{"/baidu/baidu/z", "/baidu/z"},
		// Prose / message text with the prefix after a space is rewritten.
		{"failed get /baidu/a and /123pan/b", "failed get /a and /b"},
		// URL paths whose left neighbour is not a boundary are conservative:
		// "…/p/baidu/a" could be a legitimate segment named baidu.
		{"http://h/p/baidu/a", "http://h/p/baidu/a"},
		{"", ""},
		{"no mounts here", "no mounts here"},
	}
	for _, tc := range cases {
		if got := StripMountPrefixes(tc.in, mounts); got != tc.want {
			t.Errorf("StripMountPrefixes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripMountPrefixesOverlappingMounts(t *testing.T) {
	// "/m" is a prefix of "/m/doc"; longest-first ordering must strip
	// "/m/doc" before "/m" ever gets a chance.
	mounts := []string{"/m/doc", "/m"}
	if got := StripMountPrefixes("/m/doc/a.txt", mounts); got != "/a.txt" {
		t.Errorf("got %q, want /a.txt", got)
	}
	if got := StripMountPrefixes("/m/x", mounts); got != "/x" {
		t.Errorf("got %q, want /x", got)
	}
}

func TestRewriteJSONRecursive(t *testing.T) {
	mounts := []string{"/baidu", "/123pan"}
	in := `{
	  "code": 200,
	  "message": "success",
	  "data": {
	    "path": "/baidu/sub/file.txt",
	    "parent": "/baidu/sub",
	    "content": [
	      {"name": "keep", "path": "/123pan/other", "size": 12},
	      {"name": "plain", "path": "/relative"}
	    ],
	    "raw_url": "http://openlist:5244/p/baidu/sub/file.txt?sign=xyz:0",
	    "sign": "xyz:0",
	    "provider": "Local",
	    "count": 3
	  }
	}`
	var doc any
	if err := json.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	// The full proxy pipeline: strip mount prefixes, then scrub leak keys.
	out, err := json.Marshal(scrubSensitive(rewriteJSON(doc, mounts)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	data := m["data"].(map[string]any)
	if data["path"] != "/sub/file.txt" {
		t.Errorf("path = %v", data["path"])
	}
	if data["parent"] != "/sub" {
		t.Errorf("parent = %v", data["parent"])
	}
	// v0.3.1: raw_url/sign/provider must be gone from the response — they
	// leak the internal OpenList address, a non-expiring download sign and
	// the internal driver name (UPGRADE §1.2 P1).
	for _, k := range []string{"raw_url", "sign", "provider"} {
		if _, present := data[k]; present {
			t.Errorf("data.%s leaked into the response: %v", k, data[k])
		}
	}
	if strings.Contains(string(out), "5244") {
		t.Errorf("response still contains the internal OpenList port: %s", out)
	}
	content := data["content"].([]any)
	first := content[0].(map[string]any)
	if first["path"] != "/other" {
		t.Errorf("content[0].path = %v", first["path"])
	}
	second := content[1].(map[string]any)
	if second["path"] != "/relative" {
		t.Errorf("content[1].path = %v", second["path"])
	}
	if first["size"] != float64(12) {
		t.Errorf("size must be preserved, got %v", first["size"])
	}
	if m["code"] != float64(200) || m["message"] != "success" {
		t.Errorf("scalars must be preserved, got %v/%v", m["code"], m["message"])
	}
}

func TestScrubSensitive(t *testing.T) {
	// Content items can carry the keys too (fs/list entries, nested objects).
	in := `{"data":{"content":[{"name":"f","raw_url":"http://127.0.0.1:5244/p/x?sign=s:0","sign":"s:0","provider":"Local","size":1}],"related":{"sign":"deep"}}}`
	var doc any
	if err := json.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(scrubSensitive(doc))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, key := range []string{`"raw_url"`, `"sign"`, `"provider"`, "5244"} {
		if strings.Contains(s, key) {
			t.Errorf("scrubbed response still contains %s: %s", key, s)
		}
	}
	// Legitimate entries named like the keys are values, not keys: kept.
	if !strings.Contains(s, `"name":"f"`) {
		t.Errorf("legitimate fields must survive scrubbing: %s", s)
	}
	// A file named "sign" or "provider" keeps its name value.
	named := `{"data":{"content":[{"name":"sign"},{"name":"provider"}]}}`
	var doc2 any
	if err := json.Unmarshal([]byte(named), &doc2); err != nil {
		t.Fatal(err)
	}
	out2, _ := json.Marshal(scrubSensitive(doc2))
	for _, want := range []string{`"name":"sign"`, `"name":"provider"`} {
		if !strings.Contains(string(out2), want) {
			t.Errorf("file named like a sensitive key must survive: %s", out2)
		}
	}
}

func TestRewriteJSONLeavesNonStrings(t *testing.T) {
	var doc any = json.Number("12345")
	got := rewriteJSON(doc, []string{"/baidu"})
	if n, ok := got.(json.Number); !ok || n.String() != "12345" {
		t.Errorf("json.Number must pass through unchanged, got %#v", got)
	}
}
