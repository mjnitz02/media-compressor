package encode

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Progress is one report from a running ffmpeg.
type Progress struct {
	// Seconds is how far into the output ffmpeg has got. Normally ffmpeg's
	// out_time; derived from Frame when that is unavailable.
	Seconds float64

	// Fraction is Seconds over the source's duration, 0 when the duration is
	// unknown. Clamped to 1: the last report can overshoot slightly.
	Fraction float64

	// Speed is the multiple of real time. Normally ffmpeg's own "1.8x";
	// derived from FPS when that is unavailable.
	Speed float64

	// FPS is frames per second, Frame the count encoded so far, and Bytes the
	// size written so far.
	FPS   float64
	Frame int64
	Bytes int64

	// Done is set on the final report.
	Done bool
}

// readProgress consumes ffmpeg's -progress stream and calls fn once per
// report. It reads to EOF, which is what os/exec requires before Wait.
//
// The format is one key=value per line, in blocks terminated by a
// progress=continue or progress=end line. Accumulating a block and emitting
// it whole means a caller never sees a half-updated report.
//
// sourceFPS is the source's frame rate and may be 0. It is only needed for the
// out_time=N/A case described below.
func readProgress(r io.Reader, sourceSeconds, sourceFPS float64, fn func(Progress)) {
	scanner := bufio.NewScanner(r)
	var p Progress

	// Held outside p because both can be absent from any given block, and the
	// fallbacks below have to be able to tell "ffmpeg did not report this" from
	// "the value we derived last time". Writing a derived number straight into
	// p would make it look reported and freeze the fallback on a stale value.
	var rawSeconds, rawSpeed float64

	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "out_time_us", "out_time_ms":
			// Both are microseconds. out_time_ms being microseconds is a
			// long-standing ffmpeg misnomer, not a typo here.
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
				rawSeconds = float64(n) / 1e6
			}
		case "total_size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.Bytes = n
			}
		case "frame":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
				p.Frame = n
			}
		case "fps":
			// Only overwrite on a real number: "N/A" appears mid-run as well as
			// at the start, and dropping to zero for one block would make the
			// reading flicker.
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				p.FPS = f
			}
		case "speed":
			// "1.83x", or "N/A" before the first frame -- and for the whole run
			// in the out_time case below, since ffmpeg derives speed from it.
			if f, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil {
				rawSpeed = f
			}
		case "progress":
			p.Done = value == "end"
			p.Seconds, p.Speed = rawSeconds, rawSpeed

			// ffmpeg's out_time is an aggregate over the output streams that
			// carry timestamps, and attachment streams carry none. So any
			// encode that maps attachments -- every `-map 0` over a file with
			// embedded subtitle fonts -- reports out_time=N/A, and speed and
			// bitrate with it, for its entire run. Confirmed against ffmpeg in
			// this image: the same command reports normally once -map -0:t is
			// added.
			//
			// Dropping the attachments is not an option; they are the fonts the
			// ASS subtitles need. frame= and fps= keep working throughout, so
			// derive from those instead.
			if p.Seconds == 0 && p.Frame > 0 && sourceFPS > 0 {
				p.Seconds = float64(p.Frame) / sourceFPS
			}
			if p.Speed == 0 && sourceFPS > 0 {
				p.Speed = p.FPS / sourceFPS
			}

			p.Fraction = 0
			if sourceSeconds > 0 && p.Seconds > 0 {
				p.Fraction = min(p.Seconds/sourceSeconds, 1)
			}
			fn(p)
		}
	}
	// A scanner error here means ffmpeg's stdout went away, which Wait will
	// report properly. Nothing useful to add.
	io.Copy(io.Discard, r)
}

// tail keeps the last `limit` bytes written to it.
//
// ffmpeg on a broken file can produce megabytes of repeated warnings, and all
// that is wanted is enough of the end to explain the failure.
type tail struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	over  bool // something was discarded
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
		t.over = true
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := string(t.buf)
	if t.over {
		return "[...]\n" + s
	}
	return s
}
