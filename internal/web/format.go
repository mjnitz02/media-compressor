package web

import (
	"fmt"
	"html/template"
	"path/filepath"
	"strings"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/human"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// funcs are the template helpers. They are all formatting: nothing in here
// decides anything, and no label invents information the row does not carry.
var funcs = template.FuncMap{
	"bytes":       human.Bytes,
	"duration":    human.Duration,
	"ago":         human.Ago,
	"until":       human.Until,
	"base":        filepath.Base,
	"dir":         filepath.Dir,
	"pct":         percent,
	"kbps":        kbps,
	"action":      actionLabel,
	"video":       videoLabel,
	"status":      statusLabel,
	"statusClass": statusClass,
	"clock":       clock,
	"firstLine":   firstLine,
	"saved":       savedLabel,
	"since":       since,
	"mul":         func(f, n float64) float64 { return f * n },

	// actionCount exists because ByAction is keyed by decide.Action, and a
	// template's map lookup will not convert a plain string to a named type.
	"actionCount": func(m map[decide.Action]int, key string) int { return m[decide.Action(key)] },
}

// since is how long something running has been running.
func since(t, now time.Time) string {
	if t.IsZero() {
		return "--"
	}
	return human.Duration(now.Sub(t))
}

func percent(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func kbps(n int) string {
	if n <= 0 {
		return "--"
	}
	return fmt.Sprintf("%d kbps", n)
}

// actionLabel is what happens to the file as a whole.
func actionLabel(a decide.Action) string {
	switch a {
	case decide.ActionNone:
		return "leave alone"
	case decide.ActionRemux:
		return "remux"
	case decide.ActionEncode:
		return "encode"
	case decide.ActionError:
		return "could not be planned"
	case "":
		return "not decided yet"
	}
	return string(a)
}

// videoLabel is what happens to the video stream specifically. It is reported
// separately because "we declined to re-encode this" is the most important
// thing this tool says, and a file can be left alone as a whole for a
// completely different reason than its video was.
func videoLabel(v decide.VideoDecision) string {
	switch v {
	case decide.VideoCopy:
		return "video kept as it is"
	case decide.VideoEncode:
		return "video re-encoded"
	case decide.VideoSkippedFloor:
		return "encode declined: under the bitrate floor"
	case "":
		return "no video decision"
	}
	return string(v)
}

func statusLabel(s string) string {
	switch s {
	case store.StatusDone:
		return "done"
	case store.StatusFailed:
		return "failed"
	case store.StatusRunning:
		return "running"
	}
	return s
}

func statusClass(s string) string {
	switch s {
	case store.StatusDone:
		return "ok"
	case store.StatusFailed:
		return "bad"
	}
	return "busy"
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "--"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// firstLine is for error text in a table cell. ffmpeg failures run to several
// lines and the first one is nearly always the whole story; the rest is on
// the history page.
func firstLine(s string) string {
	line := strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	if len(line) > 160 {
		return line[:159] + "…"
	}
	return line
}

// savedLabel reads as a reduction rather than as a number to subtract in your
// head: "4.1 GiB (38%)".
func savedLabel(before, after int64) string {
	if before <= 0 || after <= 0 {
		return "--"
	}
	saved := before - after
	return fmt.Sprintf("%s (%.0f%%)", human.Bytes(saved), float64(saved)/float64(before)*100)
}
