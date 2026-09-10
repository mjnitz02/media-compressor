package scan

import "testing"

// The example config ships `**/.Recycle.Bin/**`, and path/filepath.Match does
// not understand `**` at all. Getting this wrong means an ignore rule that
// silently never fires, which is the failure mode this project keeps trying
// to design out: no error, no output, just a share full of deleted files
// being re-encoded.
func TestMatchUnderstandsDoubleStar(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/.Recycle.Bin/**", "/mnt/media_video/movies/.Recycle.Bin/Old.mkv", true},
		{"**/.Recycle.Bin/**", "/mnt/media_video/movies/.Recycle.Bin/2019/Old.mkv", true},
		{"**/.Recycle.Bin/**", "/mnt/.Recycle.Bin/Old.mkv", true},
		// The directory itself matches, which is what lets the walk prune it
		// instead of descending into it.
		{"**/.Recycle.Bin/**", "/mnt/media_video/movies/.Recycle.Bin", true},
		{"**/.Recycle.Bin/**", "/mnt/media_video/movies/Recycle.Bin/Old.mkv", false},
		{"**/.Recycle.Bin/**", "/mnt/media_video/movies/Film.mkv", false},

		{"**/@eaDir/**", "/mnt/media_video/tv/Show/@eaDir/thumb.mkv", true},
		{"**/@eaDir/**", "/mnt/media_video/tv/Show/eaDir/thumb.mkv", false},

		// A pattern with no separator is matched against the base name, which
		// is what anyone writing this means.
		{"*.partial.*", "/mnt/media_video/movies/Film.partial.mkv", true},
		{"*.partial.*", "/mnt/media_video/movies/Film.mkv", false},

		// A single star never crosses a separator.
		{"/mnt/*/movies", "/mnt/media_video/movies", true},
		{"/mnt/*", "/mnt/media_video/movies", false},

		{"", "/anything", false},
	}

	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// The temp files the encoder writes are named to be caught by the shipped
// ignore globs. If that ever stops being true, a crashed encode leaves a
// partial file that the next scan treats as a new arrival -- and then plans,
// encodes and replaces.
func TestShippedGlobsCatchOurOwnTempFiles(t *testing.T) {
	globs := []string{"**/.Recycle.Bin/**", "**/@eaDir/**", "**/*.partial.*"}
	temp := "/mnt/media_video/movies/.Blade Runner.a1b2c3d4.mediacompressor.partial.mkv"
	if _, ok := MatchAny(globs, temp); !ok {
		t.Fatalf("no shipped ignore glob matches the encoder's temp file %s", temp)
	}
}

func TestValidPattern(t *testing.T) {
	for _, good := range []string{"**/.Recycle.Bin/**", "*.mkv", "**/*.partial.*", "[a-z]*.mkv"} {
		if !ValidPattern(good) {
			t.Errorf("ValidPattern(%q) = false, want true", good)
		}
	}
	if ValidPattern("**/[a-.mkv") {
		t.Error("ValidPattern accepted an unterminated character class")
	}
}
