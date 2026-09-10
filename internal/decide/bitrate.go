package decide

import (
	"strconv"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/probe"
)

// Bitrate is the outcome of the bitrate math, in kbps. Every field is an
// integer because ffmpeg is given whole kbps and because the old stack
// truncated at each step; reproducing that truncation is what makes the
// golden corpus match.
type Bitrate struct {
	SourceKbps  int // what the source is judged to be
	TargetKbps  int // -b:v
	MinKbps     int // -minrate
	MaxKbps     int // -maxrate
	BufsizeKbps int // -bufsize
}

// tdarrBytesPerMinuteFactor is the constant Tdarr multiplied a duration in
// minutes by, i.e. (kbit/s -> MiB/min) = 0.0075.
const bitrateToMiBPerMinute = 0.0075

// SourceBitrateKbps derives the bitrate this file is judged to have.
//
// The two bases differ by more than they sound. `container` divides the whole
// file size by the duration, which charges the audio and subtitle bytes to the
// video budget and so over-estimates. It is wrong, and it is what produced
// output the operator is happy with, so it is the default. See
// docs/tdarr-analysis.md.
func SourceBitrateKbps(r *probe.Result, p Profile) int {
	seconds := r.DurationSeconds()
	if seconds <= 0 {
		return 0
	}

	if p.Video.Bitrate.Basis == BasisVideoStream {
		if kbps, ok := videoStreamKbps(r); ok {
			return kbps
		}
		// Fall through to the container basis rather than guess. Matroska
		// very often omits a per-stream bit_rate.
	}

	// The order of operations here is deliberate and must not be "simplified".
	// Tdarr computed minutes first, then multiplied, and floating-point
	// rounding at these magnitudes is close enough to a whole kbps that
	// reassociating the arithmetic flips roughly one fixture in thirty.
	minutes := seconds * p.Quirks.MinuteFactor
	return trunc(r.SizeMiB() / (minutes * bitrateToMiBPerMinute))
}

// videoStreamKbps reads the real video bitrate when the file happens to carry
// one, either as a stream field or as the BPS tag Matroska writes.
func videoStreamKbps(r *probe.Result) (int, bool) {
	s, _, ok := r.MainVideo()
	if !ok {
		return 0, false
	}
	if bps, err := strconv.ParseFloat(strings.TrimSpace(s.BitRate), 64); err == nil && bps > 0 {
		return trunc(bps / 1000), true
	}
	for k, v := range s.Tags {
		if !strings.EqualFold(k, "BPS") && !strings.EqualFold(k, "BPS-eng") {
			continue
		}
		if bps, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && bps > 0 {
			return trunc(bps / 1000), true
		}
	}
	return 0, false
}

// CalculateBitrate applies the profile's tier table to a source bitrate.
//
// The tiers are the entire quality control of this project. A 1.5x nominal
// reduction on a container-basis bitrate works out to roughly 1.35x on the
// actual video stream, which for H.264 -> HEVC is about double what
// transparency needs. That generosity is the point.
func CalculateBitrate(r *probe.Result, p Profile) Bitrate {
	source := SourceBitrateKbps(r, p)
	if source <= 0 {
		return Bitrate{}
	}

	divisor := p.Video.Bitrate.divisorFor(source)
	target := trunc(float64(source) / divisor)

	return Bitrate{
		SourceKbps:  source,
		TargetKbps:  target,
		MinKbps:     trunc(float64(target) * p.Video.Bitrate.MinMultiplier),
		MaxKbps:     trunc(float64(target) * p.Video.Bitrate.MaxMultiplier),
		BufsizeKbps: source,
	}
}

// divisorFor returns the divisor for the first tier the source bitrate reaches,
// scanning highest threshold first.
func (b BitrateRules) divisorFor(sourceKbps int) float64 {
	for _, t := range b.Tiers {
		if sourceKbps >= t.AboveKbps {
			return t.Divisor
		}
	}
	return 1
}

// trunc truncates toward zero, which is what JavaScript's `~~` does for the
// positive values in range here.
func trunc(f float64) int {
	if f != f || f < 0 { // NaN or negative
		return 0
	}
	return int(f)
}
