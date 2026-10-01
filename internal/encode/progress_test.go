package encode

import (
	"math"
	"strings"
	"testing"
)

// ffmpeg's -progress output is blocks of key=value lines terminated by a
// progress= line. A report is only emitted at the end of a block, so a caller
// never sees half of one.
func TestReadProgressEmitsOneReportPerBlock(t *testing.T) {
	stream := strings.Join([]string{
		"frame=120", "fps=48.0", "total_size=1048576", "out_time_us=4000000", "speed=1.83x", "progress=continue",
		"frame=240", "fps=50.0", "total_size=2097152", "out_time_us=8000000", "speed=1.90x", "progress=end",
	}, "\n") + "\n"

	var got []Progress
	readProgress(strings.NewReader(stream), 16, 0, func(p Progress) { got = append(got, p) })

	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2: %+v", len(got), got)
	}
	if got[0].Seconds != 4 || got[0].Fraction != 0.25 || got[0].Speed != 1.83 || got[0].Bytes != 1048576 {
		t.Errorf("first report = %+v", got[0])
	}
	if !got[1].Done {
		t.Error("the last report should be marked done")
	}
	if got[1].Fraction != 0.5 {
		t.Errorf("fraction = %v, want 0.5", got[1].Fraction)
	}
}

// out_time_ms is microseconds, despite the name. Reading it as milliseconds
// would report a progress bar a thousand times too slow.
func TestReadProgressTreatsOutTimeMsAsMicroseconds(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("out_time_ms=30000000\nprogress=end\n"), 60, 0, func(p Progress) { got = p })
	if got.Seconds != 30 {
		t.Errorf("seconds = %v, want 30", got.Seconds)
	}
}

// The last report can overshoot the source's duration slightly, and a
// progress bar past 100% looks like a bug.
func TestProgressFractionIsClamped(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("out_time_us=61000000\nprogress=end\n"), 60, 0, func(p Progress) { got = p })
	if got.Fraction != 1 {
		t.Errorf("fraction = %v, want 1", got.Fraction)
	}
}

// Before the first frame ffmpeg reports "N/A" for speed and fps. That is not
// a reason to stop reporting progress.
func TestReadProgressSurvivesNotAvailableValues(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("fps=N/A\nspeed=N/A\nout_time_us=0\nprogress=continue\n"), 60, 0, func(p Progress) { got = p })
	if got.Speed != 0 || got.FPS != 0 {
		t.Errorf("got %+v, want zeroed speed and fps", got)
	}
}

func TestReadProgressWithAnUnknownDuration(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("out_time_us=4000000\nprogress=continue\n"), 0, 0, func(p Progress) { got = p })
	if got.Fraction != 0 {
		t.Errorf("fraction = %v; with no source duration there is no fraction to report", got.Fraction)
	}
	if got.Seconds != 4 {
		t.Errorf("seconds = %v, want 4", got.Seconds)
	}
}

// An encode that maps attachments -- `-map 0` over any file carrying subtitle
// fonts -- gets out_time=N/A from ffmpeg for its whole run, because out_time is
// an aggregate over the output streams that carry timestamps and attachment
// streams carry none. speed and bitrate are derived from out_time and go with
// it. This is a real capture from ffmpeg in the container image, encoding an
// anime episode with 11 font attachments.
//
// Before the frame fallback this reported "0% (0.0x)" for two and a half
// minutes per file, which is the whole of the visible progress.
func TestReadProgressDerivesFromFramesWhenOutTimeIsUnavailable(t *testing.T) {
	stream := strings.Join([]string{
		"frame=236", "fps=67.41", "bitrate=N/A", "total_size=6029312",
		"out_time_us=N/A", "out_time_ms=N/A", "out_time=N/A", "speed=N/A", "progress=continue",
		"frame=480", "fps=60.80", "bitrate=N/A", "total_size=8846480",
		"out_time_us=N/A", "out_time_ms=N/A", "out_time=N/A", "speed=N/A", "progress=continue",
	}, "\n") + "\n"

	var got []Progress
	// 24 fps source, 120 s long.
	readProgress(strings.NewReader(stream), 120, 24, func(p Progress) { got = append(got, p) })

	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2", len(got))
	}
	// 236 frames at 24 fps is 9.83 s of output, and 9.83 of 120 is 8%.
	if math.Abs(got[0].Seconds-9.8333) > 0.001 {
		t.Errorf("seconds = %v, want ~9.83", got[0].Seconds)
	}
	if math.Abs(got[0].Fraction-0.08194) > 0.0001 {
		t.Errorf("fraction = %v, want ~0.082", got[0].Fraction)
	}
	// Encoding at 67.41 fps a 24 fps source is 2.8x real time.
	if math.Abs(got[0].Speed-2.8088) > 0.001 {
		t.Errorf("speed = %v, want ~2.81", got[0].Speed)
	}

	// The second report must move. Deriving into the same field the fallback
	// tests would freeze it here at the first block's value.
	if got[1].Seconds <= got[0].Seconds {
		t.Errorf("seconds went %v -> %v; progress must advance", got[0].Seconds, got[1].Seconds)
	}
	if math.Abs(got[1].Speed-2.5333) > 0.001 {
		t.Errorf("speed = %v, want ~2.53 -- it should track the latest fps", got[1].Speed)
	}
}

// The fallback is a fallback. When ffmpeg reports out_time and speed itself,
// those are the numbers, and the frame count must not override them.
func TestReadProgressPrefersFFmpegsOwnNumbers(t *testing.T) {
	stream := "frame=240\nfps=48.0\nout_time_us=4000000\nspeed=1.83x\nprogress=continue\n"

	var got Progress
	readProgress(strings.NewReader(stream), 16, 24, func(p Progress) { got = p })

	if got.Seconds != 4 {
		t.Errorf("seconds = %v, want 4 from out_time -- not 10 from the frame count", got.Seconds)
	}
	if got.Speed != 1.83 {
		t.Errorf("speed = %v, want 1.83 as reported", got.Speed)
	}
}

// speed=N/A turns up mid-run, not only before the first frame. Parsing it into
// the field unconditionally dropped the reading to zero for that block.
func TestReadProgressKeepsTheLastReportedSpeedThroughANotAvailable(t *testing.T) {
	stream := strings.Join([]string{
		"out_time_us=4000000", "speed=1.83x", "progress=continue",
		"out_time_us=5000000", "speed=N/A", "progress=continue",
	}, "\n") + "\n"

	var got []Progress
	readProgress(strings.NewReader(stream), 60, 0, func(p Progress) { got = append(got, p) })

	if got[1].Speed != 1.83 {
		t.Errorf("speed = %v, want the last reported 1.83 rather than a zero", got[1].Speed)
	}
}

// ffmpeg on a damaged file can produce megabytes of repeated warnings, and
// all that is wanted is enough of the end to explain the failure.
func TestTailKeepsTheEnd(t *testing.T) {
	tl := &tail{limit: 32}
	tl.Write([]byte(strings.Repeat("a", 100)))
	tl.Write([]byte("the actual error"))

	got := tl.String()
	if !strings.HasSuffix(got, "the actual error") {
		t.Errorf("the end was lost: %q", got)
	}
	if !strings.HasPrefix(got, "[...]") {
		t.Errorf("truncation should be visible: %q", got)
	}
	if len(got) > 32+len("[...]\n") {
		t.Errorf("kept %d bytes, want at most the limit", len(got))
	}
}

func TestTailKeepsShortOutputWhole(t *testing.T) {
	tl := &tail{limit: 32}
	tl.Write([]byte("short"))
	if got := tl.String(); got != "short" {
		t.Errorf("got %q, want %q", got, "short")
	}
}
