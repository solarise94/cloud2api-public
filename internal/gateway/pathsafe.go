package gateway

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

var errBadPath = errors.New("invalid path")

// CleanLogicalPath implements DESIGN.md §4.3 rules 1-2 on a client-supplied
// logical path. The input must already be URL-decoded exactly once (net/http
// does this for request paths; JSON body fields are taken verbatim and never
// decoded).
func CleanLogicalPath(p string) (string, error) {
	if err := rejectForbidden(p); err != nil {
		return "", err
	}
	// Any ".." segment present before normalization is a traversal attempt:
	// reject it outright so that e.g. "/baidu/../123pan" is not silently
	// rewritten to "/123pan" in place.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: traversal with '..' is not allowed", errBadPath)
		}
	}
	cleaned := path.Clean("/" + p)
	// With the leading "/" prefix Clean can never escape the root; verify
	// anyway as defense in depth.
	if !strings.HasPrefix(cleaned, "/") || hasDotDotSegment(cleaned) {
		return "", fmt.Errorf("%w: escapes root", errBadPath)
	}
	return cleaned, nil
}

// MountJoin implements §4.3 rule 3: attach the internal mount prefix and
// confirm by string prefix check that the result stays under mount.
func MountJoin(mount, logical string) (string, error) {
	internal := path.Clean(mount + logical)
	if internal != mount && !strings.HasPrefix(internal, mount+"/") {
		return "", fmt.Errorf("%w: escapes mount %s", errBadPath, mount)
	}
	return internal, nil
}

// CleanName validates a single path segment (the `names` entries of
// copy/move/remove), which must not contain separators or traversal segments.
func CleanName(name string) (string, error) {
	if err := rejectForbidden(name); err != nil {
		return "", err
	}
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: invalid name %q", errBadPath, name)
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: name %q must be a single path segment", errBadPath, name)
	}
	return name, nil
}

func rejectForbidden(p string) error {
	// §4.3 rule 1: reject NUL bytes and backslashes.
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("%w: NUL byte or backslash", errBadPath)
	}
	// Encoded forms are rejected on the literal too: URL paths are decoded
	// exactly once (so %00/%2e%2e have already become NUL/\"..\" there),
	// but JSON body fields are never decoded and must not sneak them past.
	lower := strings.ToLower(p)
	if strings.Contains(lower, "%00") {
		return fmt.Errorf("%w: encoded NUL byte", errBadPath)
	}
	if strings.Contains(lower, "%2e%2e") {
		return fmt.Errorf("%w: encoded '..' sequence", errBadPath)
	}
	return nil
}

func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// LooksHostPath reports whether p carries anything the path cleaner would
// reject (forbidden bytes or traversal segments). Routing uses it so that
// hostile paths which never reach a path field (e.g. "/raw/../x") are still
// answered with 400 instead of 404 (§4.3, acceptance A5).
func LooksHostPath(p string) bool {
	if rejectForbidden(p) != nil || hasDotDotSegment(p) {
		return true
	}
	return false
}

// UnescapeOnce decodes the File-Path upload header exactly once
// (query-escaped form, the OpenList client convention). Values that do not
// decode cleanly are taken verbatim; decoding is never repeated (§4.3
// rule 5). Callers must therefore always send this header URL-encoded.
func UnescapeOnce(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}
