package encode

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/scan"
)

func TestTempFileGoesBesideTheSourceWithNoWorkDir(t *testing.T) {
	got, inWork := Encoder{}.tempPath("/mnt/media_video/movies/Film.mkv", decide.ContainerMKV)
	if inWork {
		t.Error("no work dir was configured, so none can have been used")
	}
	if filepath.Dir(got) != "/mnt/media_video/movies" {
		t.Errorf("temp dir = %s, want the source's directory", filepath.Dir(got))
	}
}

// The rule is entirely about the final rename being atomic, which it can only
// be within one filesystem. When the work dir is elsewhere -- or, as here,
// does not exist to be asked about -- the temp file goes beside the source.
func TestAnUnusableWorkDirFallsBackToTheSourceDirectory(t *testing.T) {
	e := Encoder{WorkDir: filepath.Join(t.TempDir(), "does-not-exist")}
	got, inWork := e.tempPath("/mnt/media_video/movies/Film.mkv", decide.ContainerMKV)
	if inWork {
		t.Error("a work dir that cannot be stat'd must not be used")
	}
	if filepath.Dir(got) != "/mnt/media_video/movies" {
		t.Errorf("temp dir = %s, want the source's directory", filepath.Dir(got))
	}
}

func TestAWorkDirOnTheSameFilesystemIsUsed(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir() // same filesystem as dir, both being under TMPDIR
	if !sameFilesystem(dir, work) {
		t.Skip("the two temp dirs are not on one filesystem")
	}

	e := Encoder{WorkDir: work}
	got, inWork := e.tempPath(filepath.Join(dir, "Film.mkv"), decide.ContainerMKV)
	if !inWork {
		t.Fatal("a work dir on the same filesystem should be used")
	}
	if filepath.Dir(got) != work {
		t.Errorf("temp dir = %s, want %s", filepath.Dir(got), work)
	}
}

func TestTempNameProperties(t *testing.T) {
	name := filepath.Base(tempName("/mnt/media_video/movies/Some Film (1982).mkv", ".mkv"))

	// ffmpeg picks its muxer from the extension.
	if !strings.HasSuffix(name, ".mkv") {
		t.Errorf("%q should end in the target container's extension", name)
	}
	// Hidden, so a leftover is out of the way in a media folder.
	if !strings.HasPrefix(name, ".") {
		t.Errorf("%q should be a dotfile", name)
	}
	// And caught by the shipped ignore globs, so the next scan does not treat
	// a crashed run's leftover as a new arrival to encode.
	if !scan.Match("**/*.partial.*", "/mnt/media_video/movies/"+name) {
		t.Errorf("%q is not caught by the shipped ignore globs", name)
	}
}

// A shared work dir collects files from every library, and "S01E01.mkv" is
// not unique across a TV library. Two sources must never claim one temp path.
func TestTempNamesDoNotCollideAcrossDirectories(t *testing.T) {
	a := tempName("/mnt/media_video/tv/Show A/S01E01.mkv", ".mkv")
	b := tempName("/mnt/media_video/tv/Show B/S01E01.mkv", ".mkv")
	if filepath.Base(a) == filepath.Base(b) {
		t.Errorf("two different sources produced the same temp name %q", filepath.Base(a))
	}
}

// The same source always produces the same temp name, so a file left behind
// by a failure can be found from the source path alone.
func TestTempNamesAreStable(t *testing.T) {
	src := "/mnt/media_video/movies/Film.mkv"
	if tempName(src, ".mkv") != tempName(src, ".mkv") {
		t.Error("temp names should be deterministic")
	}
}

func TestTempNameKeepsLongUnicodeNamesValid(t *testing.T) {
	long := "/mnt/media_video/anime/" + strings.Repeat("鋼の錬金術師", 40) + ".mkv"
	name := filepath.Base(tempName(long, ".mkv"))
	if len(name) > 255 {
		t.Errorf("temp name is %d bytes, which most filesystems will reject", len(name))
	}
	for _, r := range name {
		if r == '�' {
			t.Error("truncation split a multi-byte character")
		}
	}
}
