// Package runner is the thing that actually happens when you type a command:
// walk the roots, remember what was seen, decide what each file needs, and
// hand the work to the two pools.
//
// It exists as a package rather than as part of main because Phase 5's web UI
// has to be able to start exactly the same pass from a button, and because
// the order of the steps below is where the safety rules are enforced --
// which makes it worth testing directly.
//
// The three modes are one code path with two switches in it, on purpose:
//
//	plan  decide and report. Writes nothing, anywhere -- not to the media,
//	      and not to the database either. A dry run that quietly recorded
//	      state would make `run` after `plan` behave differently from `run`
//	      alone, and then --dry-run would no longer be telling the truth.
//	scan  decide and remember. Touches no media, but records sightings and
//	      caches decisions, which is what makes files eligible and what makes
//	      the next scan cheap.
//	run   scan, and then do the work.
package runner

import (
	"context"
	"sync"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/probe"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/scan"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// Mode is how far a pass goes. See the package comment.
type Mode string

const (
	ModePlan Mode = "plan"
	ModeScan Mode = "scan"
	ModeRun  Mode = "run"
)

// Target is one set of roots and the profile that governs them.
type Target struct {
	// Label is how the target is described in output.
	Label   string
	Library string
	Roots   []string
	Profile decide.Profile

	// Named is set when the operator listed these paths themselves rather
	// than naming a library. It waives the settle check: somebody typed this
	// path, so they know the file is there and are not waiting on it to
	// finish arriving.
	Named bool
}

// Options are the knobs one pass takes.
type Options struct {
	Mode  Mode
	Limit int

	Workers queue.Workers

	// ProbeWorkers is how many ffprobes run at once. Probing is I/O bound and
	// short, and the first scan of a large library is 20,000 of them.
	ProbeWorkers int

	// IncludeUnsettled acts on files that have not yet been seen unchanged by
	// a second scan.
	IncludeUnsettled bool

	// RetryFailed ignores the backoff on files that have failed before.
	RetryFailed bool

	// Sweep removes database rows for files no longer on disk. Only correct
	// for a pass that walked whole libraries.
	Sweep bool
}

// Runner carries the collaborators. Store may be nil, in which case nothing
// is remembered between passes and every file looks like a new arrival --
// which is exactly what `plan` wants when there is no database yet.
type Runner struct {
	Store   *store.Store
	Prober  probe.Prober
	Encoder encode.Encoder

	// Scan is the walk configuration: extensions, ignore globs, and the grace
	// period. MinAge is used twice, once by the walk and once by the settle
	// check, which is deliberate -- they are the two halves of one rule.
	Scan scan.Options

	// Now is the clock, injectable for tests.
	Now func() time.Time

	// OnEvent, if set, is called as the pass proceeds. It is called from
	// several goroutines at once and must be safe for that.
	OnEvent func(Event)
}

// Outcome is what happened to one file, whether or not anything was done to
// it. Declining is the most common result in this project and is recorded as
// fully as an encode: "what did it not touch, and why" is the question the
// operator actually has six months later.
type Outcome struct {
	Path  string
	State store.FileState
	Plan  decide.Plan
	Probe *probe.Result

	// FromCache is set when the decision came from the database and the file
	// was not probed at all. On a settled library this is almost every file,
	// and it is the difference between a scan taking seconds and an hour.
	FromCache bool

	// Job is the prepared work, or nil when there is nothing to do.
	Job    *encode.Job
	Queued bool

	// Declined is set when this tool chose not to act on the file: a plan
	// decide could not make, or a file encode refuses to replace in place.
	// Not a failure, and it does not affect the exit status.
	Declined string

	// Waiting is set when the file is eligible in principle but not yet: it
	// has not settled, or a previous failure is still backing off. On a plan
	// it is still decided and reported -- "not yet" is an answer about when,
	// not about what.
	Waiting string

	Err    error
	Result *encode.Result
}

// Summary is one pass.
type Summary struct {
	Target   Target
	Mode     Mode
	Outcomes []Outcome

	// Unscanned are paths the walk declined before anything was probed.
	Unscanned []scan.Skipped

	// WalkErrors are directories that could not be read. A pass keeps going
	// past them; one unreadable folder should not stop a library.
	WalkErrors []error

	Queue          *queue.Queue
	StoppedAtLimit bool
	Swept          int
	Elapsed        time.Duration
	Cancelled      bool
}

// Failures counts files that could not be read or whose work went wrong.
// Files this tool declined are not failures.
func (s *Summary) Failures() int {
	n := 0
	for _, o := range s.Outcomes {
		if o.Err != nil {
			n++
		}
	}
	return n
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) emit(e Event) {
	if r.OnEvent != nil {
		r.OnEvent(e)
	}
}

// Pass runs one complete cycle over a target.
func (r *Runner) Pass(ctx context.Context, t Target, o Options) (*Summary, error) {
	started := time.Now()
	now := r.now()
	sum := &Summary{Target: t, Mode: o.Mode}

	found := scan.Walk(t.Roots, r.Scan)
	sum.Unscanned = found.Skipped
	sum.WalkErrors = found.Errors
	r.emit(Event{Phase: PhaseWalked, Count: len(found.Candidates)})

	states, err := r.remember(ctx, found.Candidates, o, now)
	if err != nil {
		return nil, err
	}

	fingerprint := store.ProfileFingerprint(t.Profile)
	sum.Outcomes = make([]Outcome, len(found.Candidates))

	var toProbe []int
	for i, c := range found.Candidates {
		out := &sum.Outcomes[i]
		out.Path = c.Path

		st, ok := states[c.Path]
		if !ok {
			// Either there is no database, or this is `plan`, which reads the
			// store but never writes to it. Both mean the same thing here:
			// nothing is known about this file yet.
			st = store.FileState{Path: c.Path, Size: c.Size, ModTime: c.ModTime, New: true}
		}
		out.State = st

		if blocked, why := st.Blocked(now); blocked && !o.RetryFailed {
			out.Waiting = why
		} else if !t.Named && !o.IncludeUnsettled {
			if settled, why := st.Settled(r.Scan.MinAge, now); !settled {
				out.Waiting = why
			}
		}

		// A file that is not eligible yet is still planned on a dry run,
		// because "what would happen to this library" is the question plan
		// answers. In every other mode there is nothing to gain from probing
		// a file that is still moving.
		if out.Waiting != "" && o.Mode != ModePlan {
			continue
		}

		// The decision cache only ever short-circuits "there is nothing to
		// do". Anything with work in it is re-probed, because the ffmpeg
		// arguments have to be derived from the file as it is right now, not
		// from what it looked like at the last scan.
		if d, ok := st.CachedDecision(fingerprint); ok && d.Action == decide.ActionNone {
			out.Plan = d.Plan()
			out.FromCache = true
			continue
		}
		toProbe = append(toProbe, i)
	}

	r.probeAll(ctx, sum, toProbe, t, fingerprint, o, now)

	q := queue.New(o.Workers)
	added := 0
	for i := range sum.Outcomes {
		out := &sum.Outcomes[i]
		if out.Job == nil || out.Waiting != "" {
			continue
		}
		if o.Limit > 0 && added >= o.Limit {
			sum.StoppedAtLimit = true
			break
		}
		q.Add(queue.Item{
			Index:   i,
			Path:    out.Path,
			Library: t.Library,
			Profile: t.Profile.Name,
			Kind:    queue.KindFor(out.Plan.Action),
			Job:     out.Job,
		})
		out.Queued = true
		added++
	}
	sum.Queue = q

	if o.Mode == ModeRun {
		r.emit(Event{Phase: PhaseQueued, Count: q.Len()})
		q.Run(ctx, func(ctx context.Context, it queue.Item) error {
			return r.execute(ctx, q, it, &sum.Outcomes[it.Index], t)
		})
	}

	// Sweeping is skipped when any directory failed to read. A mount that
	// dropped out for a moment looks exactly like a library somebody deleted,
	// and forgetting 20,000 files because of a transient NFS hiccup would
	// mean re-probing all of them and waiting two scans for any of them to
	// settle again.
	if o.Sweep && o.Mode != ModePlan && r.Store != nil && len(found.Errors) == 0 && ctx.Err() == nil {
		n, err := r.Store.Sweep(ctx, t.Roots, now)
		if err != nil {
			return nil, err
		}
		sum.Swept = n
	}

	sum.Elapsed = time.Since(started)
	sum.Cancelled = ctx.Err() != nil
	return sum, nil
}

// remember records the sighting of every candidate, except on a dry run,
// which only reads.
func (r *Runner) remember(ctx context.Context, candidates []scan.Candidate, o Options, now time.Time) (map[string]store.FileState, error) {
	if r.Store == nil {
		return map[string]store.FileState{}, nil
	}

	if o.Mode == ModePlan {
		paths := make([]string, len(candidates))
		for i, c := range candidates {
			paths[i] = c.Path
		}
		return r.Store.Lookup(ctx, paths)
	}

	seen := make([]store.Sighting, len(candidates))
	for i, c := range candidates {
		seen[i] = store.Sighting{Path: c.Path, Size: c.Size, ModTime: c.ModTime}
	}
	states, err := r.Store.Observe(ctx, seen, now)
	if err != nil {
		return nil, err
	}

	if o.RetryFailed {
		var blocked []string
		for path, st := range states {
			if st.Failures > 0 {
				blocked = append(blocked, path)
			}
		}
		if err := r.Store.ClearFailures(ctx, blocked...); err != nil {
			return nil, err
		}
	}
	return states, nil
}

// probeAll probes and decides the files that need it, several at a time.
func (r *Runner) probeAll(ctx context.Context, sum *Summary, indexes []int, t Target, fingerprint string, o Options, now time.Time) {
	if len(indexes) == 0 {
		return
	}
	workers := o.ProbeWorkers
	if workers < 1 {
		workers = 1
	}
	if workers > len(indexes) {
		workers = len(indexes)
	}

	feed := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Draining rather than returning early on cancellation: the
			// sender below must not be left blocked on a channel nobody is
			// reading.
			for i := range feed {
				if ctx.Err() != nil {
					continue
				}
				r.decide(ctx, &sum.Outcomes[i], t, fingerprint, o, now)
			}
		}()
	}
	for _, i := range indexes {
		feed <- i
	}
	close(feed)
	wg.Wait()
}

// decide probes one file, decides what it needs, caches the answer, and
// prepares the job.
func (r *Runner) decide(ctx context.Context, out *Outcome, t Target, fingerprint string, o Options, now time.Time) {
	r.emit(Event{Phase: PhaseProbe, Path: out.Path})

	result, err := r.Prober.Run(ctx, out.Path)
	if err != nil {
		out.Err = err
		// A file ffprobe cannot read is not going to become readable by
		// itself, and without this it is re-probed and re-reported on every
		// scan for as long as it sits there -- which is how an unattended
		// run ends up permanently failing and permanently ignored. It backs
		// off exactly like a failed encode does.
		if o.Mode != ModePlan && r.Store != nil {
			if rerr := r.Store.RecordFailure(ctx, out.Path, out.State.Size, out.State.ModTime,
				err.Error(), now); rerr != nil {
				r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: rerr})
			}
		}
		return
	}
	out.Probe = result
	out.Plan = decide.Decide(result, t.Profile)

	if o.Mode != ModePlan && r.Store != nil {
		// A failure to cache is not a failure of the pass: the cost is one
		// re-probe next time, which is exactly what would have happened
		// anyway before this table existed.
		err := r.Store.RecordDecision(ctx, out.Path, out.State.Size, out.State.ModTime, store.Decision{
			Profile:     t.Profile.Name,
			Fingerprint: fingerprint,
			DecidedAt:   now,
			Action:      out.Plan.Action,
			Video:       out.Plan.Video,
			Reason:      out.Plan.Reason,
			SourceKbps:  out.Plan.Bitrate.SourceKbps,
			TargetKbps:  out.Plan.Bitrate.TargetKbps,
			Notes:       out.Plan.Notes,
		})
		if err != nil {
			r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: err})
		}
	}

	switch out.Plan.Action {
	case decide.ActionNone:
		return
	case decide.ActionError:
		out.Declined = out.Plan.Reason
		return
	}

	job, err := r.Encoder.Prepare(result, out.Plan)
	if err != nil {
		// Prepare refuses only for reasons about this file being unsafe to
		// replace -- a symlink, something already in the way -- never about
		// the encode itself. Those are declines, not failures.
		out.Declined = err.Error()
		return
	}
	out.Job = job
}

// execute carries out one queued job and records what happened.
func (r *Runner) execute(ctx context.Context, q *queue.Queue, it queue.Item, out *Outcome, t Target) error {
	started := r.now()
	r.emit(Event{Phase: PhaseStart, Item: &it})

	var jobID int64
	if r.Store != nil {
		id, err := r.Store.StartJob(ctx, store.JobStart{
			Path:       it.Job.SourcePath,
			FinalPath:  it.Job.FinalPath,
			Library:    t.Library,
			Profile:    t.Profile.Name,
			Action:     out.Plan.Action,
			SizeBefore: out.State.Size,
			StartedAt:  started,
		})
		if err != nil {
			r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: err})
		}
		jobID = id
	}

	// Encoder is a value type, so each worker takes its own copy and hangs
	// its own progress callback on it. Sharing one callback across a pool
	// would interleave reports from several files with no way to tell them
	// apart.
	enc := r.Encoder
	enc.OnProgress = func(p encode.Progress) {
		q.SetProgress(it.Index, p)
		r.emit(Event{Phase: PhaseProgress, Item: &it, Progress: p})
	}

	res, err := enc.Run(ctx, it.Job)
	out.Result = res
	out.Err = err
	finished := r.now()

	if r.Store != nil {
		if jobID != 0 {
			var sizeAfter int64
			if res != nil {
				sizeAfter = res.Verification.OutputSize
			}
			// The plan's notes are kept alongside the encoder's. A note
			// means a safety rule overrode the configuration, and the row
			// that recorded it is deleted the moment the file is replaced --
			// so without this, the notes worth reading are exactly the ones
			// that disappear.
			notes := append([]string(nil), out.Plan.Notes...)
			if res != nil {
				notes = append(notes, res.Notes...)
			}
			if ferr := r.Store.FinishJob(ctx, jobID, store.JobFinish{
				FinishedAt: finished,
				Elapsed:    finished.Sub(started),
				SizeAfter:  sizeAfter,
				Err:        err,
				FFmpegTail: ffmpegTail(res),
				Notes:      notes,
			}); ferr != nil {
				r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: ferr})
			}
		}

		switch {
		case err == nil && res != nil && res.Replaced:
			// The bytes on disk are new, so everything remembered about this
			// path is about a file that no longer exists. Both names are
			// forgotten: a container change means the work landed under the
			// second one.
			if ferr := r.Store.Forget(ctx, it.Job.SourcePath, it.Job.FinalPath); ferr != nil {
				r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: ferr})
			}
		case err != nil && ctx.Err() == nil:
			// A cancelled run is a shutdown, not a failure of the file, and
			// must not count against it -- otherwise restarting the container
			// a few times would give up on whatever was mid-encode.
			if ferr := r.Store.RecordFailure(ctx, out.Path, out.State.Size, out.State.ModTime,
				err.Error(), finished); ferr != nil {
				r.emit(Event{Phase: PhaseWarning, Path: out.Path, Err: ferr})
			}
		}
	}

	r.emit(Event{Phase: PhaseDone, Item: &it, Result: res, Err: err})
	return err
}

func ffmpegTail(res *encode.Result) string {
	if res == nil {
		return ""
	}
	return res.FFmpegOutput
}
