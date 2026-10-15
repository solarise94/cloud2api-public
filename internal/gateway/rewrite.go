package gateway

import "strings"

// StripMountPrefixes rewrites occurrences of internal mount prefixes in s
// back to logical paths ("/m1/x" -> "/x", DESIGN.md §1/§5). An occurrence
// counts only when it starts the string or is preceded by '/' or ' ', and is
// followed by '/' or ends the string; anything else (e.g. a directory named
// like the mount inside another path, or the host part of a URL) is left
// untouched. Each mount is stripped at most once per string: a path under a
// mount may legitimately contain the mount name deeper ("/m1/m1/z" is mount
// + "/m1/z"), and stripping again would corrupt it. Longer mounts must come
// first so that one mount being a prefix of another does not shadow it;
// Server.New sorts accordingly.
func StripMountPrefixes(s string, mounts []string) string {
	if s == "" || len(mounts) == 0 {
		return s
	}
	for _, m := range mounts {
		i := 0
		for i+len(m) <= len(s) {
			j := strings.Index(s[i:], m)
			if j < 0 {
				break
			}
			i += j
			end := i + len(m)
			leftOK := i == 0 || s[i-1] == '/' || s[i-1] == ' '
			rightOK := end == len(s) || s[end] == '/'
			if leftOK && rightOK {
				s = s[:i] + s[end:]
				break
			}
			i++
		}
	}
	if s == "" {
		return "/"
	}
	return s
}

// rewriteJSON walks a decoded JSON value and strips mount prefixes from
// every string value. json.Number and other scalar types pass through
// unchanged.
func rewriteJSON(v any, mounts []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			t[k] = rewriteJSON(vv, mounts)
		}
		return t
	case []any:
		for i := range t {
			t[i] = rewriteJSON(t[i], mounts)
		}
		return t
	case string:
		return StripMountPrefixes(t, mounts)
	default:
		return v
	}
}

// sensitiveKeys are removed from every proxied JSON response before it is
// returned to the client (v0.3.1, UPGRADE §1.2 P1 / §4.9):
//
//   - raw_url embeds the internal OpenList address and download URL
//   - sign is a non-expiring download credential (…:0)
//   - provider leaks the internal driver name behind a mount
//
// Key-based removal is safe: a file merely named "sign" is a value under a
// "name" key, never a key itself.
var sensitiveKeys = map[string]bool{
	"raw_url":  true,
	"sign":     true,
	"provider": true,
}

// scrubSensitive recursively rebuilds a decoded JSON value without the
// sensitive keys. Clients that need file downloads use /api/storage/{a}/raw,
// where the sign is resolved server-side and never exposed.
func scrubSensitive(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			if sensitiveKeys[k] {
				continue
			}
			out[k] = scrubSensitive(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = scrubSensitive(t[i])
		}
		return out
	default:
		return v
	}
}
