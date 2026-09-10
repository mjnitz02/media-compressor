package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// Tests generate their own clips with ffmpeg's testsrc and never touch a real
// library file. Anything that needs ffmpeg skips when it is absent, so the
// suite still runs on a machine that only has Go.

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// makeClip writes a two-second clip with one video and two audio streams, the
// second tagged as commentary, so a probe of it exercises the accessors.
func makeClip(t *testing.T, name string) string {
	t.Helper()
	requireFFmpeg(t)

	path := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-metadata:s:a:0", "language=eng",
		"-metadata:s:a:1", "language=eng",
		"-metadata:s:a:1", "title=Director Commentary",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	return path
}

func TestProberOnGeneratedClip(t *testing.T) {
	path := makeClip(t, "clip.mkv")

	r, err := Prober{}.Run(context.Background(), path)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if r.Path != path {
		t.Errorf("Path = %q, want %q", r.Path, path)
	}
	if got := r.Container(); got != "mkv" {
		t.Errorf("Container() = %q, want mkv", got)
	}
	if d := r.DurationSeconds(); d < 1.5 || d > 2.5 {
		t.Errorf("DurationSeconds() = %v, want about 2", d)
	}
	if r.SizeBytes() <= 0 {
		t.Error("SizeBytes() = 0 for a file that exists")
	}

	main, _, ok := r.MainVideo()
	if !ok {
		t.Fatal("MainVideo() found nothing")
	}
	if main.CodecName != "h264" {
		t.Errorf("main video codec = %q, want h264", main.CodecName)
	}

	audio := r.StreamsOfType(TypeAudio)
	if len(audio) != 2 {
		t.Fatalf("audio streams = %d, want 2", len(audio))
	}
	if got := audio[0].Language(); got != "eng" {
		t.Errorf("audio 0 language = %q, want eng", got)
	}
	if got := audio[1].Title(); got != "Director Commentary" {
		t.Errorf("audio 1 title = %q, want the commentary title", got)
	}
}

func TestProberOnMissingFile(t *testing.T) {
	requireFFmpeg(t)

	_, err := (Prober{}).Run(context.Background(), filepath.Join(t.TempDir(), "nope.mkv"))
	if err == nil {
		t.Fatal("Run on a missing file returned no error")
	}
}

// A file whose name starts with a dash must not be read as an option.
func TestProberOnAwkwardFilename(t *testing.T) {
	path := makeClip(t, "-weird name'.mkv")

	if _, err := (Prober{}).Run(context.Background(), path); err != nil {
		t.Errorf("Run on %q: %v", path, err)
	}
}

func TestProberOnNonMedia(t *testing.T) {
	requireFFmpeg(t)

	path := filepath.Join(t.TempDir(), "notmedia.mkv")
	if err := writeFile(path, "this is not a video"); err != nil {
		t.Fatal(err)
	}
	if _, err := (Prober{}).Run(context.Background(), path); err == nil {
		t.Error("Run on a text file returned no error")
	}
}

func TestProberRespectsContextCancellation(t *testing.T) {
	requireFFmpeg(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (Prober{}).Run(ctx, "whatever.mkv"); err == nil {
		t.Error("Run with a cancelled context returned no error")
	}
}

func TestProberReportsMissingBinary(t *testing.T) {
	_, err := Prober{Binary: "ffprobe-does-not-exist"}.Run(context.Background(), "x.mkv")
	if err == nil {
		t.Fatal("Run with a bogus binary returned no error")
	}
}
