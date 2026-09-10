package store

import (
	"context"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// decided is the fixture the UI queries are about: a library where most files
// are already as the profile wants them, a few were declined, and one has
// work outstanding.
func decided(t *testing.T) *Store {
	t.Helper()
	s := open(t)
	ctx := context.Background()
	mtime := t0.Add(-72 * hour)

	files := []struct {
		path string
		size int64
		d    Decision
	}{
		{"/m/already.mkv", 1, Decision{Action: decide.ActionNone, Video: decide.VideoCopy}},
		{"/m/also-already.mkv", 2, Decision{Action: decide.ActionNone, Video: decide.VideoCopy}},
		{"/m/too-low.mkv", 3, Decision{Action: decide.ActionNone, Video: decide.VideoSkippedFloor,
			Reason: "target 1470 kbps is below floor 3000 kbps"}},
		{"/m/work.mkv", 4, Decision{Action: decide.ActionEncode, Video: decide.VideoEncode}},
		{"/m/one-track.mkv", 5, Decision{Action: decide.ActionRemux, Video: decide.VideoCopy,
			Notes: []string{"kept the last audio track even though it matches no keep rule"}}},
	}

	seen := make([]Sighting, len(files))
	for i, f := range files {
		seen[i] = sight(f.path, f.size, mtime)
	}
	observe(t, s, t0, seen...)

	for _, f := range files {
		f.d.Profile, f.d.Fingerprint, f.d.DecidedAt = "standard", "abc", t0
		if err := s.RecordDecision(ctx, f.path, f.size, mtime, f.d); err != nil {
			t.Fatalf("RecordDecision(%s): %v", f.path, err)
		}
	}
	return s
}

func TestDecisionGroupsCountEveryDecidedFileExactlyOnce(t *testing.T) {
	s := decided(t)
	groups, err := s.DecisionGroups(context.Background())
	if err != nil {
		t.Fatalf("DecisionGroups: %v", err)
	}

	total := 0
	found := map[string]int{}
	for _, g := range groups {
		total += g.Count
		found[string(g.Action)+"/"+string(g.Video)] = g.Count
	}
	if total != 5 {
		t.Errorf("groups total %d files, want 5", total)
	}
	if found["none/copy"] != 2 {
		t.Errorf("none/copy = %d, want 2", found["none/copy"])
	}
	if found["none/skipped_floor"] != 1 {
		t.Errorf("none/skipped_floor = %d, want 1", found["none/skipped_floor"])
	}
	// Commonest first, so the shape of the library reads off the top row.
	if groups[0].Count < groups[len(groups)-1].Count {
		t.Errorf("groups are not ordered commonest first: %+v", groups)
	}
}

// The bucket that matters: an encode this tool declined, listed with the
// reason it gave. Auditing that is the point of the whole view.
func TestFilesByDecisionListsTheDeclinedEncodesWithTheirReason(t *testing.T) {
	s := decided(t)
	files, err := s.FilesByDecision(context.Background(), "", decide.VideoSkippedFloor, 0)
	if err != nil {
		t.Fatalf("FilesByDecision: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].Path != "/m/too-low.mkv" {
		t.Errorf("path = %s", files[0].Path)
	}
	if files[0].Decision.Reason != "target 1470 kbps is below floor 3000 kbps" {
		t.Errorf("the reason was not carried through: %q", files[0].Decision.Reason)
	}
}

func TestFilesByDecisionTreatsAnEmptyFilterAsAny(t *testing.T) {
	s := decided(t)
	ctx := context.Background()

	all, err := s.FilesByDecision(ctx, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("no filter returned %d files, want all 5", len(all))
	}

	none, err := s.FilesByDecision(ctx, decide.ActionNone, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 3 {
		t.Errorf("action=none returned %d files, want 3", len(none))
	}
}

func TestNotesAreFindableWithoutReadingEveryFile(t *testing.T) {
	s := decided(t)
	ctx := context.Background()

	files, err := s.FilesWithNotes(ctx, 0)
	if err != nil {
		t.Fatalf("FilesWithNotes: %v", err)
	}
	if len(files) != 1 || files[0].Path != "/m/one-track.mkv" {
		t.Fatalf("got %+v, want only the file whose last audio track was kept", files)
	}

	nFiles, nJobs, err := s.NoteCounts(ctx)
	if err != nil {
		t.Fatalf("NoteCounts: %v", err)
	}
	if nFiles != 1 || nJobs != 0 {
		t.Errorf("note counts = %d files, %d jobs; want 1, 0", nFiles, nJobs)
	}
}

func TestPendingCountMatchesTheListing(t *testing.T) {
	s := decided(t)
	ctx := context.Background()

	n, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	files, err := s.PendingFiles(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(files) {
		t.Errorf("PendingCount = %d but PendingFiles listed %d", n, len(files))
	}
	if n != 2 {
		t.Errorf("outstanding = %d, want the encode and the remux", n)
	}
}

// A file that is failing is not work you can do, so it must not be counted as
// outstanding -- otherwise the number on the dashboard never goes down and
// stops meaning anything.
func TestABlockedFileIsNotOutstanding(t *testing.T) {
	s := decided(t)
	ctx := context.Background()

	if err := s.RecordFailure(ctx, "/m/work.mkv", 4, t0.Add(-72*hour), "boom", t0); err != nil {
		t.Fatal(err)
	}
	n, err := s.PendingCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("outstanding = %d after one of the two failed, want 1", n)
	}
}

func TestJobsWithNotesFindsWorkWorthReadingAbout(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	quiet, err := s.StartJob(ctx, JobStart{Path: "/m/quiet.mkv", StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, quiet, JobFinish{FinishedAt: t0.Add(hour)}); err != nil {
		t.Fatal(err)
	}

	noisy, err := s.StartJob(ctx, JobStart{Path: "/m/noisy.mkv", StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, noisy, JobFinish{FinishedAt: t0.Add(hour),
		Notes: []string{"could not change the owner: not running as root"}}); err != nil {
		t.Fatal(err)
	}

	jobs, err := s.JobsWithNotes(ctx, 0)
	if err != nil {
		t.Fatalf("JobsWithNotes: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Path != "/m/noisy.mkv" {
		t.Fatalf("got %+v, want only the job with a note", jobs)
	}
}
