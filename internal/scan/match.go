// Package scan finds candidate files under a library's paths.
//
// Phase 3 uses it for --dry-run and for running a library by hand. Phase 4
// builds the real scanner on top: this package answers "which files are even
// candidates", and the store answers "which of those are eligible and have not
// been done already".
package scan

import (
	"path/filepath"
	"strings"
)

// Match reports whether a path matches an ignore glob.
//
// path/filepath.Match is the engine for one path segment, but it does not
// understand `**`, and every useful ignore pattern here needs it:
// `**/.Recycle.Bin/**` has to match at any depth. So the pattern and the path
// are split into segments and matched segment by segment, with `**` standing
// for zero or more of them.
//
// A pattern containing no separator at all is matched against the base name,
// which is what anyone writing `*.partial.*` means.
func Match(pattern, path string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		ok, err := filepath.Match(pattern, filepath.Base(path))
		return err == nil && ok
	}
	return matchSegments(
		strings.Split(pattern, "/"),
		strings.Split(filepath.ToSlash(filepath.Clean(path)), "/"),
	)
}

// MatchAny reports whether any pattern matches, and which one.
func MatchAny(patterns []string, path string) (string, bool) {
	for _, p := range patterns {
		if Match(p, path) {
			return p, true
		}
	}
	return "", false
}

// ValidPattern reports whether a pattern is syntactically usable, so that a
// typo is a startup error rather than a rule that silently never fires.
func ValidPattern(pattern string) bool {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := filepath.Match(seg, ""); err == filepath.ErrBadPattern {
			return false
		}
	}
	return true
}

// matchSegments matches pattern segments against path segments, where `**`
// consumes zero or more path segments.
//
// Go note: this is the textbook recursive glob matcher. Patterns here have a
// handful of segments and at most one or two `**`, so the exponential worst
// case is not reachable in practice.
func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		// Try consuming 0, 1, 2 ... path segments.
		for i := 0; i <= len(path); i++ {
			if matchSegments(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pattern[0], path[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
}
