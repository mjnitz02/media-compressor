package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
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
	if targets[0].profile.Name != "anime" {
		t.Errorf("profile = %s, want anime", targets[0].profile.Name)
	}
	if !strings.Contains(targets[0].label, "library anime") {
		t.Errorf("label = %q, should name the library", targets[0].label)
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
	rep.add(outcome{
		path: "/mnt/media_video/movies/Already HEVC.mkv",
		plan: decide.Plan{Action: decide.ActionNone},
	})
	rep.add(outcome{
		path: "/mnt/media_video/movies/Too Low.mkv",
		plan: decide.Plan{
			Action: decide.ActionNone,
			Video:  decide.VideoSkippedFloor,
			Reason: "target bitrate 1200 kbps below floor 3000 kbps",
		},
	})
	rep.add(outcome{
		path: "/mnt/media_video/movies/Wordy.mkv",
		plan: decide.Plan{
			Action: decide.ActionRemux,
			Drops:  []decide.Drop{{Kind: "audio", TypeIdx: 1, Codec: "dts", Language: "fra", Reason: "language fra not in keep list"}},
			Notes:  []string{"kept audio track 0 even though it matches no keep rule"},
		},
		job: &encode.Job{SourcePath: "/mnt/media_video/movies/Wordy.mkv", FinalPath: "/mnt/media_video/movies/Wordy.mkv"},
	})
	rep.add(outcome{
		path: "/mnt/media_video/movies/Old.mp4",
		plan: decide.Plan{Action: decide.ActionEncode, Video: decide.VideoEncode, TargetCodec: "hevc", Encoder: "hevc_vaapi",
			Bitrate: decide.Bitrate{SourceKbps: 9000, TargetKbps: 6000, MinKbps: 4200, MaxKbps: 7800}},
		job: &encode.Job{SourcePath: "/mnt/media_video/movies/Old.mp4", FinalPath: "/mnt/media_video/movies/Old.mkv"},
	})
	rep.add(outcome{
		path:     "/mnt/media_video/movies/Linked.mkv",
		declined: "refusing: it is a symlink to /elsewhere/Linked.mkv",
	})
	rep.add(outcome{
		path: "/mnt/media_video/movies/Broken.mkv",
		err:  errors.New("ffprobe: Invalid data found"),
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
	rep.add(outcome{path: "/lib/Broken.mkv", err: errors.New("first line\nsecond line")})

	var buf bytes.Buffer
	rep.print(&buf, false)
	if !strings.Contains(buf.String(), "      second line") {
		t.Errorf("continuation lines should be indented:\n%s", buf.String())
	}
}
