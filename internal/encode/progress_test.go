package encode

import (
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
	readProgress(strings.NewReader(stream), 16, func(p Progress) { got = append(got, p) })

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
	readProgress(strings.NewReader("out_time_ms=30000000\nprogress=end\n"), 60, func(p Progress) { got = p })
	if got.Seconds != 30 {
		t.Errorf("seconds = %v, want 30", got.Seconds)
	}
}

// The last report can overshoot the source's duration slightly, and a
// progress bar past 100% looks like a bug.
func TestProgressFractionIsClamped(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("out_time_us=61000000\nprogress=end\n"), 60, func(p Progress) { got = p })
	if got.Fraction != 1 {
		t.Errorf("fraction = %v, want 1", got.Fraction)
	}
}

// Before the first frame ffmpeg reports "N/A" for speed and fps. That is not
// a reason to stop reporting progress.
func TestReadProgressSurvivesNotAvailableValues(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("fps=N/A\nspeed=N/A\nout_time_us=0\nprogress=continue\n"), 60, func(p Progress) { got = p })
	if got.Speed != 0 || got.FPS != 0 {
		t.Errorf("got %+v, want zeroed speed and fps", got)
	}
}

func TestReadProgressWithAnUnknownDuration(t *testing.T) {
	var got Progress
	readProgress(strings.NewReader("out_time_us=4000000\nprogress=continue\n"), 0, func(p Progress) { got = p })
	if got.Fraction != 0 {
		t.Errorf("fraction = %v; with no source duration there is no fraction to report", got.Fraction)
	}
	if got.Seconds != 4 {
		t.Errorf("seconds = %v, want 4", got.Seconds)
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
