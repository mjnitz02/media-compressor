package scan

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options control which files a walk considers.
type Options struct {
	// Extensions are the file extensions that are candidates, without dots.
	// Empty means every file.
	Extensions []string

	// IgnoreGlobs skip paths outright. A directory that matches is not
	// descended into at all.
	IgnoreGlobs []string

	// MinAge leaves freshly written files alone. This is only half of the
	// eligibility rule -- a file on this server can be older than the grace
	// period and still be mid-move -- and the other half, a size that has not
	// changed since the previous scan, needs the store and arrives in Phase 4.
	MinAge time.Duration

	// Now is the clock, injectable so tests do not have to sleep. Zero means
	// time.Now.
	Now time.Time
}

// Candidate is one file worth probing.
type Candidate struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Skipped is one file or directory the walk declined, and why. It is kept
// rather than discarded because "what did it not look at, and why" is a
// question worth being able to answer without a second run.
type Skipped struct {
	Path   string
	Reason string
}

// Result is what a walk found.
type Result struct {
	Candidates []Candidate
	Skipped    []Skipped

	// Errors are directories or files that could not be read. A walk keeps
	// going past them: one unreadable folder should not stop a library.
	Errors []error
}

// Walk finds the candidate files under a set of roots.
func Walk(roots []string, o Options) Result {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}

	var r Result
	seen := map[string]bool{}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				r.Errors = append(r.Errors, err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}

			if pattern, ok := MatchAny(o.IgnoreGlobs, path); ok {
				r.Skipped = append(r.Skipped, Skipped{path, "matches ignore glob " + pattern})
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}

			// A symlink is not replaceable in place, so it is not a
			// candidate. encode.Prepare refuses them too; catching it here
			// keeps them out of the count entirely.
			if d.Type()&fs.ModeSymlink != 0 {
				r.Skipped = append(r.Skipped, Skipped{path, "symlink"})
				return nil
			}
			if !hasExtension(path, o.Extensions) {
				return nil // not a media file; not worth reporting
			}

			info, err := d.Info()
			if err != nil {
				r.Errors = append(r.Errors, err)
				return nil
			}
			if age := now.Sub(info.ModTime()); o.MinAge > 0 && age < o.MinAge {
				r.Skipped = append(r.Skipped, Skipped{path, fmt.Sprintf(
					"only %s old, waiting for it to settle", roundDuration(age))})
				return nil
			}

			// Two library roots can be different mounts of one directory, and
			// a file should not be planned twice.
			if seen[path] {
				return nil
			}
			seen[path] = true

			r.Candidates = append(r.Candidates, Candidate{path, info.Size(), info.ModTime()})
			return nil
		})
		if err != nil {
			r.Errors = append(r.Errors, err)
		}
	}

	// Sorted so that a dry run of the same library reads the same way twice.
	sort.Slice(r.Candidates, func(i, j int) bool { return r.Candidates[i].Path < r.Candidates[j].Path })
	sort.Slice(r.Skipped, func(i, j int) bool { return r.Skipped[i].Path < r.Skipped[j].Path })
	return r
}

func hasExtension(path string, exts []string) bool {
	if len(exts) == 0 {
		return true
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	for _, e := range exts {
		if strings.EqualFold(ext, strings.TrimPrefix(e, ".")) {
			return true
		}
	}
	return false
}

func roundDuration(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
