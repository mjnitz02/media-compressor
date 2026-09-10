package config

import "testing"

// The checks worth having are the ones for mistakes that would otherwise
// never announce themselves. Each test here names the failure it prevents.

// A forgotten floor would send several thousand files into an encode the old
// stack declined -- the one setting that is the reason its output always
// looked good.
func TestBitrateFloorMustBeStatedExplicitly(t *testing.T) {
	wantProblem(t, replace(t, "        floor_kbps: 3000\n", ""), "floor_kbps")

	// Zero is a legitimate answer, it just has to be written down.
	c := mustParse(t, replace(t, "floor_kbps: 3000", "floor_kbps: 0"))
	if got := c.Profiles["standard"].Video.Bitrate.FloorKbps; got != 0 {
		t.Errorf("floor = %d, want 0", got)
	}
}

// The engine takes the first tier the source bitrate reaches, scanning in
// order, so a mis-ordered table quietly applies the wrong divisor.
func TestTiersMustBeOrderedHighestFirst(t *testing.T) {
	src := replace(t, "          - { above: 3000, divisor: 1.5 }\n          - { above: 0, divisor: 1.0 }",
		"          - { above: 0, divisor: 1.0 }\n          - { above: 3000, divisor: 1.5 }")
	wantProblem(t, src, "ordered")
}

func TestTiersMustEndAtZero(t *testing.T) {
	src := replace(t, "          - { above: 0, divisor: 1.0 }\n", "")
	wantProblem(t, src, "above: 0")
}

func TestDivisorBelowOneWouldRaiseTheBitrate(t *testing.T) {
	src := replace(t, "{ above: 3000, divisor: 1.5 }", "{ above: 3000, divisor: 0.5 }")
	wantProblem(t, src, "raise the bitrate")
}

// Without the target codec in leave_alone, every file this tool converts
// becomes a candidate again on the next scan, forever.
func TestLeaveAloneMustContainTheTargetCodec(t *testing.T) {
	wantProblem(t, replace(t, "leave_alone: [hevc, av1]", "leave_alone: [av1]"), "leave_alone")
	wantProblem(t, replace(t, "      leave_alone: [hevc, av1]\n", ""), "leave_alone")
}

func TestHardwareEncoderNeedsADevice(t *testing.T) {
	wantProblem(t, replace(t, "encoder: libx265", "encoder: hevc_vaapi"), "video.device")

	// A software encoder has no render node to point at.
	mustParse(t, minimal)
}

// A misspelled language code does not fail, it just stops matching -- and you
// find out months later when a film has no English audio.
func TestLanguageCodeTyposAreCaught(t *testing.T) {
	wantProblem(t, replace(t, "keep_languages: [eng]\n      primary_languages: [eng]",
		"keep_languages: [english]\n      primary_languages: [english]"), "language code")
	wantProblem(t, replace(t, "      multichannel_to: ac3", "      tag_untagged_as: englsh\n      multichannel_to: ac3"), "language code")
}

// Naming a language as one the library is built around while also dropping it
// is a contradiction, and it silently produces files with no audio in the
// language you care about.
func TestPrimaryLanguageMustAlsoBeKept(t *testing.T) {
	src := replace(t, "      keep_languages: [eng]\n      primary_languages: [eng]",
		"      keep_languages: [eng]\n      primary_languages: [jpn]")
	wantProblem(t, src, "primary_languages")
}

func TestEmptyAudioKeepListIsAnError(t *testing.T) {
	src := replace(t, "      keep_languages: [eng]\n      primary_languages: [eng]", "      primary_languages: [eng]")
	wantProblem(t, src, "audio.keep_languages")
}

// Two libraries covering the same tree means one file has two profiles, and
// which one wins comes down to scan order.
func TestOverlappingLibraryPathsAreAnError(t *testing.T) {
	src := replace(t, "  - name: movies\n    paths: [/mnt/movies]",
		"  - name: movies\n    paths: [/mnt/movies]\n  - name: other\n    paths: [/mnt/movies/4k]")
	wantProblem(t, src, "two profiles")

	// The same trap inside one library just scans everything twice.
	src2 := replace(t, "paths: [/mnt/movies]", "paths: [/mnt/movies, /mnt/movies/4k]")
	wantProblem(t, src2, "scanned twice")
}

// A sibling directory whose name merely starts with another's is not nested.
func TestSimilarlyNamedPathsDoNotCountAsOverlapping(t *testing.T) {
	src := replace(t, "  - name: movies\n    paths: [/mnt/movies]",
		"  - name: movies\n    paths: [/mnt/movies]\n  - name: movies4k\n    paths: [/mnt/movies_4k]")
	mustParse(t, src)
}

func TestRelativeLibraryPathIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "paths: [/mnt/movies]", "paths: [movies]"), "absolute")
}

func TestDuplicateLibraryNameIsAnError(t *testing.T) {
	src := replace(t, "  - name: movies\n    paths: [/mnt/movies]",
		"  - name: movies\n    paths: [/mnt/movies]\n  - name: movies\n    paths: [/mnt/other]")
	wantProblem(t, src, "defined twice")
}

func TestLibraryWithNoPathsIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "    paths: [/mnt/movies]", "    paths: []"), "no paths")
}

func TestBadContainerNameIsAnError(t *testing.T) {
	wantProblem(t, replace(t, "  container: mkv", "  container: mkvv"), "defaults.container")

	src := replace(t, "  - name: movies", "  - name: movies\n    container: matroska")
	wantProblem(t, src, "matroska")
}

func TestNoLibrariesIsAnError(t *testing.T) {
	src := replace(t, "libraries:\n  - name: movies\n    paths: [/mnt/movies]\n", "libraries: []\n")
	wantProblem(t, src, "nothing to do")
}

func TestScannerExtensionsAreCheckedForShape(t *testing.T) {
	wantProblem(t, replace(t, "extensions: [mkv]", "extensions: [.mkv]"), "leading dot")
	wantProblem(t, replace(t, "extensions: [mkv]", "extensions: [MKV]"), "lowercase")
	wantProblem(t, replace(t, "  extensions: [mkv]\n", ""), "scanner.extensions")
}

func TestWorkerCountsMustBeAtLeastOne(t *testing.T) {
	src := replace(t, "  container: mkv", "  container: mkv\n  workers:\n    remux: 0\n    encode: 1")
	// A zero would be read as "unset" and defaulted, so this is really
	// checking that an explicit -1 cannot wedge the pools.
	mustParse(t, src)
	src = replace(t, "  container: mkv", "  container: mkv\n  workers:\n    remux: -1\n    encode: 1")
	wantProblem(t, src, "workers.remux")
}

func TestRequiredPathsAreChecked(t *testing.T) {
	wantProblem(t, replace(t, "  work_dir: /temp\n", ""), "work_dir")
	wantProblem(t, replace(t, "  database: /config/db.sqlite\n", ""), "database")
}

func TestPathContains(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/mnt/movies", "/mnt/movies", true},
		{"/mnt/movies", "/mnt/movies/4k", true},
		{"/mnt/movies", "/mnt/movies_4k", false},
		{"/mnt/movies/4k", "/mnt/movies", false},
		{"/mnt", "/mnt/a/b/c", true},
	}
	for _, c := range cases {
		if got := pathContains(c.parent, c.child); got != c.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", c.parent, c.child, got, c.want)
		}
	}
}
