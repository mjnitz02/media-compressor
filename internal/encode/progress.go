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
	// Seconds is how far into the output ffmpeg has got.
	Seconds float64

	// Fraction is Seconds over the source's duration, 0 when the duration is
	// unknown. Clamped to 1: the last report can overshoot slightly.
	Fraction float64

	// Speed is the multiple of real time, as ffmpeg reports it ("1.8x").
	Speed float64

	// FPS is frames per second, and Bytes the size written so far.
	FPS   float64
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
func readProgress(r io.Reader, sourceSeconds float64, fn func(Progress)) {
	scanner := bufio.NewScanner(r)
	var p Progress
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
				p.Seconds = float64(n) / 1e6
			}
		case "total_size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.Bytes = n
			}
		case "fps":
			p.FPS, _ = strconv.ParseFloat(value, 64)
		case "speed":
			// "1.83x", or "N/A" before the first frame.
			p.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64)
		case "progress":
			p.Done = value == "end"
			p.Fraction = 0
			if sourceSeconds > 0 {
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
