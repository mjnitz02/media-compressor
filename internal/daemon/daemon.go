// Package daemon is the loop: pass over every library, wait, do it again.
//
// It exists as a package rather than as a for-loop in main because the web UI
// has to be able to see what the loop is doing while it is doing it, and to
// ask it to start a pass now. Both of those need somewhere to keep "what is
// happening right now", and a local variable in main is not somewhere another
// goroutine can read.
//
// Two properties matter more than anything clever this could do instead.
//
// It must never wedge. Every failure is recorded and stepped over -- a library
// that cannot be read, a file ffprobe chokes on, an encode that goes wrong --
// because the alternative is a service that stopped making progress in March
// and nobody noticed until September.
//
// And it must be interruptible. A cancelled context finishes the files in
// flight and stops starting new ones, which matters because Unraid stops
// containers with a SIGTERM and an encode killed half-way leaves a temp file.
package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/runner"
)

// Phase is what the loop is doing, in the words the UI shows.
type Phase string

const (
	// PhaseIdle is waiting for the next pass. It is the normal state: this
	// thing spends most of its life having nothing to do.
	PhaseIdle Phase = "idle"
	// PhaseWalking is looking at the filesystem.
	PhaseWalking Phase = "walking"
	// PhaseDeciding is probing files and working out what they need.
	PhaseDeciding Phase = "deciding"
	// PhaseWorking is ffmpeg actually running.
	PhaseWorking Phase = "working"
)

// defaultHistory is how many finished passes are remembered. Enough to see
// what happened overnight; the database has the real history.
const defaultHistory = 20

// Daemon runs passes and keeps a live picture of them.
//
// It takes ownership of Runner.OnEvent -- that is how it knows what is
// happening. Set the Daemon's own OnEvent to watch as well; the terminal
// output does exactly that.
type Daemon struct {
	Runner  *runner.Runner
	Targets []runner.Target
	Options runner.Options

	// Interval is how long after a pass finishes the next one starts. It is
	// measured from the end and not the start, because an encode that took
	// six hours should not be followed immediately by another scan.
	Interval time.Duration

	// MinWait is the floor under that gap, so that a library which finishes
	// instantly cannot spin.
	MinWait time.Duration

	// HistoryLimit is how many finished passes Activity reports.
	HistoryLimit int

	// Now is the clock, injectable for tests.
	Now func() time.Time

	// OnEvent and OnPass are for whoever is watching -- the terminal, and in
	// principle anything else. OnEvent is called from several goroutines at
	// once and must be safe for that.
	OnEvent func(runner.Event)
	OnPass  func(runner.Target, *runner.Summary, error)

	setup   sync.Once
	trigger chan struct{}

	mu  sync.Mutex
	act Activity
}

// Running is one file being worked on right now.
type Running struct {
	// index is the queue's own handle on this item. Matching on it rather
	// than on the path is what makes a rename safe: an item whose output
	// lands under a new extension is still the same running job.
	index int

	Path     string
	Library  string
	Kind     queue.Kind
	Started  time.Time
	Progress encode.Progress
}

// Warning is something that went wrong without stopping the pass -- almost
// always the database, which is not worth failing a run over but is worth
// seeing if it keeps happening.
type Warning struct {
	When time.Time
	Path string
	Err  string
}

// PassRecord is one finished pass over one target, reduced to the numbers
// worth keeping. Declined and Waiting are in here beside the failures on
// purpose: in this project "left it alone" is the commonest outcome and the
// one most worth being able to see at a glance.
type PassRecord struct {
	Target  string
	Library string
	Started time.Time
	Elapsed time.Duration

	Considered int
	Encoded    int
	Remuxed    int
	Declined   int
	Waiting    int
	Failed     int
	Swept      int
	Saved      int64

	Cancelled bool
	Err       string
}

// Activity is everything the status page shows about the loop itself. It is a
// value, copied under the lock, so a page render cannot see it half-updated.
type Activity struct {
	Since  time.Time
	Passes int

	Phase   Phase
	Target  string
	Library string

	PassStarted time.Time
	NextPass    time.Time

	// Considered, Probed and Queued describe the pass in flight. Probed is
	// the one that moves during the long silent part of a first scan, when
	// 20,000 files are being read and nothing has been queued yet.
	Considered int
	Probed     int
	Queued     int

	Running []Running
	Done    int
	Failed  int

	Warnings []Warning
	Recent   []PassRecord
}

// Busy reports whether a pass is in flight. The zero Activity is idle, which
// is what a daemon that has not started yet honestly is.
func (a Activity) Busy() bool { return a.Phase != PhaseIdle && a.Phase != "" }

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Daemon) init() {
	d.setup.Do(func() {
		// Buffered by one: a trigger while a pass is running means "go again
		// when this finishes", and a second one before then is the same
		// request, not two passes.
		d.trigger = make(chan struct{}, 1)
		d.mu.Lock()
		if d.act.Since.IsZero() {
			d.act.Since = d.now()
		}
		d.mu.Unlock()
		if d.Runner != nil {
			d.Runner.OnEvent = d.Observe
		}
	})
}

// Trigger asks for a pass to start now, or as soon as the current one
// finishes. It reports whether the request was new; a second trigger while one
// is already pending is the same request rather than a second pass.
func (d *Daemon) Trigger() bool {
	d.init()
	select {
	case d.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

// Activity is the live picture, safe to call from any goroutine at any time.
func (d *Daemon) Activity() Activity {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()

	a := d.act
	if a.Phase == "" {
		a.Phase = PhaseIdle
	}
	a.Running = append([]Running(nil), d.act.Running...)
	a.Warnings = append([]Warning(nil), d.act.Warnings...)
	a.Recent = append([]PassRecord(nil), d.act.Recent...)
	// Oldest first is the order they started in, which is the order somebody
	// watching saw them appear.
	sort.Slice(a.Running, func(i, j int) bool {
		if !a.Running[i].Started.Equal(a.Running[j].Started) {
			return a.Running[i].Started.Before(a.Running[j].Started)
		}
		return a.Running[i].Path < a.Running[j].Path
	})
	return a
}

// Run loops until the context is cancelled.
func (d *Daemon) Run(ctx context.Context) {
	d.init()
	for {
		started := d.now()
		d.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		wait := d.Interval - d.now().Sub(started)
		if min := d.minWait(); wait < min {
			wait = min
		}
		d.setNextPass(d.now().Add(wait))

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-d.trigger:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// RunOnce makes a single pass over every target. A target that fails is
// reported and the rest still run: one unreadable library must not stop the
// others.
func (d *Daemon) RunOnce(ctx context.Context) {
	d.init()
	d.beginPass()
	defer d.endPass()

	for _, t := range d.Targets {
		if ctx.Err() != nil {
			return
		}
		d.beginTarget(t)
		sum, err := d.Runner.Pass(ctx, t, d.Options)
		d.recordPass(t, sum, err)
		if d.OnPass != nil {
			d.OnPass(t, sum, err)
		}
	}
}

// Observe is the runner's event hook. It keeps the live picture up to date and
// then hands the event on to whoever else is watching.
func (d *Daemon) Observe(e runner.Event) {
	d.mu.Lock()
	switch e.Phase {
	case runner.PhaseWalked:
		d.act.Considered = e.Count
		d.act.Phase = PhaseDeciding

	case runner.PhaseProbe:
		d.act.Probed++

	case runner.PhaseQueued:
		d.act.Queued = e.Count
		if e.Count > 0 {
			d.act.Phase = PhaseWorking
		}

	case runner.PhaseStart:
		if e.Item != nil {
			d.act.Running = append(d.act.Running, Running{
				index:   e.Item.Index,
				Path:    e.Item.Path,
				Library: e.Item.Library,
				Kind:    e.Item.Kind,
				Started: d.now(),
			})
		}

	case runner.PhaseProgress:
		if e.Item != nil {
			for i := range d.act.Running {
				if d.act.Running[i].index == e.Item.Index {
					d.act.Running[i].Progress = e.Progress
					break
				}
			}
		}

	case runner.PhaseDone:
		if e.Item != nil {
			for i := range d.act.Running {
				if d.act.Running[i].index == e.Item.Index {
					d.act.Running = append(d.act.Running[:i], d.act.Running[i+1:]...)
					break
				}
			}
		}
		if e.Err != nil {
			d.act.Failed++
		} else {
			d.act.Done++
		}

	case runner.PhaseWarning:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		d.act.Warnings = append(d.act.Warnings, Warning{When: d.now(), Path: e.Path, Err: msg})
		if n := len(d.act.Warnings); n > defaultHistory {
			d.act.Warnings = d.act.Warnings[n-defaultHistory:]
		}
	}
	d.mu.Unlock()

	if d.OnEvent != nil {
		d.OnEvent(e)
	}
}

func (d *Daemon) beginPass() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.act.Passes++
	d.act.PassStarted = d.now()
	d.act.NextPass = time.Time{}
	d.act.Phase = PhaseWalking
	d.act.Considered, d.act.Probed, d.act.Queued = 0, 0, 0
	d.act.Done, d.act.Failed = 0, 0
}

func (d *Daemon) beginTarget(t runner.Target) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.act.Target, d.act.Library = t.Label, t.Library
	d.act.Phase = PhaseWalking
	// Per-target, not per-pass: two libraries in one pass are two walks, and
	// showing the first one's counts while the second is running would be a
	// lie about which files are being looked at. The finished counts reset
	// with them, for the same reason -- everything on the page is about the
	// library named at the top of it.
	d.act.Considered, d.act.Probed, d.act.Queued = 0, 0, 0
	d.act.Done, d.act.Failed = 0, 0
}

func (d *Daemon) endPass() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.act.Phase = PhaseIdle
	d.act.Target, d.act.Library = "", ""
	d.act.Running = nil
}

func (d *Daemon) setNextPass(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.act.NextPass = t
}

// recordPass reduces a finished pass to the handful of numbers the history
// keeps.
func (d *Daemon) recordPass(t runner.Target, sum *runner.Summary, err error) {
	rec := PassRecord{
		Target:  t.Label,
		Library: t.Library,
		Started: d.now(),
	}
	if err != nil {
		rec.Err = err.Error()
	}
	if sum != nil {
		rec.Started = d.now().Add(-sum.Elapsed)
		rec.Elapsed = sum.Elapsed
		rec.Considered = len(sum.Outcomes)
		rec.Swept = sum.Swept
		rec.Cancelled = sum.Cancelled
		for i := range sum.Outcomes {
			o := &sum.Outcomes[i]
			switch {
			case o.Err != nil:
				rec.Failed++
			case o.Declined != "":
				rec.Declined++
			case o.Waiting != "":
				rec.Waiting++
			}
			if o.Result == nil || !o.Result.Replaced {
				continue
			}
			if o.Plan.Action == decide.ActionEncode {
				rec.Encoded++
			} else {
				rec.Remuxed++
			}
			rec.Saved += o.Result.Verification.SourceSize - o.Result.Verification.OutputSize
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	limit := d.HistoryLimit
	if limit <= 0 {
		limit = defaultHistory
	}
	d.act.Recent = append([]PassRecord{rec}, d.act.Recent...)
	if len(d.act.Recent) > limit {
		d.act.Recent = d.act.Recent[:limit]
	}
}

func (d *Daemon) minWait() time.Duration {
	if d.MinWait > 0 {
		return d.MinWait
	}
	return time.Minute
}
