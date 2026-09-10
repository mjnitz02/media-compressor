package encode

import (
	"fmt"
	"math"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

// Defaults for VerifyOptions. Each zero-valued field falls back to these, so
// the zero VerifyOptions is the intended configuration rather than "no
// checks" -- there is no way to ask for no checks, on purpose.
const (
	DefaultDurationTolerance = 1.0  // seconds
	DefaultMinSizeRatio      = 0.10 // of the source's size
)

// VerifyOptions tunes the checks an output must pass before it is allowed to
// replace anything.
type VerifyOptions struct {
	// DurationToleranceSeconds is how far the output's duration may sit from
	// the source's. Zero means DefaultDurationTolerance.
	//
	// A remux is frame-accurate and an encode very nearly so, so this is
	// tight by design: it is the check that catches a truncated output, which
	// is the most likely way for ffmpeg to fail while still exiting 0.
	DurationToleranceSeconds float64

	// MinSizeRatio is the smallest fraction of the source's size the output
	// may be. Zero means DefaultMinSizeRatio.
	//
	// It is a backstop against a file that ffprobe parses but that plainly is
	// not the film -- a header and nothing else. It is not a quality check;
	// the bitrate arithmetic in decide is what controls how small an output
	// is allowed to get, and it never targets anything like this low.
	MinSizeRatio float64

	// AllowLargerOutput permits a re-encode whose output is bigger than its
	// source. By default that fails: spending an hour of GPU time to make a
	// file worse and then deleting the better original is the one outcome
	// with no upside at all. A remux is exempt -- it can legitimately grow by
	// a few bytes of metadata.
	AllowLargerOutput bool
}

func (o VerifyOptions) durationTolerance() float64 {
	if o.DurationToleranceSeconds <= 0 {
		return DefaultDurationTolerance
	}
	return o.DurationToleranceSeconds
}

func (o VerifyOptions) minSizeRatio() float64 {
	if o.MinSizeRatio <= 0 {
		return DefaultMinSizeRatio
	}
	return o.MinSizeRatio
}

// Verification is the measurement of an output, whether or not it passed.
// Everything it checked is recorded, so a failure message can say what the
// numbers actually were.
type Verification struct {
	OK       bool
	Problems []string

	SourceDuration float64
	OutputDuration float64
	SourceSize     int64
	OutputSize     int64

	// ExpectedStreams and OutputStreams are counts per kind: what the plan
	// said the output should contain, and what it contains.
	ExpectedStreams StreamCounts
	OutputStreams   StreamCounts

	// VideoCodec is the codec of the output's main video stream.
	VideoCodec string
}

// StreamCounts is a count of streams by kind.
type StreamCounts struct {
	Video    int
	Audio    int
	Subtitle int
}

func (s StreamCounts) String() string {
	return fmt.Sprintf("%d video, %d audio, %d subtitle", s.Video, s.Audio, s.Subtitle)
}

// SizeRatio is the output's size as a fraction of the source's.
func (v Verification) SizeRatio() float64 {
	if v.SourceSize <= 0 {
		return 0
	}
	return float64(v.OutputSize) / float64(v.SourceSize)
}

// verify measures an output against the plan that produced it.
//
// It takes the probe result rather than a path so that every check is pure
// arithmetic over two probe results and one plan -- the same property that
// makes decide testable. The only I/O is the caller's ffprobe.
func verify(job *Job, out *probe.Result, probeErr error, opts VerifyOptions) Verification {
	v := Verification{
		SourceDuration:  job.Source.DurationSeconds(),
		SourceSize:      job.Source.SizeBytes(),
		ExpectedStreams: expectedStreams(job),
	}

	// Check 1: it has to be a media file at all. Nothing else can be checked
	// if this fails, so there is no point collecting more problems.
	if probeErr != nil {
		v.Problems = append(v.Problems, "output does not ffprobe cleanly: "+probeErr.Error())
		return v
	}

	v.OutputDuration = out.DurationSeconds()
	v.OutputSize = out.SizeBytes()
	v.OutputStreams = countStreams(out)
	if main, _, ok := out.MainVideo(); ok {
		v.VideoCodec = main.CodecName
	}

	// Check 2: duration.
	switch {
	case v.SourceDuration <= 0:
		// Cannot compare, so cannot pass. Declining is the safe direction and
		// this has never been seen: all 1,881 corpus files report one.
		v.Problems = append(v.Problems, "source duration is unknown, so the output cannot be checked against it")
	case v.OutputDuration <= 0:
		v.Problems = append(v.Problems, "output reports no duration")
	default:
		if diff := math.Abs(v.OutputDuration - v.SourceDuration); diff > opts.durationTolerance() {
			v.Problems = append(v.Problems, fmt.Sprintf(
				"duration is %.2fs but the source is %.2fs (%.2fs apart, tolerance %.2fs)",
				v.OutputDuration, v.SourceDuration, diff, opts.durationTolerance()))
		}
	}

	// Check 3: the streams the plan said would be there.
	v.Problems = append(v.Problems, streamProblems(job, out, v)...)

	// Check 4: a plausible size.
	v.Problems = append(v.Problems, sizeProblems(job, v, opts)...)

	v.OK = len(v.Problems) == 0
	return v
}

// expectedStreams is what the plan says the output should contain: every
// stream of that kind in the source, less the ones the plan drops.
func expectedStreams(job *Job) StreamCounts {
	have := countStreams(job.Source)
	dropped := map[string]map[int]bool{}
	for _, d := range job.Plan.Drops {
		if dropped[d.Kind] == nil {
			dropped[d.Kind] = map[int]bool{}
		}
		// Keyed by stream index because a compat quirk can record the same
		// stream twice, once per reason it failed.
		dropped[d.Kind][d.TypeIdx] = true
	}
	have.Video -= len(dropped[probe.TypeVideo])
	have.Audio -= len(dropped[probe.TypeAudio])
	have.Subtitle -= len(dropped[probe.TypeSubtitle])
	return have
}

func countStreams(r *probe.Result) StreamCounts {
	return StreamCounts{
		Video:    len(r.StreamsOfType(probe.TypeVideo)),
		Audio:    len(r.StreamsOfType(probe.TypeAudio)),
		Subtitle: len(r.StreamsOfType(probe.TypeSubtitle)),
	}
}

// streamProblems reports every way the output's streams differ from the plan.
//
// This is the check that catches ffmpeg quietly declining to carry a stream:
// a codec the muxer refuses is a warning on stderr and an exit status of 0,
// and the resulting file plays perfectly -- just without the track. The
// counts are the only way to notice.
func streamProblems(job *Job, out *probe.Result, v Verification) []string {
	var problems []string

	if _, _, ok := out.MainVideo(); !ok {
		problems = append(problems, "output has no video stream")
	} else if job.Plan.Video == decide.VideoEncode && job.Plan.TargetCodec != "" &&
		!strings.EqualFold(v.VideoCodec, job.Plan.TargetCodec) {
		problems = append(problems, fmt.Sprintf(
			"video was encoded to %s but the profile targets %s", v.VideoCodec, job.Plan.TargetCodec))
	}

	if v.OutputStreams.Audio == 0 && v.ExpectedStreams.Audio > 0 {
		// Called out separately from the count mismatch below because a
		// silent file is the specific outcome the whole audio path is built
		// to prevent, and the message should say so.
		problems = append(problems, "output has no audio at all")
	} else if v.OutputStreams.Audio != v.ExpectedStreams.Audio {
		problems = append(problems, countProblem("audio", v.ExpectedStreams.Audio, v.OutputStreams.Audio))
	}

	if v.OutputStreams.Subtitle != v.ExpectedStreams.Subtitle {
		problems = append(problems, countProblem("subtitle", v.ExpectedStreams.Subtitle, v.OutputStreams.Subtitle))
	}
	if v.OutputStreams.Video != v.ExpectedStreams.Video {
		problems = append(problems, countProblem("video", v.ExpectedStreams.Video, v.OutputStreams.Video))
	}
	return problems
}

func countProblem(kind string, want, got int) string {
	return fmt.Sprintf("expected %d %s stream(s), found %d", want, kind, got)
}

func sizeProblems(job *Job, v Verification, opts VerifyOptions) []string {
	var problems []string
	if v.OutputSize <= 0 {
		return append(problems, "output is empty")
	}
	if v.SourceSize > 0 {
		if ratio := v.SizeRatio(); ratio < opts.minSizeRatio() {
			problems = append(problems, fmt.Sprintf(
				"output is %.1f%% of the source (%d bytes from %d), which is implausibly small",
				ratio*100, v.OutputSize, v.SourceSize))
		}
	}
	if !opts.AllowLargerOutput && job.Plan.Action == decide.ActionEncode &&
		v.SourceSize > 0 && v.OutputSize >= v.SourceSize {
		problems = append(problems, fmt.Sprintf(
			"re-encoding made the file bigger (%d bytes from %d), so replacing the source would be a straight loss",
			v.OutputSize, v.SourceSize))
	}
	return problems
}
