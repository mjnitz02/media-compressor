package encode

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

// result builds a probe result with a given duration, size and stream layout.
// Verification is pure arithmetic over two of these and a plan, so every check
// below is exact and needs no ffmpeg.
func result(path string, seconds float64, size int64, videoCodec string, audio, subs int) *probe.Result {
	r := &probe.Result{
		Path: path,
		Format: probe.Format{
			Duration: fmt.Sprintf("%f", seconds),
			Size:     fmt.Sprintf("%d", size),
		},
	}
	if videoCodec != "" {
		r.Streams = append(r.Streams, probe.Stream{Index: 0, CodecType: probe.TypeVideo, CodecName: videoCodec})
	}
	for i := 0; i < audio; i++ {
		r.Streams = append(r.Streams, probe.Stream{CodecType: probe.TypeAudio, CodecName: "aac"})
	}
	for i := 0; i < subs; i++ {
		r.Streams = append(r.Streams, probe.Stream{CodecType: probe.TypeSubtitle, CodecName: "subrip"})
	}
	return r
}

func job(src *probe.Result, plan decide.Plan) *Job {
	return &Job{Plan: plan, Source: src, SourcePath: src.Path, FinalPath: src.Path}
}

func hasProblem(v Verification, substr string) bool {
	for _, p := range v.Problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func TestVerifyAcceptsAGoodRemux(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 3, 2)
	plan := decide.Plan{
		Action: decide.ActionRemux,
		Video:  decide.VideoCopy,
		Drops: []decide.Drop{
			{Kind: "audio", TypeIdx: 2, Reason: "language fra not in keep list"},
		},
	}
	out := result("/tmp/x.mkv", 3600.4, 7_400_000_000, "h264", 2, 2)

	v := verify(job(src, plan), out, nil, VerifyOptions{})
	if !v.OK {
		t.Fatalf("expected pass, got problems: %v", v.Problems)
	}
	if v.ExpectedStreams.Audio != 2 {
		t.Errorf("expected audio count = %d, want 2 (3 in the source, 1 dropped)", v.ExpectedStreams.Audio)
	}
}

// The output has to be probed successfully before anything else can be said
// about it, and a file that does not parse can never replace one that does.
func TestVerifyRejectsAnUnprobeableOutput(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), nil, errors.New("moov atom not found"), VerifyOptions{})
	if v.OK {
		t.Fatal("an output that does not ffprobe must never be accepted")
	}
	if !hasProblem(v, "moov atom not found") {
		t.Errorf("the ffprobe error should be reported verbatim, got %v", v.Problems)
	}
	if len(v.Problems) != 1 {
		t.Errorf("nothing else can be checked, so there should be one problem; got %v", v.Problems)
	}
}

// The check that catches a truncated encode. ffmpeg killed part-way through
// still writes a playable file and can still exit 0.
func TestVerifyRejectsATruncatedOutput(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 1800, 4_000_000_000, "h264", 1, 0)

	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("half a film must not replace a whole one")
	}
	if !hasProblem(v, "1800.00s apart") {
		t.Errorf("want the gap stated, got %v", v.Problems)
	}
}

func TestVerifyAllowsSubSecondDurationDrift(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	for _, out := range []float64{3599.2, 3600.9, 3600} {
		v := verify(job(src, decide.Plan{Action: decide.ActionRemux}),
			result("/tmp/x.mkv", out, 7_000_000_000, "h264", 1, 0), nil, VerifyOptions{})
		if !v.OK {
			t.Errorf("duration %v should be within tolerance: %v", out, v.Problems)
		}
	}
}

// A source whose duration cannot be determined cannot be compared against, so
// it cannot be replaced. Declining costs a few gigabytes; the alternative is
// replacing a film with something unverified.
func TestVerifyRefusesWhenTheSourceDurationIsUnknown(t *testing.T) {
	src := result("/lib/Film.mkv", 0, 8_000_000_000, "h264", 1, 0)
	src.Format.Duration = ""
	out := result("/tmp/x.mkv", 3600, 7_000_000_000, "h264", 1, 0)

	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("an unverifiable source must not be replaced")
	}
	if !hasProblem(v, "source duration is unknown") {
		t.Errorf("want a clear reason, got %v", v.Problems)
	}
}

// ffmpeg refusing to carry a stream is a warning on stderr and an exit status
// of zero. The file plays perfectly, just without the track. Counting is the
// only way to notice.
func TestVerifyRejectsAMissingStream(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 2, 3)
	plan := decide.Plan{Action: decide.ActionRemux}
	out := result("/tmp/x.mkv", 3600, 7_000_000_000, "h264", 2, 1)

	v := verify(job(src, plan), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("two lost subtitle tracks must fail verification")
	}
	if !hasProblem(v, "expected 3 subtitle stream(s), found 1") {
		t.Errorf("want the counts stated, got %v", v.Problems)
	}
}

// Silence is the specific outcome the whole audio path is built to prevent,
// so it gets its own message rather than an off-by-N count.
func TestVerifyNamesSilenceExplicitly(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 3600, 7_000_000_000, "h264", 0, 0)

	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{})
	if !hasProblem(v, "no audio at all") {
		t.Errorf("want the silence called out, got %v", v.Problems)
	}
}

func TestVerifyRejectsAnOutputWithNoVideo(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 3600, 7_000_000_000, "", 1, 0)

	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{})
	if !hasProblem(v, "no video stream") {
		t.Errorf("want the missing video called out, got %v", v.Problems)
	}
}

// A VAAPI encoder failing over to something else, or a profile pointed at the
// wrong encoder, produces a perfectly valid file in the wrong codec -- which
// would then be re-encoded on every subsequent scan, forever.
func TestVerifyRejectsTheWrongOutputCodec(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	plan := decide.Plan{Action: decide.ActionEncode, Video: decide.VideoEncode, TargetCodec: "hevc"}
	out := result("/tmp/x.mkv", 3600, 4_000_000_000, "h264", 1, 0)

	v := verify(job(src, plan), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("an encode that did not produce the target codec must fail")
	}
	if !hasProblem(v, "profile targets hevc") {
		t.Errorf("want the codec mismatch named, got %v", v.Problems)
	}
}

// A remux is not checked against the target codec: it is a copy, so the video
// is whatever it already was and that is the intended result.
func TestVerifyDoesNotDemandTheTargetCodecOfARemux(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	plan := decide.Plan{Action: decide.ActionRemux, Video: decide.VideoCopy, TargetCodec: "hevc"}
	out := result("/tmp/x.mkv", 3600, 7_900_000_000, "h264", 1, 0)

	if v := verify(job(src, plan), out, nil, VerifyOptions{}); !v.OK {
		t.Fatalf("a copied h264 stream is the point of a remux: %v", v.Problems)
	}
}

func TestVerifyRejectsAnImplausiblySmallOutput(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 3600, 40_000_000, "h264", 1, 0) // 0.5%

	v := verify(job(src, decide.Plan{Action: decide.ActionEncode, Video: decide.VideoEncode}), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("half a percent of the source is not the film")
	}
	if !hasProblem(v, "implausibly small") {
		t.Errorf("want the size called out, got %v", v.Problems)
	}
}

func TestVerifyRejectsAnEmptyOutput(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 3600, 0, "h264", 1, 0)

	v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{})
	if !hasProblem(v, "output is empty") {
		t.Errorf("want the empty output called out, got %v", v.Problems)
	}
}

// Spending an hour of GPU time to make a file bigger and then deleting the
// smaller original is the one outcome with no upside at all.
func TestVerifyRejectsAnEncodeThatGrewTheFile(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 4_000_000_000, "h264", 1, 0)
	plan := decide.Plan{Action: decide.ActionEncode, Video: decide.VideoEncode, TargetCodec: "hevc"}
	out := result("/tmp/x.mkv", 3600, 4_500_000_000, "hevc", 1, 0)

	v := verify(job(src, plan), out, nil, VerifyOptions{})
	if v.OK {
		t.Fatal("a re-encode that grew the file must not replace it")
	}
	if !hasProblem(v, "bigger") {
		t.Errorf("want the growth called out, got %v", v.Problems)
	}

	if v := verify(job(src, plan), out, nil, VerifyOptions{AllowLargerOutput: true}); !v.OK {
		t.Errorf("AllowLargerOutput should permit it: %v", v.Problems)
	}
}

// A remux can legitimately gain a few bytes -- a language tag written onto an
// untagged track is the common case, and it is pure metadata.
func TestVerifyAllowsARemuxToGrowSlightly(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 4_000_000_000, "h264", 1, 0)
	out := result("/tmp/x.mkv", 3600, 4_000_001_000, "h264", 1, 0)

	if v := verify(job(src, decide.Plan{Action: decide.ActionRemux}), out, nil, VerifyOptions{}); !v.OK {
		t.Fatalf("a remux gaining a kilobyte of metadata is normal: %v", v.Problems)
	}
}

// The zero VerifyOptions has to be the intended configuration, because that
// is what an Encoder built without one gets.
func TestZeroVerifyOptionsAreTheRealDefaults(t *testing.T) {
	var o VerifyOptions
	if o.durationTolerance() != DefaultDurationTolerance {
		t.Errorf("duration tolerance = %v, want %v", o.durationTolerance(), DefaultDurationTolerance)
	}
	if o.minSizeRatio() != DefaultMinSizeRatio {
		t.Errorf("min size ratio = %v, want %v", o.minSizeRatio(), DefaultMinSizeRatio)
	}
	if o.AllowLargerOutput {
		t.Error("the zero value must not permit a re-encode that grew the file")
	}
}

// A compat quirk can record the same stream in Drops twice, once per reason.
// Counting drops rather than distinct streams would expect one track too few.
func TestExpectedStreamsCountsEachDroppedStreamOnce(t *testing.T) {
	src := result("/lib/Film.mkv", 3600, 8_000_000_000, "h264", 3, 0)
	plan := decide.Plan{
		Action: decide.ActionRemux,
		Drops: []decide.Drop{
			{Kind: "audio", TypeIdx: 1, Reason: "language fra not in keep list"},
			{Kind: "audio", TypeIdx: 1, Reason: "title matches commentary"},
		},
	}
	if got := expectedStreams(job(src, plan)).Audio; got != 2 {
		t.Errorf("expected audio = %d, want 2", got)
	}
}
