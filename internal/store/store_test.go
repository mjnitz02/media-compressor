package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var (
	t0   = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	hour = time.Hour
)

func sight(path string, size int64, mtime time.Time) Sighting {
	return Sighting{Path: path, Size: size, ModTime: mtime}
}

func observe(t *testing.T, s *Store, now time.Time, seen ...Sighting) map[string]FileState {
	t.Helper()
	states, err := s.Observe(context.Background(), seen, now)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return states
}

func TestOpenIsIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	observe(t, s, t0, sight("/m/a.mkv", 10, t0))
	s.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	states, err := again.Lookup(context.Background(), []string{"/m/a.mkv"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := states["/m/a.mkv"]; !ok {
		t.Fatal("the row did not survive a reopen")
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.DB().Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	s.Close()

	// Downgrading the binary must not silently run against a schema it does
	// not understand; the failure mode of that is a corrupted decision cache.
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a database from a newer version")
	}
}

func TestFirstSightingIsNotSettled(t *testing.T) {
	s := open(t)
	states := observe(t, s, t0, sight("/m/a.mkv", 10, t0.Add(-24*hour)))

	f := states["/m/a.mkv"]
	if !f.New {
		t.Error("want New on a path the store had never seen")
	}
	if f.Sightings != 1 {
		t.Errorf("Sightings = %d, want 1", f.Sightings)
	}
	// The file is a day old, so age alone would say yes. One sighting is not
	// evidence that its bytes have stopped moving.
	if ok, why := f.Settled(hour, t0); ok {
		t.Error("a file seen once must not be settled")
	} else if why == "" {
		t.Error("want a reason a first sighting is not settled")
	}
}

func TestASecondUnchangedSightingSettlesTheFile(t *testing.T) {
	s := open(t)
	mtime := t0.Add(-24 * hour)
	observe(t, s, t0, sight("/m/a.mkv", 10, mtime))

	// Immediately afterwards it is still not settled: two scans a second
	// apart prove nothing about a file being copied.
	states := observe(t, s, t0.Add(time.Second), sight("/m/a.mkv", 10, mtime))
	if ok, _ := states["/m/a.mkv"].Settled(hour, t0.Add(time.Second)); ok {
		t.Error("two sightings a second apart must not settle a file")
	}

	later := t0.Add(3 * hour)
	states = observe(t, s, later, sight("/m/a.mkv", 10, mtime))
	f := states["/m/a.mkv"]
	if f.Sightings != 3 {
		t.Errorf("Sightings = %d, want 3", f.Sightings)
	}
	if ok, why := f.Settled(hour, later); !ok {
		t.Errorf("want settled after being unchanged for three hours, got %q", why)
	}
}

func TestAGrowingFileNeverSettles(t *testing.T) {
	s := open(t)
	// The case this rule exists for: a share-to-share move preserves the
	// mtime, so the file looks old the whole time it is being written.
	mtime := t0.Add(-24 * hour)
	now := t0
	for size := int64(10); size < 60; size += 10 {
		states := observe(t, s, now, sight("/m/a.mkv", size, mtime))
		if ok, _ := states["/m/a.mkv"].Settled(hour, now); ok {
			t.Fatalf("a file that is still growing settled at size %d", size)
		}
		now = now.Add(2 * hour)
	}
}

func TestARecentlyWrittenFileIsNotSettled(t *testing.T) {
	s := open(t)
	states := observe(t, s, t0, sight("/m/a.mkv", 10, t0.Add(-time.Minute)))
	observe(t, s, t0.Add(2*hour), sight("/m/a.mkv", 10, t0.Add(-time.Minute)))
	states = observe(t, s, t0.Add(4*hour), sight("/m/a.mkv", 10, t0.Add(-time.Minute)))

	if ok, _ := states["/m/a.mkv"].Settled(8*hour, t0.Add(4*hour)); ok {
		t.Error("a file younger than the grace period must not be settled")
	}
}

func TestChangingTheFileDiscardsEverythingKnownAboutIt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mtime := t0.Add(-24 * hour)
	observe(t, s, t0, sight("/m/a.mkv", 10, mtime))

	if err := s.RecordDecision(ctx, "/m/a.mkv", 10, mtime, Decision{
		Profile: "standard", Fingerprint: "abc", DecidedAt: t0,
		Action: decide.ActionNone, Video: decide.VideoCopy,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if err := s.RecordFailure(ctx, "/m/a.mkv", 10, mtime, "boom", t0); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	// An *arr stack upgrades the file. Every answer the store holds was about
	// the bytes that are now gone.
	states := observe(t, s, t0.Add(4*hour), sight("/m/a.mkv", 999, t0.Add(3*hour)))
	f := states["/m/a.mkv"]

	if _, ok := f.CachedDecision("abc"); ok {
		t.Error("the cached decision survived the file changing")
	}
	if f.Failures != 0 {
		t.Errorf("Failures = %d, want the failure history cleared by the change", f.Failures)
	}
	if f.Sightings != 1 {
		t.Errorf("Sightings = %d, want the settle clock restarted", f.Sightings)
	}
	if !f.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want it kept at %v", f.FirstSeen, t0)
	}
}

func TestADecisionIsOnlyACacheHitForTheProfileThatMadeIt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mtime := t0.Add(-24 * hour)
	observe(t, s, t0, sight("/m/a.mkv", 10, mtime))

	if err := s.RecordDecision(ctx, "/m/a.mkv", 10, mtime, Decision{
		Profile: "standard", Fingerprint: "fingerprint-1", DecidedAt: t0,
		Action: decide.ActionNone, Video: decide.VideoSkippedFloor,
		Reason: "2100 kbps below floor 3000", SourceKbps: 4200, TargetKbps: 2100,
		Notes: []string{"kept the last audio track"},
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	states := observe(t, s, t0.Add(4*hour), sight("/m/a.mkv", 10, mtime))
	f := states["/m/a.mkv"]

	d, ok := f.CachedDecision("fingerprint-1")
	if !ok {
		t.Fatal("want a cache hit for the profile that made the decision")
	}
	if d.Video != decide.VideoSkippedFloor || d.TargetKbps != 2100 {
		t.Errorf("decision came back as %+v", d)
	}
	if got := d.Plan().Reason; got != "2100 kbps below floor 3000" {
		t.Errorf("Plan().Reason = %q", got)
	}
	if len(d.Notes) != 1 || d.Notes[0] != "kept the last audio track" {
		t.Errorf("Notes = %v", d.Notes)
	}

	// Editing the floor in config.yaml changes the fingerprint, and every
	// cached answer has to be recomputed. Getting this wrong would make a
	// config change silently do nothing.
	if _, ok := f.CachedDecision("fingerprint-2"); ok {
		t.Error("a decision from a different profile was treated as a cache hit")
	}
}

func TestADecisionForStaleBytesIsNotStored(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mtime := t0.Add(-24 * hour)
	observe(t, s, t0, sight("/m/a.mkv", 10, mtime))

	// The probe was of a 10-byte file; by the time it finished the row says
	// 999. Attaching the answer anyway would cache a decision about bytes
	// that are gone.
	observe(t, s, t0.Add(hour), sight("/m/a.mkv", 999, t0.Add(hour)))
	if err := s.RecordDecision(ctx, "/m/a.mkv", 10, mtime, Decision{
		Fingerprint: "abc", DecidedAt: t0, Action: decide.ActionNone,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	states, err := s.Lookup(ctx, []string{"/m/a.mkv"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, ok := states["/m/a.mkv"].CachedDecision("abc"); ok {
		t.Error("a decision computed from bytes that have since changed was cached")
	}
}

func TestFailuresBackOffAndThenGiveUp(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mtime := t0.Add(-24 * hour)
	observe(t, s, t0, sight("/m/a.mkv", 10, mtime))

	read := func() FileState {
		states, err := s.Lookup(ctx, []string{"/m/a.mkv"})
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		return states["/m/a.mkv"]
	}

	// First failure: blocked for an hour, then eligible again.
	if err := s.RecordFailure(ctx, "/m/a.mkv", 10, mtime, "ffmpeg failed: no space left on device", t0); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	f := read()
	if blocked, why := f.Blocked(t0.Add(30 * time.Minute)); !blocked {
		t.Error("want blocked half an hour after failing")
	} else if why == "" {
		t.Error("want a reason")
	}
	if blocked, _ := f.Blocked(t0.Add(2 * hour)); blocked {
		t.Error("want the first failure retried after an hour")
	}

	// Failures two and three back off further; the fourth gives up.
	for i := 2; i <= 4; i++ {
		if err := s.RecordFailure(ctx, "/m/a.mkv", 10, mtime, "same failure again", t0); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
	}
	f = read()
	if f.Failures != 4 {
		t.Fatalf("Failures = %d, want 4", f.Failures)
	}
	if !f.RetryAfter.IsZero() {
		t.Error("want no automatic retry after four failures")
	}
	blocked, why := f.Blocked(t0.Add(365 * 24 * hour))
	if !blocked {
		t.Error("a file given up on stays blocked until somebody asks")
	}
	if !strings.Contains(why, "-retry-failed") {
		t.Errorf("the reason should say how to get past it, got %q", why)
	}

	if err := s.ClearFailures(ctx, "/m/a.mkv"); err != nil {
		t.Fatalf("ClearFailures: %v", err)
	}
	if blocked, _ := read().Blocked(t0); blocked {
		t.Error("ClearFailures did not unblock the file")
	}
}

func TestForgetRemovesAReplacedFile(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	observe(t, s, t0, sight("/m/a.mkv", 10, t0))

	if err := s.Forget(ctx, "/m/a.mkv", ""); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	states, err := s.Lookup(ctx, []string{"/m/a.mkv"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(states) != 0 {
		t.Error("the row survived Forget")
	}
}

func TestSweepOnlyTouchesTheRootsItWasGiven(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	// The underscore is the trap: it is LIKE's single-character wildcard, and
	// media roots are full of them. An unescaped prefix would make sweeping
	// /mnt/media_video delete rows for /mnt/media_isolated.
	observe(t, s, t0,
		sight("/mnt/media_video/a.mkv", 1, t0),
		sight("/mnt/media_video/gone.mkv", 1, t0),
		sight("/mnt/media_isolated/b.mkv", 1, t0),
		sight("/mnt/media-video/c.mkv", 1, t0),
	)

	later := t0.Add(4 * hour)
	observe(t, s, later, sight("/mnt/media_video/a.mkv", 1, t0))

	n, err := s.Sweep(ctx, []string{"/mnt/media_video"}, later)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d rows, want 1", n)
	}

	states, err := s.Lookup(ctx, []string{
		"/mnt/media_video/a.mkv", "/mnt/media_video/gone.mkv",
		"/mnt/media_isolated/b.mkv", "/mnt/media-video/c.mkv"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for _, want := range []string{"/mnt/media_video/a.mkv", "/mnt/media_isolated/b.mkv", "/mnt/media-video/c.mkv"} {
		if _, ok := states[want]; !ok {
			t.Errorf("Sweep removed %s, which it was not asked about", want)
		}
	}
	if _, ok := states["/mnt/media_video/gone.mkv"]; ok {
		t.Error("Sweep left a row for a file the scan no longer finds")
	}
}

func TestJobHistoryRecordsBothOutcomes(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id, err := s.StartJob(ctx, JobStart{
		Path: "/m/a.mkv", Library: "movies", Profile: "standard",
		Action: decide.ActionEncode, SizeBefore: 8_000_000_000, StartedAt: t0,
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if err := s.FinishJob(ctx, id, JobFinish{
		FinishedAt: t0.Add(40 * time.Minute), Elapsed: 40 * time.Minute,
		SizeAfter: 5_000_000_000, Notes: []string{"three hard links"},
	}); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	bad, err := s.StartJob(ctx, JobStart{Path: "/m/b.mkv", Action: decide.ActionRemux, StartedAt: t0})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if err := s.FinishJob(ctx, bad, JobFinish{
		FinishedAt: t0.Add(time.Minute), Err: errors.New("ffmpeg failed"), FFmpegTail: "some stderr",
	}); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	jobs, err := s.RecentJobs(ctx, 10)
	if err != nil {
		t.Fatalf("RecentJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}

	byPath := map[string]JobRecord{}
	for _, j := range jobs {
		byPath[j.Path] = j
	}
	if got := byPath["/m/a.mkv"]; got.Status != StatusDone || got.Saved() != 3_000_000_000 {
		t.Errorf("finished job: status %q saved %d", got.Status, got.Saved())
	}
	if got := byPath["/m/a.mkv"]; got.Elapsed != 40*time.Minute || len(got.Notes) != 1 {
		t.Errorf("finished job: elapsed %v notes %v", got.Elapsed, got.Notes)
	}
	if got := byPath["/m/b.mkv"]; got.Status != StatusFailed || got.Error != "ffmpeg failed" {
		t.Errorf("failed job: status %q error %q", got.Status, got.Error)
	}
	if got := byPath["/m/b.mkv"]; got.FFmpegTail != "some stderr" {
		t.Error("ffmpeg's output was not kept for the failure")
	}
	if got := byPath["/m/b.mkv"]; got.Saved() != 0 {
		t.Errorf("a failed job saved %d bytes", got.Saved())
	}
}

func TestJobsLeftRunningByACrashAreClosedOff(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.StartJob(ctx, JobStart{Path: "/m/a.mkv", StartedAt: t0}); err != nil {
		t.Fatalf("StartJob: %v", err)
	}

	n, err := s.AbandonRunningJobs(ctx, t0.Add(hour))
	if err != nil {
		t.Fatalf("AbandonRunningJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("closed %d jobs, want 1", n)
	}

	jobs, _ := s.RecentJobs(ctx, 10)
	if jobs[0].Status != StatusFailed {
		t.Errorf("status = %q, want the row closed off", jobs[0].Status)
	}
	if !strings.Contains(jobs[0].Error, "untouched") {
		t.Errorf("the message should say the original is safe, got %q", jobs[0].Error)
	}
}

func TestCountsSummarisesTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	mtime := t0.Add(-24 * hour)

	observe(t, s, t0,
		sight("/m/none.mkv", 1, mtime),
		sight("/m/remux.mkv", 2, mtime),
		sight("/m/encode.mkv", 3, mtime),
		sight("/m/floor.mkv", 4, mtime),
		sight("/m/broken.mkv", 5, mtime),
	)

	record := func(path string, size int64, a decide.Action, v decide.VideoDecision) {
		if err := s.RecordDecision(ctx, path, size, mtime, Decision{
			Fingerprint: "f", DecidedAt: t0, Action: a, Video: v,
		}); err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
	}
	record("/m/none.mkv", 1, decide.ActionNone, decide.VideoCopy)
	record("/m/remux.mkv", 2, decide.ActionRemux, decide.VideoCopy)
	record("/m/encode.mkv", 3, decide.ActionEncode, decide.VideoEncode)
	record("/m/floor.mkv", 4, decide.ActionNone, decide.VideoSkippedFloor)
	if err := s.RecordFailure(ctx, "/m/broken.mkv", 5, mtime, "output was larger than the source", t0); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	id, _ := s.StartJob(ctx, JobStart{Path: "/m/encode.mkv", SizeBefore: 100, StartedAt: t0})
	if err := s.FinishJob(ctx, id, JobFinish{FinishedAt: t0, SizeAfter: 60}); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if c.Files != 5 || c.Decided != 4 {
		t.Errorf("Files = %d, Decided = %d; want 5 and 4", c.Files, c.Decided)
	}
	if c.ByAction[decide.ActionNone] != 2 || c.ByAction[decide.ActionEncode] != 1 {
		t.Errorf("ByAction = %v", c.ByAction)
	}
	if c.SkippedFloor != 1 {
		t.Errorf("SkippedFloor = %d, want 1", c.SkippedFloor)
	}
	if c.Blocked != 1 {
		t.Errorf("Blocked = %d, want 1", c.Blocked)
	}
	if c.JobsDone != 1 || c.BytesSaved != 40 {
		t.Errorf("JobsDone = %d, BytesSaved = %d", c.JobsDone, c.BytesSaved)
	}

	blocked, err := s.BlockedFiles(ctx, 10)
	if err != nil {
		t.Fatalf("BlockedFiles: %v", err)
	}
	if len(blocked) != 1 || blocked[0].Path != "/m/broken.mkv" {
		t.Errorf("BlockedFiles = %+v", blocked)
	}

	pending, err := s.PendingFiles(ctx, 10)
	if err != nil {
		t.Fatalf("PendingFiles: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("PendingFiles returned %d, want the remux and the encode", len(pending))
	}
}

func TestProfileFingerprintTracksEveryQualityLever(t *testing.T) {
	base := decide.Standard()
	same := ProfileFingerprint(base)
	if same != ProfileFingerprint(decide.Standard()) {
		t.Fatal("the same profile hashed to two different fingerprints")
	}

	// Each of these is a change that must invalidate 20,000 cached answers.
	changes := map[string]func(p *decide.Profile){
		"the bitrate floor":  func(p *decide.Profile) { p.Video.Bitrate.FloorKbps = 2000 },
		"a tier divisor":     func(p *decide.Profile) { p.Video.Bitrate.Tiers[0].Divisor = 3 },
		"the encoder":        func(p *decide.Profile) { p.Video.Encoder = "libx265" },
		"the container":      func(p *decide.Profile) { p.Container = decide.ContainerSource },
		"the keep list":      func(p *decide.Profile) { p.Audio.KeepLanguages = []string{"eng"} },
		"leave_alone":        func(p *decide.Profile) { p.Video.LeaveAlone = []string{"hevc"} },
		"tagging untagged":   func(p *decide.Profile) { p.Audio.TagUntaggedAs = "" },
		"stripping chapters": func(p *decide.Profile) { p.Strip.Chapters = !p.Strip.Chapters },
	}
	for what, change := range changes {
		p := decide.Standard()
		change(&p)
		if ProfileFingerprint(p) == same {
			t.Errorf("changing %s did not change the fingerprint, so the cache would not be invalidated", what)
		}
	}

	if ProfileFingerprint(decide.Anime()) == same {
		t.Error("standard and anime share a fingerprint")
	}
}
