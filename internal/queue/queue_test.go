package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
)

// items builds n items of one kind, numbered from base.
func items(kind Kind, base, n int) []Item {
	out := make([]Item, n)
	for i := range out {
		out[i] = Item{Index: base + i, Path: string(kind) + string(rune('a'+i)), Kind: kind}
	}
	return out
}

// tracker records the high-water mark of concurrent work per kind, which is
// the property the two pools exist to bound.
type tracker struct {
	mu      sync.Mutex
	now     map[Kind]int
	highest map[Kind]int
}

func newTracker() *tracker {
	return &tracker{now: map[Kind]int{}, highest: map[Kind]int{}}
}

func (t *tracker) enter(k Kind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now[k]++
	if t.now[k] > t.highest[k] {
		t.highest[k] = t.now[k]
	}
}

func (t *tracker) leave(k Kind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now[k]--
}

func (t *tracker) peak(k Kind) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.highest[k]
}

func TestEachPoolRunsAtMostItsOwnWorkerCount(t *testing.T) {
	q := New(Workers{Remux: 3, Encode: 1})
	q.Add(items(KindRemux, 0, 12)...)
	q.Add(items(KindEncode, 100, 6)...)

	seen := newTracker()
	q.Run(context.Background(), func(ctx context.Context, it Item) error {
		seen.enter(it.Kind)
		time.Sleep(2 * time.Millisecond)
		seen.leave(it.Kind)
		return nil
	})

	if got := seen.peak(KindEncode); got != 1 {
		t.Errorf("%d encodes ran at once; the iGPU supports one", got)
	}
	if got := seen.peak(KindRemux); got > 3 {
		t.Errorf("%d remuxes ran at once, want at most 3", got)
	}
	if got := seen.peak(KindRemux); got < 2 {
		t.Errorf("remuxes peaked at %d, so they are not actually running in parallel", got)
	}
}

// This is the behaviour the whole package exists for: in a single queue, a
// hundred second-long remuxes must not sit behind one four-hour encode.
func TestRemuxesDoNotWaitBehindALongEncode(t *testing.T) {
	q := New(Workers{Remux: 2, Encode: 1})
	q.Add(Item{Index: 0, Path: "huge.mkv", Kind: KindEncode})
	q.Add(items(KindRemux, 1, 6)...)

	release := make(chan struct{})
	var remuxesDone atomic.Int32

	go func() {
		// Let every remux finish, then let the encode go.
		for remuxesDone.Load() < 6 {
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()

	done := make(chan []Result)
	go func() {
		done <- q.Run(context.Background(), func(ctx context.Context, it Item) error {
			if it.Kind == KindEncode {
				<-release // would deadlock if the remuxes were stuck behind it
				return nil
			}
			remuxesDone.Add(1)
			return nil
		})
	}()

	select {
	case results := <-done:
		if len(results) != 7 {
			t.Fatalf("got %d results, want 7", len(results))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the remuxes were blocked behind the encode")
	}
}

func TestResultsComeBackInTheOrderTheyWereAdded(t *testing.T) {
	q := New(Workers{Remux: 4, Encode: 2})
	var added []Item
	added = append(added, items(KindRemux, 0, 5)...)
	added = append(added, items(KindEncode, 100, 5)...)
	q.Add(added...)

	results := q.Run(context.Background(), func(ctx context.Context, it Item) error {
		// Deliberately finish out of order.
		time.Sleep(time.Duration(10-it.Index%10) * time.Millisecond)
		return nil
	})

	if len(results) != len(added) {
		t.Fatalf("got %d results, want %d", len(results), len(added))
	}
	for i := range added {
		if results[i].Item.Index != added[i].Index {
			t.Fatalf("result %d is for item %d, want %d -- a report that reorders itself "+
				"between runs is not comparable", i, results[i].Item.Index, added[i].Index)
		}
	}
}

func TestFailuresAreReportedPerItemAndDoNotStopTheRest(t *testing.T) {
	q := New(Workers{Remux: 2, Encode: 1})
	q.Add(items(KindRemux, 0, 6)...)

	boom := errors.New("ffmpeg failed")
	results := q.Run(context.Background(), func(ctx context.Context, it Item) error {
		if it.Index == 2 {
			return boom
		}
		return nil
	})

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			if r.Item.Index != 2 {
				t.Errorf("item %d failed unexpectedly", r.Item.Index)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d items failed, want 1: one bad file must not stop a library", failed)
	}

	snap := q.Snapshot()
	if snap.Done != 5 || snap.Failed != 1 {
		t.Errorf("Snapshot after the run: %d done, %d failed", snap.Done, snap.Failed)
	}
}

func TestCancellingStopsStartingNewWork(t *testing.T) {
	q := New(Workers{Remux: 1, Encode: 1})
	q.Add(items(KindRemux, 0, 20)...)

	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int32
	results := q.Run(ctx, func(ctx context.Context, it Item) error {
		if ran.Add(1) == 3 {
			cancel()
		}
		return nil
	})

	if int(ran.Load()) >= 20 {
		t.Fatal("cancelling did not stop the queue")
	}
	skipped := 0
	for _, r := range results {
		if r.Skipped {
			skipped++
		}
	}
	if skipped == 0 {
		t.Error("want the un-run items marked skipped rather than silently missing")
	}
	if int(ran.Load())+skipped != 20 {
		t.Errorf("%d ran and %d were skipped, want 20 accounted for", ran.Load(), skipped)
	}
}

func TestSnapshotShowsWhatIsRunningAndHowFarAlong(t *testing.T) {
	q := New(Workers{Remux: 1, Encode: 1})
	q.Add(Item{Index: 0, Path: "film.mkv", Kind: KindEncode})
	q.Add(items(KindRemux, 1, 3)...)

	inFlight := make(chan Snapshot, 1)
	release := make(chan struct{})

	// Released only once the snapshot has been taken, so the encode is
	// genuinely in flight when Snapshot runs.
	go func() {
		<-inFlight
		close(release)
	}()

	var snap Snapshot
	q.Run(context.Background(), func(ctx context.Context, it Item) error {
		if it.Kind == KindEncode {
			q.SetProgress(it.Index, encode.Progress{Fraction: 0.5, Speed: 2.5})
			snap = q.Snapshot()
			inFlight <- snap
			<-release
			return nil
		}
		return nil
	})

	found := false
	for _, r := range snap.Running {
		if r.Item.Path == "film.mkv" {
			found = true
			if r.Progress.Fraction != 0.5 || r.Progress.Speed != 2.5 {
				t.Errorf("progress = %+v", r.Progress)
			}
			if r.Started.IsZero() {
				t.Error("a running item has no start time")
			}
		}
	}
	if !found {
		t.Error("the running encode is not in the snapshot")
	}
}

func TestKindForSendsOnlyRealEncodesToTheGPUPool(t *testing.T) {
	if KindFor(decide.ActionEncode) != KindEncode {
		t.Error("an encode must go to the encode pool")
	}
	// A remux is cheap and I/O bound whatever else the plan says about it.
	if KindFor(decide.ActionRemux) != KindRemux {
		t.Error("a remux must go to the remux pool")
	}
}

func TestZeroWorkersStillMakesProgress(t *testing.T) {
	// A config with workers set to zero must not wedge; one at a time is the
	// conservative reading, and this tool must never need a nudge.
	q := New(Workers{})
	q.Add(items(KindRemux, 0, 3)...)

	var ran atomic.Int32
	q.Run(context.Background(), func(ctx context.Context, it Item) error {
		ran.Add(1)
		return nil
	})
	if ran.Load() != 3 {
		t.Errorf("%d of 3 items ran with zero workers configured", ran.Load())
	}
}
