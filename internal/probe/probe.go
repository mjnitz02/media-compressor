// Package probe holds the typed representation of an ffprobe result, plus the
// small amount of code that runs ffprobe to produce one.
//
// The types are deliberately dumb: ffprobe reports most numbers as JSON
// strings, and this package keeps them as strings and offers accessors that
// parse them. That way a missing or malformed field is a zero value at the
// point of use rather than an unmarshal error that throws away the whole
// probe.
package probe

import (
	"path/filepath"
	"strconv"
	"strings"
)

// Result is one probed file.
type Result struct {
	// Path is the file that was probed. decide never uses it for anything but
	// deriving the container from the extension, and must never treat it as
	// somewhere it may write.
	Path    string   `json:"-"`
	Format  Format   `json:"format"`
	Streams []Stream `json:"streams"`
}

// Format is ffprobe's `format` object, trimmed to the fields used here.
type Format struct {
	Duration   string `json:"duration"`
	Size       string `json:"size"`
	BitRate    string `json:"bit_rate"`
	FormatName string `json:"format_name"`
	NBStreams  int    `json:"nb_streams"`
}

// Stream is one ffprobe stream. Numeric-looking fields that ffprobe emits as
// strings are kept as strings; see the accessors below.
type Stream struct {
	Index         int               `json:"index"`
	CodecName     string            `json:"codec_name"`
	CodecType     string            `json:"codec_type"`
	Profile       string            `json:"profile"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	PixFmt        string            `json:"pix_fmt"`
	BitRate       string            `json:"bit_rate"`
	Channels      int               `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	SampleRate    string            `json:"sample_rate"`
	RFrameRate    string            `json:"r_frame_rate"`
	AvgFrameRate  string            `json:"avg_frame_rate"`
	Duration      string            `json:"duration"`
	Disposition   map[string]int    `json:"disposition"`
	Tags          map[string]string `json:"tags"`
}

// Stream type constants, matching ffprobe's `codec_type` values.
const (
	TypeVideo    = "video"
	TypeAudio    = "audio"
	TypeSubtitle = "subtitle"
	TypeData     = "data"
	TypeAttach   = "attachment"
)

// DurationSeconds is the file's duration, preferring the container-level value
// and falling back to the first stream that carries one.
//
// The old Tdarr plugin had a third fallback to a mediainfo-derived
// `meta.Duration`. This project only ever has ffprobe output, and across all
// 1,881 corpus fixtures format.duration was present and positive, so that
// branch would never have fired anyway.
func (r *Result) DurationSeconds() float64 {
	if d := parseFloat(r.Format.Duration); d > 0 {
		return d
	}
	for i := range r.Streams {
		if d := parseFloat(r.Streams[i].Duration); d > 0 {
			return d
		}
	}
	return 0
}

// FrameRate is the main video stream's frame rate in frames per second, or 0
// when there is no video stream or ffprobe reported none.
//
// It exists so that encode progress can be derived from ffmpeg's frame counter
// when its out_time is unavailable -- which is what happens whenever
// attachments are mapped. See internal/encode/progress.go.
func (r *Result) FrameRate() float64 {
	s, _, ok := r.MainVideo()
	if !ok {
		return 0
	}
	if f := parseRational(s.AvgFrameRate); f > 0 {
		return f
	}
	return parseRational(s.RFrameRate)
}

// SizeBytes is the file size as ffprobe reported it.
func (r *Result) SizeBytes() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(r.Format.Size), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// SizeMiB is the file size in binary megabytes.
//
// This exists because the bitrate math it feeds is a port of Tdarr's, and
// Tdarr worked in MiB. See decide.SourceBitrateKbps.
func (r *Result) SizeMiB() float64 {
	return float64(r.SizeBytes()) / (1024 * 1024)
}

// Container is the lowercase extension of Path, without the dot ("mkv").
//
// It deliberately looks only at the base name. A media root like
// /mnt/media/movies/Blade.Runner.1982 has dots in the directory name, and
// taking the last dot in the whole path would report a "container" of
// "Runner.1982/file" for an extensionless file.
func (r *Result) Container() string {
	ext := filepath.Ext(filepath.Base(r.Path))
	if len(ext) < 2 {
		return ""
	}
	return strings.ToLower(ext[1:])
}

// StreamsOfType returns the streams of one codec_type, in file order.
func (r *Result) StreamsOfType(codecType string) []Stream {
	// Go note: the `var out []Stream` zero value is a nil slice, which appends
	// and ranges just like an empty one. No need to pre-allocate.
	var out []Stream
	for _, s := range r.Streams {
		if strings.EqualFold(s.CodecType, codecType) {
			out = append(out, s)
		}
	}
	return out
}

// MainVideo returns the stream that is actually the picture, skipping cover
// art and thumbnails, plus its index among video streams.
//
// This matters more than it looks. The old stack looped over every video
// stream and treated each one as a candidate for encoding, so a file with a
// PNG poster attached could be selected for a video encode on the strength of
// the poster. Twelve such files are in the corpus. See docs/tdarr-analysis.md.
func (r *Result) MainVideo() (Stream, int, bool) {
	video := r.StreamsOfType(TypeVideo)
	for i, s := range video {
		if !s.IsCoverArt() {
			return s, i, true
		}
	}
	return Stream{}, 0, false
}

// IsCoverArt reports whether a video stream is embedded artwork rather than
// the picture: an attached_pic disposition, or a still-image codec.
func (s *Stream) IsCoverArt() bool {
	if s.Disposition["attached_pic"] == 1 || s.Disposition["timed_thumbnails"] == 1 {
		return true
	}
	switch strings.ToLower(s.CodecName) {
	case "mjpeg", "png", "bmp", "gif", "webp", "tiff", "ppm":
		return true
	}
	return false
}

// Language is the stream's lowercase 3-letter language tag, or "" if untagged.
func (s *Stream) Language() string {
	// ffprobe casing is inconsistent across muxers, so check both spellings.
	for _, k := range []string{"language", "LANGUAGE"} {
		if v, ok := s.Tags[k]; ok {
			v = strings.ToLower(strings.TrimSpace(v))
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// Title is the stream's title tag, or "".
func (s *Stream) Title() string {
	for _, k := range []string{"title", "TITLE"} {
		if v, ok := s.Tags[k]; ok {
			return v
		}
	}
	return ""
}

// Forced reports the forced disposition, which is a reason to keep a subtitle
// regardless of language.
func (s *Stream) Forced() bool { return s.Disposition["forced"] == 1 }

// parseRational parses the "numerator/denominator" form ffprobe uses for
// frame rates ("24000/1001"). A plain number is accepted too, and the "0/0"
// ffprobe gives for streams with no frame rate comes back as 0.
func parseRational(s string) float64 {
	num, den, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return parseFloat(s)
	}
	d := parseFloat(den)
	if d == 0 {
		return 0
	}
	return parseFloat(num) / d
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}
