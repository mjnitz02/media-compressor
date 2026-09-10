package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
)

// outcome is what happened to one file, whether or not anything was done to
// it. Declining is the most common outcome in this project and it is reported
// as fully as an encode is -- "what did it not touch, and why" is the question
// the operator actually has.
type outcome struct {
	path string
	plan decide.Plan
	job  *encode.Job // nil when there is no work
	err  error

	// declined is set when this tool decided not to act on the file at all:
	// a plan decide could not make, or a file encode refuses to replace in
	// place. It is not a failure, and it does not affect the exit status.
	declined string

	// result is set only on a real run.
	result *encode.Result
}

// report collects outcomes and prints the summary.
type report struct {
	title     string
	root      string
	outcomes  []outcome
	unscanned []string // files the walk declined before probing
	verbose   bool
	binary    string
}

func (r *report) add(o outcome) { r.outcomes = append(r.outcomes, o) }

// print writes the whole summary: counts first, then the files that would be
// touched, then the things worth reading.
//
// The breakdown is by Action, so every file lands in exactly one bucket. The
// declined-encode count is reported separately underneath rather than as a
// fifth bucket, because a file whose encode was declined on the bitrate floor
// can still want a cheap remux -- it is a fact about the video decision, not
// about the file as a whole.
func (r *report) print(w io.Writer, dryRun bool) {
	fmt.Fprintf(w, "\n%s\n", r.title)

	counts := map[decide.Action]int{}
	skipReasons := map[string]int{}
	var skipped, touched, failed []outcome

	var declined []outcome
	for _, o := range r.outcomes {
		if o.err != nil {
			failed = append(failed, o)
			continue
		}
		if o.declined != "" {
			declined = append(declined, o)
			continue
		}
		counts[o.plan.Action]++
		if o.plan.Action != decide.ActionNone {
			touched = append(touched, o)
		}
		if o.plan.Video == decide.VideoSkippedFloor {
			skipReasons[skipReason(o.plan)]++
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

	r.printGroup(w, "encode", touched, decide.ActionEncode, dryRun)
	r.printGroup(w, "remux", touched, decide.ActionRemux, dryRun)
	r.printRenames(w, touched)
	r.printNotes(w)
	r.printDeclined(w, declined)
	r.printFailures(w, failed)

	if r.verbose && len(skipped) > 0 {
		fmt.Fprintf(w, "\ndeclined encodes (%d)\n", len(skipped))
		for _, o := range skipped {
			fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.path), indent(o.plan.Reason))
		}
	}
	if r.verbose && len(r.unscanned) > 0 {
		fmt.Fprintf(w, "\nnot considered (%d)\n", len(r.unscanned))
		for _, s := range r.unscanned {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
}

func (r *report) printGroup(w io.Writer, label string, all []outcome, action decide.Action, dryRun bool) {
	var group []outcome
	for _, o := range all {
		if o.plan.Action == action {
			group = append(group, o)
		}
	}
	if len(group) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d)\n", label, len(group))
	for _, o := range group {
		fmt.Fprintf(w, "  %s\n", r.rel(o.path))
		if action == decide.ActionEncode {
			fmt.Fprintf(w, "      %s\n", describeEncode(o.plan))
		}
		for _, d := range o.plan.Drops {
			fmt.Fprintf(w, "      drop %s:%d %s%s -- %s\n", d.Kind, d.TypeIdx, d.Codec, langSuffix(d.Language), d.Reason)
		}
		for _, c := range o.plan.Changes {
			fmt.Fprintf(w, "      %s:%d %s -> %s -- %s\n", c.Kind, c.TypeIdx, orNone(c.From), c.To, c.Reason)
		}
		if o.job != nil && o.job.Renames() {
			fmt.Fprintf(w, "      renames to %s\n", filepath.Base(o.job.FinalPath))
		}
		if r.verbose && o.job != nil {
			fmt.Fprintf(w, "      temp %s\n", o.job.TempPath)
			fmt.Fprintf(w, "      %s\n", o.job.Command(r.binary))
		}
		if !dryRun && o.result != nil {
			fmt.Fprintf(w, "      %s\n", describeResult(o.result))
		}
	}
}

// printRenames calls out extension changes on their own, because a rename is
// the one thing here that is visible to Plex and to the *arr stacks, and it is
// worth seeing before a run rather than after.
func (r *report) printRenames(w io.Writer, all []outcome) {
	var renames []outcome
	for _, o := range all {
		if o.job != nil && o.job.Renames() {
			renames = append(renames, o)
		}
	}
	if len(renames) == 0 {
		return
	}
	fmt.Fprintf(w, "\nrenames (%d) -- the *arr stacks track files by path, and Plex cannot swap a file mid-playback under a new name\n", len(renames))
	for _, o := range renames {
		fmt.Fprintf(w, "  %s -> %s\n", r.rel(o.path), filepath.Base(o.job.FinalPath))
	}
}

// printNotes surfaces every case where a safety rule overrode the profile.
// A note means the configuration asked for something this tool declined to do
// to the file, which is the one thing worth reading proactively.
func (r *report) printNotes(w io.Writer) {
	type entry struct{ path, note string }
	var notes []entry
	for _, o := range r.outcomes {
		for _, n := range o.plan.Notes {
			notes = append(notes, entry{o.path, n})
		}
		if o.result != nil {
			for _, n := range o.result.Notes {
				notes = append(notes, entry{o.path, n})
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
func (r *report) printDeclined(w io.Writer, declined []outcome) {
	if len(declined) == 0 {
		return
	}
	fmt.Fprintf(w, "\ndeclined (%d) -- left completely alone\n", len(declined))
	for _, o := range declined {
		fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.path), indent(o.declined))
	}
}

func (r *report) printFailures(w io.Writer, failed []outcome) {
	if len(failed) == 0 {
		return
	}
	fmt.Fprintf(w, "\nfailures (%d)\n", len(failed))
	for _, o := range failed {
		fmt.Fprintf(w, "  %s\n%s\n", r.rel(o.path), indent(o.err.Error()))
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
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
