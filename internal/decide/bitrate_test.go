package decide_test

import (
	"testing"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

func result(sizeBytes int64, duration string) *probe.Result {
	r := &probe.Result{Path: "/m/a.mkv"}
	r.Format.Size = itoa(sizeBytes)
	r.Format.Duration = duration
	r.Streams = []probe.Stream{video("h264")}
	return r
}

// The tier table is the entire quality control of this project, so its
// boundaries are worth pinning exactly. A file landing one kbps either side of
// a threshold gets a materially different target.
func TestTierBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		sourceKbps  int
		wantDivisor float64
	}{
		{"just below the 3000 tier", 2999, 1.0},
		{"exactly 3000", 3000, 1.5},
		{"mid 1.5x tier", 5000, 1.5},
		{"just below the 6000 tier", 5999, 1.5},
		{"exactly 6000", 6000, 1.75},
		{"just below the 10000 tier", 9999, 1.75},
		{"exactly 10000", 10000, 2.0},
		{"well above", 40000, 2.0},
	}

	p := decide.Standard()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Construct a file with the wanted source bitrate: one hour long,
			// sized to suit. The math works in binary megabytes.
			const seconds = 3600.0
			sizeBytes := int64(float64(tc.sourceKbps) * seconds / 8000 * 1024 * 1024)
			r := result(sizeBytes, "3600")

			b := decide.CalculateBitrate(r, p)
			if b.SourceKbps < tc.sourceKbps-1 || b.SourceKbps > tc.sourceKbps+1 {
				t.Fatalf("source = %d, want about %d", b.SourceKbps, tc.sourceKbps)
			}

			wantTarget := int(float64(b.SourceKbps) / tc.wantDivisor)
			if b.TargetKbps != wantTarget {
				t.Errorf("target = %d, want %d (divisor %.2f on source %d)",
					b.TargetKbps, wantTarget, tc.wantDivisor, b.SourceKbps)
			}
		})
	}
}

// Below 3000 kbps the divisor is 1.0, i.e. the target equals the source. Since
// the floor is also 3000, such a file is always declined -- which is the rule
// that made the old stack skip 7,880 files and never produce a bad-looking
// one.
func TestLowBitrateFilesAreAlwaysDeclined(t *testing.T) {
	p := decide.Standard()
	for _, kbps := range []int{500, 1500, 2500, 2999} {
		sizeBytes := int64(float64(kbps) * 3600 / 8000 * 1024 * 1024)
		r := result(sizeBytes, "3600")

		b := decide.CalculateBitrate(r, p)
		if b.TargetKbps != b.SourceKbps {
			t.Errorf("%d kbps: target %d != source %d, expected no reduction",
				kbps, b.TargetKbps, b.SourceKbps)
		}
		if b.TargetKbps >= p.Video.Bitrate.FloorKbps {
			t.Errorf("%d kbps: target %d is above the floor", kbps, b.TargetKbps)
		}
	}
}

func TestMinAndMaxBracketTheTarget(t *testing.T) {
	p := decide.Standard()
	r := result(3_000_000_000, "3600")

	b := decide.CalculateBitrate(r, p)
	if want := int(float64(b.TargetKbps) * 0.7); b.MinKbps != want {
		t.Errorf("min = %d, want %d", b.MinKbps, want)
	}
	if want := int(float64(b.TargetKbps) * 1.3); b.MaxKbps != want {
		t.Errorf("max = %d, want %d", b.MaxKbps, want)
	}
	// bufsize is the full source bitrate, which lets the encoder spend the
	// original budget on a hard scene.
	if b.BufsizeKbps != b.SourceKbps {
		t.Errorf("bufsize = %d, want the source %d", b.BufsizeKbps, b.SourceKbps)
	}
}

func TestZeroDurationYieldsNoBitrate(t *testing.T) {
	for _, d := range []string{"0", "", "N/A", "-1"} {
		if b := decide.CalculateBitrate(result(1_000_000_000, d), decide.Standard()); b.SourceKbps != 0 {
			t.Errorf("duration %q: source = %d, want 0", d, b.SourceKbps)
		}
	}
}

// The compat profile's duration constant is 0.0166667 rather than 1/60. The
// difference is under a tenth of a percent, but it lands on a truncation
// boundary often enough to matter to the golden corpus.
func TestMinuteFactorQuirkShiftsTheResultSlightly(t *testing.T) {
	r := result(1_845_711_143, "1407.040000")

	compat := decide.CalculateBitrate(r, decide.TdarrCompat())
	honest := decide.CalculateBitrate(r, decide.Standard())

	if compat.SourceKbps != 10007 {
		t.Errorf("compat source = %d, want 10007 as recorded by the old stack", compat.SourceKbps)
	}
	if honest.SourceKbps != 10008 {
		t.Errorf("honest source = %d, want 10008", honest.SourceKbps)
	}
}

// The container basis charges audio and subtitle bytes to the video budget.
// That over-allocation is the default on purpose; the video_stream basis is
// the honest alternative.
func TestVideoStreamBasisUsesTheStreamsOwnBitrate(t *testing.T) {
	r := result(1_000_000_000, "3600")
	r.Streams = []probe.Stream{{
		Index: 0, CodecName: "h264", CodecType: probe.TypeVideo,
		BitRate: "4229000", Disposition: map[string]int{},
	}}

	p := decide.Standard()
	p.Video.Bitrate.Basis = decide.BasisVideoStream

	if b := decide.CalculateBitrate(r, p); b.SourceKbps != 4229 {
		t.Errorf("source = %d, want 4229 from the stream's own bit_rate", b.SourceKbps)
	}
}

// Matroska usually omits a per-stream bit_rate and writes a BPS tag instead.
func TestVideoStreamBasisReadsTheMatroskaBPSTag(t *testing.T) {
	r := result(1_000_000_000, "3600")
	r.Streams = []probe.Stream{{
		Index: 0, CodecName: "h264", CodecType: probe.TypeVideo,
		Tags: map[string]string{"BPS": "4229000"}, Disposition: map[string]int{},
	}}

	p := decide.Standard()
	p.Video.Bitrate.Basis = decide.BasisVideoStream

	if b := decide.CalculateBitrate(r, p); b.SourceKbps != 4229 {
		t.Errorf("source = %d, want 4229 from the BPS tag", b.SourceKbps)
	}
}

// When the file reports no video bitrate at all, the video_stream basis has to
// fall back rather than conclude the file has none.
func TestVideoStreamBasisFallsBackToTheContainer(t *testing.T) {
	r := result(3_000_000_000, "3600")

	p := decide.Standard()
	p.Video.Bitrate.Basis = decide.BasisVideoStream

	container := decide.CalculateBitrate(r, decide.Standard())
	if b := decide.CalculateBitrate(r, p); b.SourceKbps != container.SourceKbps {
		t.Errorf("source = %d, want the container basis %d", b.SourceKbps, container.SourceKbps)
	}
}
