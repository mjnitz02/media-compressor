package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/runner"
)

func testConfig() *config.Config {
	std := decide.Standard()
	anime := decide.Anime()
	return &config.Config{
		Profiles: map[string]decide.Profile{"standard": std, "anime": anime},
		Libraries: []config.Library{
			{Name: "movies", ProfileName: "standard", Paths: []string{"/mnt/media_video/movies"}, Profile: std},
			{Name: "anime", ProfileName: "anime", Paths: []string{"/mnt/media_video/anime"}, Profile: anime},
		},
	}
}

// A path that belongs to no library has no profile, and guessing one would
// mean encoding a folder to some other library's quality target. Refusing is
// the only safe answer.
func TestAPathOutsideEveryLibraryIsRefused(t *testing.T) {
	_, err := resolveTargets(testConfig(), "", "", []string{"/etc"})
	if err == nil {
		t.Fatal("a path outside every library should not resolve")
	}
	if !strings.Contains(err.Error(), "-profile") {
		t.Errorf("the error should say how to proceed, got %v", err)
	}
}

func TestLibraryTargetCarriesItsOwnProfile(t *testing.T) {
	targets, err := resolveTargets(testConfig(), "anime", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if targets[0].Profile.Name != "anime" {
		t.Errorf("profile = %s, want anime", targets[0].Profile.Name)
	}
	if !strings.Contains(targets[0].Label, "library anime") {
		t.Errorf("label = %q, should name the library", targets[0].Label)
	}
}

func TestAnUnknownLibraryIsAnError(t *testing.T) {
	if _, err := resolveTargets(testConfig(), "films", "", nil); err == nil {
		t.Fatal("an undefined library should be an error, not an empty run")
	}
}

func TestLibraryAndPathsAreMutuallyExclusive(t *testing.T) {
	if _, err := resolveTargets(testConfig(), "movies", "", []string{"/mnt/media_video/movies"}); err == nil {
		t.Fatal("giving both -library and paths is ambiguous and should be refused")
	}
}

func TestAnUnknownProfileOverrideIsAnError(t *testing.T) {
	if _, err := resolveTargets(testConfig(), "movies", "nope", nil); err == nil {
		t.Fatal("an undefined -profile should be an error")
	}
}

// The summary has to make each outcome legible six months later, so the
// counts, the reasons and the renames all have to survive into the output.
func TestReportSummarisesEveryKindOfOutcome(t *testing.T) {
	rep := &report{title: "dry run: library movies", root: "/mnt/media_video/movies"}
	rep.add(runner.Outcome{
		Path: "/mnt/media_video/movies/Already HEVC.mkv",
		Plan: decide.Plan{Action: decide.ActionNone},
	})
	rep.add(runner.Outcome{
		Path: "/mnt/media_video/movies/Too Low.mkv",
		Plan: decide.Plan{
			Action: decide.ActionNone,
			Video:  decide.VideoSkippedFloor,
			Reason: "target bitrate 1200 kbps below floor 3000 kbps",
		},
	})
	rep.add(runner.Outcome{
		Path: "/mnt/media_video/movies/Wordy.mkv",
		Plan: decide.Plan{
			Action: decide.ActionRemux,
			Drops:  []decide.Drop{{Kind: "audio", TypeIdx: 1, Codec: "dts", Language: "fra", Reason: "language fra not in keep list"}},
			Notes:  []string{"kept audio track 0 even though it matches no keep rule"},
		},
		Job: &encode.Job{SourcePath: "/mnt/media_video/movies/Wordy.mkv", FinalPath: "/mnt/media_video/movies/Wordy.mkv"},
	})
	rep.add(runner.Outcome{
		Path: "/mnt/media_video/movies/Old.mp4",
		Plan: decide.Plan{Action: decide.ActionEncode, Video: decide.VideoEncode, TargetCodec: "hevc", Encoder: "hevc_vaapi",
			Bitrate: decide.Bitrate{SourceKbps: 9000, TargetKbps: 6000, MinKbps: 4200, MaxKbps: 7800}},
		Job: &encode.Job{SourcePath: "/mnt/media_video/movies/Old.mp4", FinalPath: "/mnt/media_video/movies/Old.mkv"},
	})
	rep.add(runner.Outcome{
		Path:     "/mnt/media_video/movies/Linked.mkv",
		Declined: "refusing: it is a symlink to /elsewhere/Linked.mkv",
	})
	rep.add(runner.Outcome{
		Path: "/mnt/media_video/movies/Broken.mkv",
		Err:  errors.New("ffprobe: Invalid data found"),
	})

	var buf bytes.Buffer
	rep.print(&buf, true)
	got := buf.String()

	for _, want := range []string{
		"6 files considered",
		"2  leave alone",
		"1  remux",
		"1  encode",
		"1  declined",
		"1  could not be read or processed",
		"1  of those, encode declined: target bitrate below the floor",
		// Paths are shown relative to the root being reported on.
		"Wordy.mkv",
		"drop audio:1 dts (fra) -- language fra not in keep list",
		"9000 kbps -> hevc 6000 kbps",
		"renames to Old.mkv",
		"renames (1)",
		"safety notes (1)",
		"symlink",
		"Invalid data found",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/mnt/media_video/movies/Wordy.mkv") {
		t.Errorf("paths under the root should be shortened:\n%s", got)
	}
}

// A multi-line ffprobe error must not read as several filenames.
func TestReportIndentsMultiLineReasons(t *testing.T) {
	rep := &report{title: "x"}
	rep.add(runner.Outcome{Path: "/lib/Broken.mkv", Err: errors.New("first line\nsecond line")})

	var buf bytes.Buffer
	rep.print(&buf, false)
	if !strings.Contains(buf.String(), "      second line") {
		t.Errorf("continuation lines should be indented:\n%s", buf.String())
	}
}

// A file that is not eligible yet is a first-class outcome, not an omission.
// On a dry run it is planned in full and flagged; on a real run it has no
// decision at all and must not be counted as one.
func TestReportSeparatesFilesThatAreNotEligibleYet(t *testing.T) {
	planned := &report{title: "dry run: library movies"}
	planned.add(runner.Outcome{
		Path:    "/lib/Arriving.mkv",
		Plan:    decide.Plan{Action: decide.ActionRemux},
		Waiting: "seen once; eligible when a later scan finds it unchanged",
		Job:     &encode.Job{SourcePath: "/lib/Arriving.mkv", FinalPath: "/lib/Arriving.mkv"},
	})

	var buf bytes.Buffer
	planned.print(&buf, true)
	got := buf.String()

	for _, want := range []string{
		"1  remux",
		"not eligible yet",
		"planned above anyway",
		"not yet: seen once",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run summary is missing %q:\n%s", want, got)
		}
	}

	// On a real run the same file was never probed, so there is no action to
	// report -- inventing one would be a decision nobody made.
	real := &report{title: "library movies"}
	real.add(runner.Outcome{
		Path:    "/lib/Arriving.mkv",
		Waiting: "seen once; eligible when a later scan finds it unchanged",
	})
	buf.Reset()
	real.print(&buf, false)
	got = buf.String()

	if strings.Contains(got, "leave alone") || strings.Contains(got, "remux") {
		t.Errorf("a file that was never decided was counted under an action:\n%s", got)
	}
	if !strings.Contains(got, "not eligible yet") {
		t.Errorf("summary is missing the waiting count:\n%s", got)
	}
}

// The cache is the reason a scan of 20,000 files takes seconds, so the report
// has to say how much of the answer came from it.
func TestReportSaysHowMuchCameFromTheCache(t *testing.T) {
	rep := &report{title: "library movies"}
	rep.add(runner.Outcome{Path: "/lib/a.mkv", Plan: decide.Plan{Action: decide.ActionNone}, FromCache: true})
	rep.add(runner.Outcome{Path: "/lib/b.mkv", Plan: decide.Plan{Action: decide.ActionNone}})

	var buf bytes.Buffer
	rep.print(&buf, false)
	if !strings.Contains(buf.String(), "1  answered from the last scan, not re-probed") {
		t.Errorf("summary should say what was not re-probed:\n%s", buf.String())
	}
}
