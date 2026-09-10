package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/runner"
	"github.com/mjnitz02/media-compressor/internal/scan"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// emptyLibrary is a directory with nothing in it a scan would look at, which
// is enough for every test about the loop itself: a pass over it does no I/O
// beyond the walk and needs neither ffmpeg nor a database.
func emptyLibrary(t *testing.T, label string) runner.Target {
	t.Helper()
	return runner.Target{Label: label, Library: label, Roots: []string{t.TempDir()}}
}

func newDaemon(t *testing.T, targets ...runner.Target) *Daemon {
	t.Helper()
	return &Daemon{
		Runner:  &runner.Runner{Scan: scan.Options{Extensions: []string{"mkv"}}},
		Targets: targets,
		MinWait: time.Millisecond,
	}
}

func TestOnePassVisitsEveryTarget(t *testing.T) {
	d := newDaemon(t, emptyLibrary(t, "movies"), emptyLibrary(t, "tv"))

	var seen []string
	d.OnPass = func(tgt runner.Target, sum *runner.Summary, err error) {
		if err != nil {
			t.Errorf("%s: %v", tgt.Label, err)
		}
		seen = append(seen, tgt.Library)
	}
	d.RunOnce(context.Background())

	if len(seen) != 2 || seen[0] != "movies" || seen[1] != "tv" {
		t.Fatalf("visited %v, want [movies tv]", seen)
	}
	if a := d.Activity(); a.Busy() {
		t.Errorf("still busy after the pass finished: %s", a.Phase)
	}
}

// A library that cannot be read must not take the others down with it. This
// is the property that keeps an unattended service making progress.
func TestOneTargetFailingDoesNotStopTheRest(t *testing.T) {
	// A closed database is the simplest way to make a pass fail for a reason
	// that has nothing to do with the file it is looking at.
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "film.mkv"), []byte("not really media"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDaemon(t,
		runner.Target{Label: "broken", Library: "broken", Roots: []string{broken}},
		emptyLibrary(t, "fine"),
	)
	d.Runner.Store = db
	d.Options = runner.Options{Mode: runner.ModeScan}

	errs := map[string]error{}
	d.OnPass = func(tgt runner.Target, _ *runner.Summary, err error) { errs[tgt.Library] = err }
	d.RunOnce(context.Background())

	if errs["broken"] == nil {
		t.Error("the unreadable library reported no error")
	}
	if _, ran := errs["fine"]; !ran {
		t.Error("the second library never ran")
	} else if errs["fine"] != nil {
		t.Errorf("the second library failed: %v", errs["fine"])
	}
}

func TestTriggerStartsAPassWithoutWaitingForTheInterval(t *testing.T) {
	d := newDaemon(t, emptyLibrary(t, "movies"))
	// An interval nothing in this test could wait out: only the trigger can
	// produce a second pass.
	d.Interval = time.Hour
	d.MinWait = time.Hour

	passes := make(chan struct{}, 4)
	d.OnPass = func(runner.Target, *runner.Summary, error) { passes <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	waitFor(t, passes, "the first pass")
	if !d.Trigger() {
		t.Fatal("Trigger was refused")
	}
	waitFor(t, passes, "the triggered pass")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not stop when the context was cancelled")
	}
}

func TestASecondTriggerIsTheSameRequest(t *testing.T) {
	d := newDaemon(t, emptyLibrary(t, "movies"))
	if !d.Trigger() {
		t.Fatal("the first trigger was refused")
	}
	if d.Trigger() {
		t.Error("a second trigger queued a second pass; it should be the same request")
	}
}

// The status page is fed entirely by runner events, so what it can show is
// exactly what the events carry. This pins that.
func TestActivityFollowsARunningJob(t *testing.T) {
	d := newDaemon(t)
	item := queue.Item{Index: 0, Path: "/lib/film.mkv", Library: "movies", Kind: queue.KindEncode}
	other := queue.Item{Index: 1, Path: "/lib/show.mkv", Library: "movies", Kind: queue.KindRemux}

	d.beginPass()
	d.Observe(runner.Event{Phase: runner.PhaseWalked, Count: 1200})
	d.Observe(runner.Event{Phase: runner.PhaseProbe, Path: item.Path})
	d.Observe(runner.Event{Phase: runner.PhaseQueued, Count: 2})
	d.Observe(runner.Event{Phase: runner.PhaseStart, Item: &item})
	d.Observe(runner.Event{Phase: runner.PhaseStart, Item: &other})
	d.Observe(runner.Event{Phase: runner.PhaseProgress, Item: &item,
		Progress: encode.Progress{Fraction: 0.42, Speed: 1.8}})

	a := d.Activity()
	if a.Phase != PhaseWorking {
		t.Errorf("phase = %q, want %q", a.Phase, PhaseWorking)
	}
	if a.Considered != 1200 || a.Probed != 1 || a.Queued != 2 {
		t.Errorf("considered/probed/queued = %d/%d/%d, want 1200/1/2", a.Considered, a.Probed, a.Queued)
	}
	if len(a.Running) != 2 {
		t.Fatalf("running = %d, want 2", len(a.Running))
	}
	if a.Running[0].Path != item.Path || a.Running[0].Progress.Fraction != 0.42 {
		t.Errorf("progress landed on the wrong job: %+v", a.Running[0])
	}
	if a.Running[0].Kind != queue.KindEncode {
		t.Errorf("kind = %q, want %q", a.Running[0].Kind, queue.KindEncode)
	}

	d.Observe(runner.Event{Phase: runner.PhaseDone, Item: &item})
	d.Observe(runner.Event{Phase: runner.PhaseDone, Item: &other, Err: errors.New("no")})

	a = d.Activity()
	if len(a.Running) != 0 {
		t.Errorf("running = %d after both finished, want 0", len(a.Running))
	}
	if a.Done != 1 || a.Failed != 1 {
		t.Errorf("done/failed = %d/%d, want 1/1", a.Done, a.Failed)
	}
}

// Activity is read from the web server's goroutine while the pass writes to
// it from several of its own. The copy it returns must be one nothing else
// still holds a reference to.
func TestActivityIsSafeToReadWhileAPassWrites(t *testing.T) {
	d := newDaemon(t)
	d.beginPass()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			it := queue.Item{Index: i, Path: "/lib/f.mkv", Kind: queue.KindRemux}
			d.Observe(runner.Event{Phase: runner.PhaseStart, Item: &it})
			d.Observe(runner.Event{Phase: runner.PhaseProgress, Item: &it,
				Progress: encode.Progress{Fraction: 0.5}})
			d.Observe(runner.Event{Phase: runner.PhaseDone, Item: &it})
		}
	}()

	for i := 0; i < 500; i++ {
		a := d.Activity()
		for _, r := range a.Running {
			_ = r.Progress.Fraction
		}
	}
	close(stop)
	wg.Wait()
}

func TestHistoryKeepsTheNewestPassFirst(t *testing.T) {
	d := newDaemon(t)
	d.HistoryLimit = 2

	for _, label := range []string{"one", "two", "three"} {
		d.recordPass(runner.Target{Label: label, Library: label}, &runner.Summary{
			Outcomes: []runner.Outcome{
				{Plan: decide.Plan{Action: decide.ActionEncode},
					Result: &encode.Result{Replaced: true, Verification: encode.Verification{
						SourceSize: 10 << 30, OutputSize: 6 << 30}}},
				{Declined: "a symlink"},
				{Waiting: "still settling"},
				{Err: errors.New("unreadable")},
			},
			Elapsed: time.Minute,
		}, nil)
	}

	recent := d.Activity().Recent
	if len(recent) != 2 {
		t.Fatalf("kept %d passes, want the 2 most recent", len(recent))
	}
	if recent[0].Library != "three" || recent[1].Library != "two" {
		t.Errorf("history = %s, %s; want three, two", recent[0].Library, recent[1].Library)
	}
	got := recent[0]
	if got.Encoded != 1 || got.Declined != 1 || got.Waiting != 1 || got.Failed != 1 {
		t.Errorf("counts = encoded %d, declined %d, waiting %d, failed %d; want one of each",
			got.Encoded, got.Declined, got.Waiting, got.Failed)
	}
	if got.Saved != 4<<30 {
		t.Errorf("saved = %d, want %d", got.Saved, int64(4)<<30)
	}
}

func TestAPassRecordsWhyItFailed(t *testing.T) {
	d := newDaemon(t)
	d.recordPass(runner.Target{Label: "movies"}, nil, errors.New("mount is gone"))

	recent := d.Activity().Recent
	if len(recent) != 1 || recent[0].Err != "mount is gone" {
		t.Fatalf("history = %+v, want the error recorded", recent)
	}
}

func waitFor(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
