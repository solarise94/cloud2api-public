package goapiclient

import (
	stdpath "path"
	"strings"
	"unicode/utf8"
)

// Path validation for everything the driver sends to or accepts from a GoAPI
// peer. The rules are deliberately stricter than the peer contract: OpenList
// paths are already cleaned once, but a hostile or broken peer must never be
// able to smuggle separators, traversal or encoded variants through a name.

const (
	maxPathBytes = 2048
	maxNameBytes = 512
)

// checkSegment rejects one path segment that could escape the chroot or
// split into several segments on a re-decode: separators, traversal segments,
// control characters and the percent-encoded forms of separators (single and
// double encoded). A literal "%2f" in a name is legal on disk but becomes a
// separator the moment any hop re-decodes it, so it is refused here.
func checkSegment(seg string) error {
	switch seg {
	case "", ".", "..":
		return ErrInvalidPath
	}
	if !utf8.ValidString(seg) {
		return ErrInvalidPath
	}
	if len(seg) > maxNameBytes {
		return ErrInvalidPath
	}
	for _, r := range seg {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidPath
		}
	}
	if strings.ContainsAny(seg, `/\`) {
		return ErrInvalidPath
	}
	lower := strings.ToLower(seg)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") ||
		strings.Contains(lower, "%2e") || strings.Contains(lower, "%25") ||
		strings.Contains(lower, "%00") {
		// Encoded separators, encoded dots (any mix) and double-encoding
		// markers: a name survives only when no hop can re-decode it into
		// something else.
		return ErrInvalidPath
	}
	return nil
}

// CleanRel validates a path relative to the configured root and returns its
// cleaned form without a leading slash ("" for the root itself). The input
// arrives from OpenList, which has already cleaned it once; this is the
// driver's own second line of defense.
func CleanRel(p string) (string, error) {
	if len(p) > maxPathBytes {
		return "", ErrInvalidPath
	}
	if p == "/" || p == "" {
		return "", nil
	}
	var segs []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg == "" {
			continue // "a//b" is a double slash, not a name
		}
		if err := checkSegment(seg); err != nil {
			return "", err
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return "", nil
	}
	return stdpath.Join(segs...), nil
}

// RemoteJoin validates a configured root and a relative path and returns the
// remote path to send: "/<root>/<rel>".
func RemoteJoin(root, rel string) (string, error) {
	rootClean, relClean, err := cleanRootRel(root, rel)
	if err != nil {
		return "", err
	}
	if rootClean == "" {
		return "/" + relClean, nil
	}
	return "/" + rootClean + "/" + relClean, nil
}

// WithinRoot validates that p, a remote path received from OpenList (a
// directory object is built by joining the root object's path, so remote
// paths handed to the driver already carry the root), is the configured root
// itself or a descendant of it, and returns its cleaned remote form.
func WithinRoot(root, p string) (string, error) {
	rootClean, _, err := cleanRootRel(root, "")
	if err != nil {
		return "", err
	}
	if len(p) > maxPathBytes {
		return "", ErrInvalidPath
	}
	if p == "/" || p == "" {
		return "/" + rootClean, nil
	}
	rel, err := CleanRel(p)
	if err != nil {
		return "", err
	}
	if rootClean != "" && rel != rootClean && !strings.HasPrefix(rel, rootClean+"/") {
		return "", ErrInvalidPath
	}
	return "/" + rel, nil
}

func cleanRootRel(root, rel string) (rootClean, relClean string, err error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "/"
	}
	rootClean, err = CleanRel(root)
	if err != nil {
		return "", "", ErrInvalidPath
	}
	relClean, err = CleanRel(rel)
	if err != nil {
		return "", "", err
	}
	return rootClean, relClean, nil
}

// CheckName validates a single path segment (a file or directory name coming
// from OpenList or chosen by a user), with the same rules as CleanRel.
func CheckName(name string) error {
	return checkSegment(name)
}

// CheckEntry validates one directory entry against the directory it was
// listed from. name must be a single clean segment, must equal the base of
// path, and path must equal the directory joined with the name; sizes are
// non-negative. Anything else means the peer answered outside the contract
// and the whole listing is rejected (fail closed).
func CheckEntry(dir, name, path string, isDir bool, size int64) error {
	if err := checkSegment(name); err != nil {
		return ErrInvalidPath
	}
	if size < 0 {
		return ErrInvalidListing
	}
	if path == "" || name != stdpath.Base(path) {
		return ErrInvalidListing
	}
	if dir == "/" || dir == "" {
		if path != "/"+name {
			return ErrInvalidListing
		}
		return nil
	}
	if stdpath.Join(dir, name) != path {
		return ErrInvalidListing
	}
	return nil
}
