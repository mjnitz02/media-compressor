// Package encode carries out a plan: it runs ffmpeg into a temporary file,
// verifies the result, and only then replaces the original.
//
// This is the phase where mistakes are expensive, so the order of operations
// is the design. Nothing is ever written over a source file that has not been
// probed, measured and found to be a complete, plausible replacement, and the
// source is never removed by any path other than the rename that replaces it.
//
// The split between Prepare and Run is deliberate: Prepare works out every
// path and every argument without touching anything, which is what makes
// --dry-run a mode rather than a special case. What --dry-run prints is the
// same *Job the encoder would have executed, not a description of one.
package encode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/probe"
)

// DefaultBinary is the ffmpeg to use when none is configured.
const DefaultBinary = "ffmpeg"

// ErrNothingToDo is returned by Prepare for a plan that does not want any
// work done. It is not a failure: it is the most common outcome in this
// project and callers are expected to check for it.
var ErrNothingToDo = errors.New("plan requires no work")

// Encoder runs plans. The zero value works, using ffmpeg and ffprobe from
// PATH and a temp file beside the source.
type Encoder struct {
	// Binary is the ffmpeg executable. Empty means DefaultBinary.
	Binary string

	// Prober verifies the output. The same one used to probe the source.
	Prober probe.Prober

	// WorkDir is scratch space for in-progress encodes. It is used only when
	// it is on the same filesystem as the file being replaced, because the
	// final step has to be a rename() and a rename across filesystems is not
	// one -- it is a copy, which is neither atomic nor free. When WorkDir is
	// elsewhere the temp file goes beside the source instead.
	WorkDir string

	// Verify tunes the checks the output must pass. The zero value is the
	// intended configuration; see VerifyOptions.
	Verify VerifyOptions

	// OnProgress, if set, is called as ffmpeg reports progress. It is called
	// from the goroutine reading ffmpeg's stdout, so it must not block.
	OnProgress func(Progress)
}

// Job is everything that will be done to one file, worked out in full before
// anything happens. It is the unit --dry-run prints and the unit Run executes.
type Job struct {
	Plan   decide.Plan
	Source *probe.Result

	// SourcePath is the file being replaced.
	SourcePath string

	// FinalPath is where the output lands. It differs from SourcePath only in
	// the extension, and only when the profile forces a container change --
	// the one rename this tool performs. See README's safety rules.
	FinalPath string

	// TempPath is the in-progress output. It is a hidden file whose name
	// contains ".partial." so that the scanner's default ignore globs skip a
	// leftover from a crashed run rather than treating it as a new arrival.
	TempPath string

	// TempInWorkDir records whether the configured work dir was usable, i.e.
	// whether it is on the same filesystem as the source.
	TempInWorkDir bool

	// Args is the complete ffmpeg argument list, excluding the binary itself.
	Args []string

	// Hardlinks is the source's link count. Above 1 means another path (very
	// often the *arr download directory) holds the same data, so replacing
	// this file will not actually free anything until that link goes too.
	// Worth reporting; not a reason to decline.
	Hardlinks uint64
}

// Renames reports whether carrying out this job changes the file's name.
func (j *Job) Renames() bool { return j.FinalPath != j.SourcePath }

// Prepare works out the paths and arguments for a plan without touching
// anything except the stat() calls it needs to answer two questions: is the
// work dir on the right filesystem, and is there anything in the way.
func (e Encoder) Prepare(r *probe.Result, plan decide.Plan) (*Job, error) {
	if r == nil || r.Path == "" {
		return nil, errors.New("prepare: no probed source")
	}
	switch plan.Action {
	case decide.ActionNone:
		return nil, ErrNothingToDo
	case decide.ActionError:
		return nil, fmt.Errorf("prepare %s: plan is an error: %s", r.Path, plan.Reason)
	}

	src := r.Path

	// A symlinked library entry cannot be replaced in place. Renaming over
	// the link would leave a regular file at this path and orphan the real
	// file wherever it lives, which is exactly the kind of quiet
	// reorganisation this tool must never do. Declining is cheap; explaining
	// it afterwards is not.
	if info, err := os.Lstat(src); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", src, err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(src)
		return nil, fmt.Errorf("refusing %s: it is a symlink to %s, and replacing it in place "+
			"would destroy the link and leave the real file behind", src, target)
	}

	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("prepare %s: %w", src, err)
	}

	final := src
	if plan.ExtensionChanges() {
		final = strings.TrimSuffix(src, filepath.Ext(src)) + plan.Container.Extension()
	}
	if final != src {
		// Converting Foo.mp4 to Foo.mkv when a Foo.mkv already exists would
		// destroy that file. It is somebody else's, whatever it is.
		if _, err := os.Lstat(final); err == nil {
			return nil, fmt.Errorf("refusing to convert %s: %s already exists", src, final)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("prepare %s: checking %s: %w", src, final, err)
		}
	}

	temp, inWorkDir := e.tempPath(src, plan.Container)

	job := &Job{
		Plan:          plan,
		Source:        r,
		SourcePath:    src,
		FinalPath:     final,
		TempPath:      temp,
		TempInWorkDir: inWorkDir,
		Hardlinks:     linkCount(info),
	}
	job.Args = e.args(plan, src, temp)
	return job, nil
}

// args assembles the full ffmpeg command line.
//
// The plan supplies everything that determines quality and nothing that
// determines location: InputArgs go before -i and OutputArgs after it, and
// this function is the only place that knows the paths.
func (e Encoder) args(plan decide.Plan, src, dst string) []string {
	args := []string{
		"-hide_banner",
		"-nostdin", // never wait for a keypress; this runs unattended
		"-loglevel", "warning",
		"-y", // the destination is our own temp file, so overwriting is safe
		"-nostats",
		"-progress", "pipe:1",
	}
	args = append(args, plan.InputArgs...)
	args = append(args, "-i", src)
	args = append(args, plan.OutputArgs...)
	return append(args, dst)
}

// Binary returns the ffmpeg that will be run.
func (e Encoder) binary() string {
	if e.Binary == "" {
		return DefaultBinary
	}
	return e.Binary
}

// Command renders the job as a copy-pasteable shell command, for --dry-run.
func (j *Job) Command(binary string) string {
	if binary == "" {
		binary = DefaultBinary
	}
	parts := make([]string, 0, len(j.Args)+1)
	parts = append(parts, shellQuote(binary))
	for _, a := range j.Args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// Result is what happened.
type Result struct {
	Job *Job

	Elapsed time.Duration

	// Verification is the measurement of the output, populated whether or not
	// it passed.
	Verification Verification

	// Replaced is true only once the output is in place at FinalPath.
	Replaced bool

	// SourceRemoved is true when a container change meant the output landed
	// under a different name and the original had to be deleted separately.
	SourceRemoved bool

	// TempKept is the temp file left behind for inspection after a failure,
	// or "" when there is nothing to look at.
	TempKept string

	// FFmpegOutput is the tail of ffmpeg's stderr, kept for the failure
	// message and for the history table in Phase 4.
	FFmpegOutput string

	// LastProgress is the final progress report ffmpeg made.
	LastProgress Progress

	// Notes are things that happened which are worth reading but did not
	// stop the replacement.
	Notes []string
}

// Run carries out a prepared job.
//
// The sequence is the safety property, so it is worth stating plainly:
// encode to a temp file, probe and measure the temp file, confirm the source
// has not changed underneath us, then rename. Any failure before the rename
// leaves the original exactly as it was.
func (e Encoder) Run(ctx context.Context, job *Job) (*Result, error) {
	res := &Result{Job: job}

	// Recorded now and re-checked immediately before the rename. Files arrive
	// on this server by themselves, and an *arr stack upgrading the source
	// mid-encode would otherwise have its new file silently overwritten by
	// our re-encode of the old one.
	before, err := os.Stat(job.SourcePath)
	if err != nil {
		return res, fmt.Errorf("encode %s: %w", job.SourcePath, err)
	}

	start := time.Now()
	err = e.runFFmpeg(ctx, job, res)
	res.Elapsed = time.Since(start)
	if err != nil {
		// A cancelled run is a shutdown, not a failure worth preserving
		// evidence of, and leaving a partial file behind on every restart
		// would litter the library.
		if ctx.Err() != nil {
			os.Remove(job.TempPath)
			return res, fmt.Errorf("encode %s: cancelled: %w", job.SourcePath, ctx.Err())
		}
		res.TempKept = job.TempPath
		return res, fmt.Errorf("encode %s: %w", job.SourcePath, err)
	}

	// Verify before considering the source expendable.
	out, probeErr := e.Prober.Run(ctx, job.TempPath)
	res.Verification = verify(job, out, probeErr, e.Verify)
	if !res.Verification.OK {
		res.TempKept = job.TempPath
		return res, fmt.Errorf("verifying %s: %s (output kept at %s)",
			job.SourcePath, strings.Join(res.Verification.Problems, "; "), job.TempPath)
	}

	if err := unchanged(job.SourcePath, before); err != nil {
		res.TempKept = job.TempPath
		return res, fmt.Errorf("not replacing %s: %w (output kept at %s)",
			job.SourcePath, err, job.TempPath)
	}

	// A rename gives the new file the temp's mode and owner, which are this
	// process's, not the library's. Copy them across first so the file the
	// library ends up with looks like the one it had.
	if note := adoptOwnership(job.TempPath, before); note != "" {
		res.Notes = append(res.Notes, note)
	}

	if err := os.Rename(job.TempPath, job.FinalPath); err != nil {
		res.TempKept = job.TempPath
		return res, fmt.Errorf("replacing %s: %w", job.FinalPath, err)
	}
	res.Replaced = true

	// Rule 4: the source is deleted only after a verified successful replace,
	// and only when the replacement landed under a different name. When the
	// name is unchanged the rename above already consumed it.
	if job.Renames() {
		if err := os.Remove(job.SourcePath); err != nil {
			return res, fmt.Errorf("wrote %s but could not remove the original %s: %w",
				job.FinalPath, job.SourcePath, err)
		}
		res.SourceRemoved = true
	}

	if job.Hardlinks > 1 {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"the original had %d hard links, so the old data stays on disk until the other link(s) go",
			job.Hardlinks))
	}
	return res, nil
}

// runFFmpeg runs the encode itself, streaming progress and keeping the tail of
// stderr for the error message.
func (e Encoder) runFFmpeg(ctx context.Context, job *Job, res *Result) error {
	if job.TempInWorkDir {
		if err := os.MkdirAll(filepath.Dir(job.TempPath), 0o755); err != nil {
			return fmt.Errorf("work dir: %w", err)
		}
	}

	cmd := exec.CommandContext(ctx, e.binary(), job.Args...)
	stderr := &tail{limit: 8 << 10}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", e.binary(), err)
	}

	// Read to EOF before Wait, which is what StdoutPipe requires.
	readProgress(stdout, job.Source.DurationSeconds(), job.Source.FrameRate(), func(p Progress) {
		res.LastProgress = p
		if e.OnProgress != nil {
			e.OnProgress(p)
		}
	})

	waitErr := cmd.Wait()
	res.FFmpegOutput = stderr.String()
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("ffmpeg failed: %s", msg)
	}
	return nil
}

// unchanged reports whether the source is still the file that was probed.
func unchanged(path string, before fs.FileInfo) error {
	after, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("it disappeared while it was being encoded: %w", err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("it changed while it was being encoded")
	}
	return nil
}

// shellQuote is enough quoting to make a printed command safe to paste. Media
// filenames are full of spaces, quotes, brackets and apostrophes.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '+' || r == '=' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
