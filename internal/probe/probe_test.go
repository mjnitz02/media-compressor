package probe

import (
	"encoding/json"
	"math"
	"testing"
)

// A trimmed real ffprobe output: HEVC video, 5.1 EAC3, a PGS subtitle, an
// attached PNG poster and a font attachment. The poster is the interesting
// part -- see TestMainVideoSkipsCoverArt.
const sample = `{
  "streams": [
    {"index":0,"codec_name":"hevc","codec_type":"video","width":1920,"height":1080,
     "disposition":{"default":1,"attached_pic":0},"tags":{"language":"eng","BPS":"4200000"}},
    {"index":1,"codec_name":"eac3","codec_type":"audio","channels":6,
     "disposition":{"default":1},"tags":{"language":"eng","title":"Surround 5.1"}},
    {"index":2,"codec_name":"hdmv_pgs_subtitle","codec_type":"subtitle",
     "disposition":{"forced":1},"tags":{"language":"eng"}},
    {"index":3,"codec_name":"png","codec_type":"video","width":600,"height":900,
     "disposition":{"attached_pic":1}},
    {"index":4,"codec_name":"ttf","codec_type":"attachment",
     "disposition":{},"tags":{"filename":"font.ttf"}}
  ],
  "format": {"duration":"1376.502000","size":"863675607","bit_rate":"5021000",
             "format_name":"matroska,webm","nb_streams":5}
}`

func parseSample(t *testing.T) *Result {
	t.Helper()
	var r Result
	if err := json.Unmarshal([]byte(sample), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r.Path = "/mnt/media/movies/Example (2019)/Example.2019.Bluray-1080p.mkv"
	return &r
}

func TestAccessors(t *testing.T) {
	r := parseSample(t)

	if got := r.DurationSeconds(); got != 1376.502 {
		t.Errorf("DurationSeconds() = %v, want 1376.502", got)
	}
	if got := r.SizeBytes(); got != 863675607 {
		t.Errorf("SizeBytes() = %d, want 863675607", got)
	}
	// The bitrate math is a port of Tdarr's and works in binary megabytes.
	if got := r.SizeMiB(); got < 823.66 || got > 823.67 {
		t.Errorf("SizeMiB() = %v, want ~823.665", got)
	}
	if got := r.Container(); got != "mkv" {
		t.Errorf("Container() = %q, want %q", got, "mkv")
	}
	if got := len(r.StreamsOfType(TypeVideo)); got != 2 {
		t.Errorf("video streams = %d, want 2", got)
	}
	if got := len(r.StreamsOfType(TypeAudio)); got != 1 {
		t.Errorf("audio streams = %d, want 1", got)
	}
}

// Cover art must never be mistaken for the picture. The stack this project
// replaces did exactly that and would have re-encoded a poster as video.
func TestMainVideoSkipsCoverArt(t *testing.T) {
	r := parseSample(t)

	main, idx, ok := r.MainVideo()
	if !ok {
		t.Fatal("MainVideo() found nothing")
	}
	if main.CodecName != "hevc" || idx != 0 {
		t.Errorf("MainVideo() = %q at video index %d, want hevc at 0", main.CodecName, idx)
	}

	video := r.StreamsOfType(TypeVideo)
	if video[0].IsCoverArt() {
		t.Error("hevc picture classified as cover art")
	}
	if !video[1].IsCoverArt() {
		t.Error("attached png not classified as cover art")
	}
}

func TestMainVideoOnAudioOnlyFile(t *testing.T) {
	r := &Result{Path: "x.mkv", Streams: []Stream{{Index: 0, CodecName: "flac", CodecType: TypeAudio}}}
	if _, _, ok := r.MainVideo(); ok {
		t.Error("MainVideo() found a video stream in an audio-only file")
	}
}

func TestCoverArtByCodecWithoutDisposition(t *testing.T) {
	// Some muxers omit the attached_pic disposition entirely, leaving the
	// codec as the only clue.
	s := Stream{CodecName: "mjpeg", CodecType: TypeVideo}
	if !s.IsCoverArt() {
		t.Error("mjpeg without disposition should still be cover art")
	}
}

func TestLanguageAndTitle(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		lang string
	}{
		{"lowercase key", map[string]string{"language": "JPN"}, "jpn"},
		{"uppercase key", map[string]string{"LANGUAGE": "eng"}, "eng"},
		{"untagged", nil, ""},
		{"empty value", map[string]string{"language": "  "}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Stream{Tags: tc.tags}
			if got := s.Language(); got != tc.lang {
				t.Errorf("Language() = %q, want %q", got, tc.lang)
			}
		})
	}
}

func TestDurationFallsBackToStream(t *testing.T) {
	r := &Result{
		Path:    "x.mkv",
		Format:  Format{Duration: "N/A"},
		Streams: []Stream{{CodecType: TypeVideo, Duration: "42.5"}},
	}
	if got := r.DurationSeconds(); got != 42.5 {
		t.Errorf("DurationSeconds() = %v, want 42.5 from the stream", got)
	}
}

// ffprobe reports frame rates as rationals, and NTSC rates are only exact in
// that form: 24000/1001 is 23.976..., which no decimal literal spells.
func TestFrameRate(t *testing.T) {
	cases := []struct {
		name string
		avg  string
		r    string
		want float64
	}{
		{"ntsc rational", "24000/1001", "24000/1001", 24000.0 / 1001.0},
		{"whole rational", "25/1", "25/1", 25},
		{"plain decimal", "29.97", "", 29.97},
		{"falls back to r_frame_rate", "0/0", "24/1", 24},
		{"none at all", "0/0", "0/0", 0},
		{"malformed", "not/a/rate", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Result{
				Path:    "x.mkv",
				Streams: []Stream{{CodecType: TypeVideo, AvgFrameRate: c.avg, RFrameRate: c.r}},
			}
			if got := r.FrameRate(); math.Abs(got-c.want) > 1e-9 {
				t.Errorf("FrameRate() = %v, want %v", got, c.want)
			}
		})
	}
}

// Progress derivation asks for a frame rate on every encode, including the
// audio-only files the scanner can hand it.
func TestFrameRateWithNoVideoStream(t *testing.T) {
	r := &Result{Path: "x.mka", Streams: []Stream{{CodecType: TypeAudio}}}
	if got := r.FrameRate(); got != 0 {
		t.Errorf("FrameRate() = %v, want 0", got)
	}
}

func TestMalformedFieldsAreZero(t *testing.T) {
	// ffprobe writes "N/A" rather than omitting fields it cannot determine.
	// Those must degrade to zero, not break the probe.
	r := &Result{Path: "x.mkv", Format: Format{Duration: "N/A", Size: "N/A"}}
	if got := r.DurationSeconds(); got != 0 {
		t.Errorf("DurationSeconds() = %v, want 0", got)
	}
	if got := r.SizeBytes(); got != 0 {
		t.Errorf("SizeBytes() = %d, want 0", got)
	}
}

func TestContainerEdgeCases(t *testing.T) {
	cases := map[string]string{
		"/a/b/file.MKV":      "mkv",
		"/a/b/file.mp4":      "mp4",
		"/a/b.dir/file":      "",
		"/a/b/trailing.":     "",
		"/a/b/two.dots.m2ts": "m2ts",
	}
	for path, want := range cases {
		r := &Result{Path: path}
		if got := r.Container(); got != want {
			t.Errorf("Container(%q) = %q, want %q", path, got, want)
		}
	}
}
