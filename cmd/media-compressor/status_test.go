package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// status is the audit surface until the web UI exists, and it is written for
// somebody who last looked six months ago. Every number it prints has to be
// one you could act on.
func TestStatusReportsWhatIsOutstandingAndWhatWentWrong(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mtime := now.Add(-48 * time.Hour)
	if _, err := db.Observe(ctx, []store.Sighting{
		{Path: "/lib/done.mkv", Size: 1, ModTime: mtime},
		{Path: "/lib/pending.mkv", Size: 2, ModTime: mtime},
		{Path: "/lib/too-low.mkv", Size: 3, ModTime: mtime},
		{Path: "/lib/broken.mkv", Size: 4, ModTime: mtime},
	}, now); err != nil {
		t.Fatal(err)
	}

	record := func(path string, size int64, a decide.Action, v decide.VideoDecision) {
		if err := db.RecordDecision(ctx, path, size, mtime, store.Decision{
			Fingerprint: "f", DecidedAt: now, Action: a, Video: v,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("/lib/done.mkv", 1, decide.ActionNone, decide.VideoCopy)
	record("/lib/pending.mkv", 2, decide.ActionEncode, decide.VideoEncode)
	record("/lib/too-low.mkv", 3, decide.ActionNone, decide.VideoSkippedFloor)

	if err := db.RecordFailure(ctx, "/lib/broken.mkv", 4, mtime,
		"the re-encode was larger than the source", now); err != nil {
		t.Fatal(err)
	}

	id, err := db.StartJob(ctx, store.JobStart{
		Path: "/lib/history.mkv", Action: decide.ActionEncode,
		SizeBefore: 8 << 30, StartedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, id, store.JobFinish{
		FinishedAt: now, Elapsed: time.Hour, SizeAfter: 5 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	bad, err := db.StartJob(ctx, store.JobStart{Path: "/lib/broken.mkv", StartedAt: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, bad, store.JobFinish{
		FinishedAt: now, Err: errors.New("the re-encode was larger than the source"),
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := printStatus(ctx, &buf, db, "/config/state.db", 10, now); err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	for _, want := range []string{
		"4 files seen",
		"3 decided",
		"1 encode outstanding",
		"1 encode declined on the bitrate floor",
		"3.0 GiB reclaimed",
		"held back after a failure",
		"the re-encode was larger than the source",
		"history.mkv",
		"8.0 GiB -> 5.0 GiB in 1h0m0s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status is missing %q:\n%s", want, got)
		}
	}
}
