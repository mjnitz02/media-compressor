package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write creates a file with a given age, so that the min-age rule can be
// tested without sleeping.
func write(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func paths(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

func TestWalkFiltersByExtensionIgnoresAndAge(t *testing.T) {
	root := t.TempDir()
	old := 2 * time.Hour

	write(t, filepath.Join(root, "Film.mkv"), old)
	write(t, filepath.Join(root, "Show", "S01E01.mp4"), old)
	write(t, filepath.Join(root, "Show", "poster.jpg"), old)
	write(t, filepath.Join(root, ".Recycle.Bin", "Deleted.mkv"), old)
	write(t, filepath.Join(root, "Fresh.mkv"), time.Minute)

	got := Walk([]string{root}, Options{
		Extensions:  []string{"mkv", "mp4"},
		IgnoreGlobs: []string{"**/.Recycle.Bin/**"},
		MinAge:      time.Hour,
	})

	want := []string{
		filepath.Join(root, "Film.mkv"),
		filepath.Join(root, "Show", "S01E01.mp4"),
	}
	if strings.Join(paths(got.Candidates), "|") != strings.Join(want, "|") {
		t.Errorf("candidates = %v, want %v", paths(got.Candidates), want)
	}

	// The two exclusions that were deliberate are reported, so a dry run can
	// answer "why is this file not in the list?". A .jpg is not.
	var reasons []string
	for _, s := range got.Skipped {
		reasons = append(reasons, s.Path+": "+s.Reason)
	}
	joined := strings.Join(reasons, "\n")
	if !strings.Contains(joined, "Recycle") || !strings.Contains(joined, "Fresh.mkv") {
		t.Errorf("skipped should explain the recycle bin and the fresh file, got:\n%s", joined)
	}
	if strings.Contains(joined, "poster.jpg") {
		t.Error("a non-media file should not be reported as skipped; it is just noise")
	}
}

// A symlink cannot be replaced in place without destroying the link, so it is
// not a candidate at all. encode.Prepare refuses them too; this keeps them out
// of the counts.
func TestWalkSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Real.mkv"), time.Hour)
	if err := os.Symlink(filepath.Join(root, "Real.mkv"), filepath.Join(root, "Link.mkv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := Walk([]string{root}, Options{Extensions: []string{"mkv"}})
	if len(got.Candidates) != 1 || filepath.Base(got.Candidates[0].Path) != "Real.mkv" {
		t.Fatalf("candidates = %v, want only Real.mkv", paths(got.Candidates))
	}
	if len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0].Reason, "symlink") {
		t.Errorf("skipped = %+v, want the symlink reported", got.Skipped)
	}
}

// Two library roots can be different mounts of the same directory. Planning
// one file twice would mean two workers racing to replace it.
func TestWalkDoesNotReturnAFileTwice(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Film.mkv"), time.Hour)

	got := Walk([]string{root, root}, Options{Extensions: []string{"mkv"}})
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %v, want one", paths(got.Candidates))
	}
}

// One unreadable directory should not stop a library.
func TestWalkKeepsGoingPastAMissingRoot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Film.mkv"), time.Hour)

	got := Walk([]string{filepath.Join(root, "nope"), root}, Options{Extensions: []string{"mkv"}})
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %v, want the readable root's file", paths(got.Candidates))
	}
	if len(got.Errors) == 0 {
		t.Error("the missing root should still be reported as an error")
	}
}

func TestWalkExtensionMatchingIsCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "Film.MKV"), time.Hour)

	got := Walk([]string{root}, Options{Extensions: []string{"mkv"}})
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %v, want Film.MKV", paths(got.Candidates))
	}
}
