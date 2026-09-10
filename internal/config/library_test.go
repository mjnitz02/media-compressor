package config

import (
	"path/filepath"
	"testing"
)

// loadExample loads the shipped config.example.yaml, which is the file an
// operator starts from.
func loadExample(t *testing.T) *Config {
	t.Helper()
	c, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml does not parse: %v", err)
	}
	return c
}

// A file found on disk has to be matched back to the profile that governs it.
// Getting this wrong means a library encoded with another library's quality
// settings, silently.
func TestLibraryForMatchesOnPathBoundaries(t *testing.T) {
	cfg := &Config{Libraries: []Library{
		{Name: "tv", Paths: []string{"/mnt/media_video/tv"}},
		{Name: "animated", Paths: []string{"/mnt/media_video/tv_animated"}},
		{Name: "isolated", Paths: []string{"/mnt/media_isolated/movies", "/mnt/media_isolated/tv"}},
	}}

	cases := []struct {
		path string
		want string
	}{
		{"/mnt/media_video/tv/Show/S01E01.mkv", "tv"},
		{"/mnt/media_video/tv", "tv"},
		// The separator check is the whole point: tv must not swallow
		// tv_animated, which is a real pair of folders in this library.
		{"/mnt/media_video/tv_animated/Show/S01E01.mkv", "animated"},
		{"/mnt/media_isolated/tv/Show/S01E01.mkv", "isolated"},
		{"/mnt/media_video/movies/Film.mkv", ""},
		{"/mnt/media_video/tv_other/Film.mkv", ""},
	}

	for _, c := range cases {
		lib, ok := cfg.LibraryFor(c.path)
		switch {
		case c.want == "" && ok:
			t.Errorf("LibraryFor(%q) = %s, want no match", c.path, lib.Name)
		case c.want != "" && !ok:
			t.Errorf("LibraryFor(%q) found nothing, want %s", c.path, c.want)
		case c.want != "" && lib.Name != c.want:
			t.Errorf("LibraryFor(%q) = %s, want %s", c.path, lib.Name, c.want)
		}
	}
}

// Overlapping library paths are rejected by validation, so this can never
// arise from a real config. The longest match still wins, so that the answer
// does not depend on the order libraries happen to be written in.
func TestLibraryForPrefersTheMoreSpecificPath(t *testing.T) {
	cfg := &Config{Libraries: []Library{
		{Name: "all", Paths: []string{"/mnt/media_video"}},
		{Name: "anime", Paths: []string{"/mnt/media_video/anime"}},
	}}
	if lib, ok := cfg.LibraryFor("/mnt/media_video/anime/Show/E01.mkv"); !ok || lib.Name != "anime" {
		t.Errorf("got %v, want anime", lib)
	}
}

// The example config's own libraries have to resolve, since that is the file
// an operator starts from.
func TestExampleConfigLibrariesResolveTheirOwnPaths(t *testing.T) {
	cfg := loadExample(t)
	for _, lib := range cfg.Libraries {
		for _, path := range lib.Paths {
			got, ok := cfg.LibraryFor(path + "/Some File.mkv")
			if !ok {
				t.Errorf("%s: no library owns %s", lib.Name, path)
				continue
			}
			if got.Name != lib.Name {
				t.Errorf("%s under %s resolved to library %s", path, lib.Name, got.Name)
			}
		}
	}
}
