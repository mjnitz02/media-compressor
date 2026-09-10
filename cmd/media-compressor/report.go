package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/runner"
)

// report renders one pass. The shape of it is the point: counts first, then
// the files that would be touched, then the things worth reading -- because
// the operator's question is almost never "what happened to this file", it is
// "did anything happen that I should look at".
type report struct {
	title     string
	root      string
	outcomes  []runner.Outcome
	unscanned []string // paths the walk declined before anything was probed
	swept     int
	verbose   bool
	binary    string
}

func (r *report) add(o runner.Outcome) { r.outcomes = append(r.outcomes, o) }

// addSummary pulls in everything a pass reported that is not per-file.
func (r *report) addSummary(sum *runner.Summary) {
	r.outcomes = append(r.outcomes, sum.Outcomes...)
	for _, s := range sum.Unscanned {
		r.unscanned = append(r.unscanned, fmt.Sprintf("%s -- %s", s.Path, s.Reason))
	}
	r.swept = sum.Swept
}

// print writes the whole summary.
//
// The breakdown is by Action, so every file that was decided lands in exactly
// one bucket. The declined-encode count is reported separately underneath
// rather than as another bucket, because a file whose encode was declined on
// the bitrate floor can still want a cheap remux -- it is a fact about the
// video decision, not about the file as a whole.
func (r *report) print(w io.Writer, dryRun bool) {
	fmt.Fprintf(w, "\n%s\n", r.title)

	counts := map[decide.Action]int{}
	skipReasons := map[string]int{}
	var skipped, touched, failed, declined, waiting []runner.Outcome
	cached := 0

	for _, o := range r.outcomes {
		if o.Err != nil {
			failed = append(failed, o)
			continue
		}
		if o.Declined != "" {
			declined = append(declined, o)
			continue
		}
		if o.Waiting != "" {
			waiting = append(waiting, o)
		}
		if o.Plan.Action == "" {
			// Not eligible, so never decided. Counting it under an action
			// would invent a decision nobody made.
			continue
		}
		if o.FromCache {
			cached++
		}
		counts[o.Plan.Action]++
		if o.Plan.Action != decide.ActionNone {
			touched = append(touched, o)
		}
		if o.Plan.Video == decide.VideoSkippedFloor {
			skipReasons[skipReason(o.Plan)]++
			skipped = append(skipped, o)
		}
	}

	fmt.Fprintf(w, "  %d %s considered\n", len(r.outcomes), plural(len(r.outcomes), "file", "files"))
	line := func(n int, label string) {
		if n > 0 {
			fmt.Fprintf(w, "    %6d  %s\n", n, label)
		}
	}
	line(counts[decide.ActionNone], "leave alone -- already as the profile wants them")
	line(counts[decide.ActionRemux], "remux -- streams only, no re-encode")
	line(counts[decide.ActionEncode], "encode")
	line(len(declined), "declined -- listed below")
	line(len(failed), "could not be read or processed")
	line(len(r.unscanned), "not considered (ignored, too new, or not a media file)")
	for _, reason := range sortedKeys(skipReasons) {
		line(skipReasons[reason], "of those, encode declined: "+reason)
	}
	line(cached, "answered from the last scan, not re-probed")

	waitingLabel := "not eligible yet -- still settling, or backing off after a failure"
	if dryRun {
		waitingLabel += " (planned above anyway)"
	}
	line(len(waiting), waitingLabel)
	if r.swept > 0 {
		line(r.swept, "forgotten -- no longer on disk")
	}

	r.printGroup(w, "encode", touched, decide.ActionEncode, dryRun)
	r.printGroup(w, "remux", touched, decide.ActionRemux, dryRun)
	r.printRenames(w, touched)
	r.printNotes(w)
	r.printDeclined(w, declined)
	r.printFailures(w, failed)

	if r.verbose && len(waiting) > 0 {
		fmt.Fprintf(w, "\nnot eligible yet (%d)\n", len(waiting))
		for _, o := range waiting {
			fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.Path), indent(o.Waiting))
		}
	}
	if r.verbose && len(skipped) > 0 {
		fmt.Fprintf(w, "\ndeclined encodes (%d)\n", len(skipped))
		for _, o := range skipped {
			fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.Path), indent(o.Plan.Reason))
		}
	}
	if r.verbose && len(r.unscanned) > 0 {
		fmt.Fprintf(w, "\nnot considered (%d)\n", len(r.unscanned))
		for _, s := range r.unscanned {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
}

func (r *report) printGroup(w io.Writer, label string, all []runner.Outcome, action decide.Action, dryRun bool) {
	var group []runner.Outcome
	for _, o := range all {
		if o.Plan.Action == action {
			group = append(group, o)
		}
	}
	if len(group) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d)\n", label, len(group))
	for _, o := range group {
		fmt.Fprintf(w, "  %s\n", r.rel(o.Path))
		if action == decide.ActionEncode {
			fmt.Fprintf(w, "      %s\n", describeEncode(o.Plan))
		}
		for _, d := range o.Plan.Drops {
			fmt.Fprintf(w, "      drop %s:%d %s%s -- %s\n", d.Kind, d.TypeIdx, d.Codec, langSuffix(d.Language), d.Reason)
		}
		for _, c := range o.Plan.Changes {
			fmt.Fprintf(w, "      %s:%d %s -> %s -- %s\n", c.Kind, c.TypeIdx, orNone(c.From), c.To, c.Reason)
		}
		if o.Job != nil && o.Job.Renames() {
			fmt.Fprintf(w, "      renames to %s\n", filepath.Base(o.Job.FinalPath))
		}
		if o.Waiting != "" {
			fmt.Fprintf(w, "      not yet: %s\n", o.Waiting)
		}
		if r.verbose && o.Job != nil {
			fmt.Fprintf(w, "      temp %s\n", o.Job.TempPath)
			fmt.Fprintf(w, "      %s\n", o.Job.Command(r.binary))
		}
		if !dryRun && o.Result != nil {
			fmt.Fprintf(w, "      %s\n", describeResult(o.Result))
		}
	}
}

// printRenames calls out extension changes on their own, because a rename is
// the one thing here that is visible to Plex and to the *arr stacks, and it is
// worth seeing before a run rather than after.
func (r *report) printRenames(w io.Writer, all []runner.Outcome) {
	var renames []runner.Outcome
	for _, o := range all {
		if o.Job != nil && o.Job.Renames() {
			renames = append(renames, o)
		}
	}
	if len(renames) == 0 {
		return
	}
	fmt.Fprintf(w, "\nrenames (%d) -- the *arr stacks track files by path, and Plex cannot swap a file mid-playback under a new name\n", len(renames))
	for _, o := range renames {
		fmt.Fprintf(w, "  %s -> %s\n", r.rel(o.Path), filepath.Base(o.Job.FinalPath))
	}
}

// printNotes surfaces every case where a safety rule overrode the profile.
// A note means the configuration asked for something this tool declined to do
// to the file, which is the one thing worth reading proactively.
func (r *report) printNotes(w io.Writer) {
	type entry struct{ path, note string }
	var notes []entry
	for _, o := range r.outcomes {
		for _, n := range o.Plan.Notes {
			notes = append(notes, entry{o.Path, n})
		}
		if o.Result != nil {
			for _, n := range o.Result.Notes {
				notes = append(notes, entry{o.Path, n})
			}
		}
	}
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(w, "\nsafety notes (%d) -- a rule overrode the profile\n", len(notes))
	for _, n := range notes {
		fmt.Fprintf(w, "  %s\n%s\n", r.rel(n.path), indent(n.note))
	}
}

// printDeclined lists the files this tool chose not to act on. Always listed
// in full rather than only counted: each one is a deliberate refusal with a
// specific reason, and there should never be many.
func (r *report) printDeclined(w io.Writer, declined []runner.Outcome) {
	if len(declined) == 0 {
		return
	}
	fmt.Fprintf(w, "\ndeclined (%d) -- left completely alone\n", len(declined))
	for _, o := range declined {
		fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.Path), indent(o.Declined))
	}
}

func (r *report) printFailures(w io.Writer, failed []runner.Outcome) {
	if len(failed) == 0 {
		return
	}
	fmt.Fprintf(w, "\nfailures (%d)\n", len(failed))
	for _, o := range failed {
		fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.Path), indent(o.Err.Error()))
	}
}

// rel shortens a path against the root being reported on, so a listing of one
// library reads as filenames rather than as the same prefix 1,200 times.
func (r *report) rel(path string) string {
	if r.root == "" {
		return path
	}
	if rel, err := filepath.Rel(r.root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func describeEncode(p decide.Plan) string {
	return fmt.Sprintf("%d kbps -> %s %d kbps (min %d, max %d) via %s",
		p.Bitrate.SourceKbps, p.TargetCodec, p.Bitrate.TargetKbps,
		p.Bitrate.MinKbps, p.Bitrate.MaxKbps, p.Encoder)
}

func describeResult(res *encode.Result) string {
	switch {
	case res.Replaced:
		v := res.Verification
		return fmt.Sprintf("replaced in %s: %s -> %s (%.0f%%)",
			res.Elapsed.Round(1e9), humanBytes(v.SourceSize), humanBytes(v.OutputSize), v.SizeRatio()*100)
	case res.TempKept != "":
		return "failed; output kept at " + res.TempKept
	default:
		return "failed"
	}
}

// skipReason collapses the per-file reason into something countable. The
// numbers are what matter in the summary; the exact bitrates are in -v.
func skipReason(p decide.Plan) string {
	if i := strings.Index(p.Reason, " below floor"); i >= 0 {
		return "target bitrate below the floor"
	}
	return p.Reason
}

// indent lines up a reason under the filename it belongs to. ffmpeg and
// ffprobe errors are routinely several lines long, and an unindented second
// line reads as another filename.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      " + strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func langSuffix(lang string) string {
	if lang == "" {
		return ""
	}
	return " (" + lang + ")"
}

func orNone(s string) string {
	if s == "" {
		return "untagged"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	if n < unit {
		return fmt.Sprintf("%s%d B", neg, n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%s%.1f %ciB", neg, float64(n)/float64(div), "KMGTP"[exp])
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
