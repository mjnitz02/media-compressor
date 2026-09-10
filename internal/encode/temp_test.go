package encode

import (
	"os"
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

// The device-number comparison above is a prediction, and CanRenameInto is
// the same question asked by doing it. This is the case where they agree.
func TestCanRenameIntoASiblingDirectory(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	media := filepath.Join(root, "media")
	for _, d := range []string{work, media} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := CanRenameInto(work, media); err != nil {
		t.Fatalf("two directories in one temp dir should be renameable between: %v", err)
	}

	// The probe writes into a media directory, so the one thing it must never
	// do is leave anything in one.
	for _, d := range []string{work, media} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s still holds %d entries after the probe; the first is %s",
				d, len(entries), entries[0].Name())
		}
	}
}

// A work dir that is not there cannot be probed, and saying so is what makes
// the caller fall back to a temp file beside the source.
func TestCanRenameIntoReportsAnUnusableWorkDir(t *testing.T) {
	media := t.TempDir()
	err := CanRenameInto(filepath.Join(t.TempDir(), "does-not-exist"), media)
	if err == nil {
		t.Fatal("a work dir that does not exist should not report itself usable")
	}
	if entries, _ := os.ReadDir(media); len(entries) != 0 {
		t.Errorf("a failed probe left %d entries in the media directory", len(entries))
	}
}

// The destination refusing the rename is the container case: /temp and the
// media as two bind mounts of one filesystem, which report the same st_dev
// and still fail EXDEV. A missing destination stands in for it here, since a
// unit test cannot make a mount.
func TestCanRenameIntoReportsADestinationThatRefuses(t *testing.T) {
	work := t.TempDir()
	if err := CanRenameInto(work, filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("a destination that cannot be renamed into should not report itself usable")
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("a failed probe left %d entries in the work dir", len(entries))
	}
}
