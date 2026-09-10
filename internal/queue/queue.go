// Package queue runs prepared jobs on two independent worker pools.
//
// The split is the whole point. A remux copies streams and is bound by disk
// and by how fast the array can read; several can run at once and the machine
// barely notices. A hardware HEVC encode is bound by one iGPU, and running
// three of them at once does not make three files finish sooner -- it makes
// all three finish later while every one of them looks stalled.
//
// Conflating the two is part of why the scheduling in the stack this replaces
// felt arbitrary: a queue of a hundred cheap remuxes would sit behind one
// four-hour encode, or four encodes would start at once and none would
// progress. Two pools, sized separately, is a dozen lines and removes the
// whole class of problem.
//
// The package does no I/O of its own. It is handed a function to call and it
// decides only what runs when, which is what makes it testable in
// milliseconds without ffmpeg anywhere near it.
package queue

import (
	"context"
	"sync"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
)

// Kind is what a job contends for, which is the only thing scheduling cares
// about.
type Kind string

const (
	// KindRemux copies streams. I/O bound, cheap, and usually seconds.
	KindRemux Kind = "remux"
	// KindEncode re-encodes the video. GPU bound, and usually hours.
	KindEncode Kind = "encode"
)

// KindFor maps a plan's action onto the pool that should run it.
func KindFor(a decide.Action) Kind {
	if a == decide.ActionEncode {
		return KindEncode
	}
	return KindRemux
}

// Workers is how many of each kind may run at once.
type Workers struct {
	Remux  int
	Encode int
}

func (w Workers) at(k Kind) int {
	n := w.Remux
	if k == KindEncode {
		n = w.Encode
	}
	if n < 1 {
		return 1
	}
	return n
}

// Item is one file's work, already prepared.
type Item struct {
	// Index is the caller's own handle on this item -- typically its position
	// in the list of files being reported on. The queue never interprets it;
	// it exists so a caller can write results back where they belong without
	// the queue needing to know what a result is. It must be unique within a
	// queue, since it is also how SetProgress addresses a running item.
	Index int

	Path    string
	Library string
	Profile string
	Kind    Kind

	// Job is what encode.Run will be given. The queue only carries it.
	Job *encode.Job
}

// Result is one finished item.
type Result struct {
	Item    Item
	Err     error
	Started time.Time
	Elapsed time.Duration

	// Skipped is set when the run was cancelled before this item started.
	Skipped bool
}

// Running is one item in flight, for the status view.
type Running struct {
	Item     Item
	Started  time.Time
	Progress encode.Progress
}

// Snapshot is the queue as it stands right now.
type Snapshot struct {
	Pending map[Kind]int
	Running []Running
	Done    int
	Failed  int
}

// Queue holds work and runs it. Safe for concurrent use; Snapshot is
// specifically meant to be called from another goroutine, which is how the
// Phase 5 status page will read it.
type Queue struct {
	workers Workers

	mu      sync.Mutex
	items   []Item
	started map[int]bool
	running map[int]*Running
	done    int
	failed  int
}

// New returns an empty queue.
func New(w Workers) *Queue {
	return &Queue{
		workers: w,
		started: map[int]bool{},
		running: map[int]*Running{},
	}
}

// Add queues items. Order is preserved within a kind: the queue is
// deliberately first-come-first-served, because the alternative is a
// scheduling policy nobody asked for and a library that never reaches the end
// of the alphabet.
func (q *Queue) Add(items ...Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, items...)
}

// Len is how many items are queued in total.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Count is how many items of one kind are queued.
func (q *Queue) Count(k Kind) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, it := range q.items {
		if it.Kind == k {
			n++
		}
	}
	return n
}

// Run drains the queue, calling do once per item, and returns one Result per
// item in the order they were added.
//
// A cancelled context stops new items being started; whatever is already
// running is left to finish, because the encoder's own cancellation handling
// is what cleans up a part-written temp file, and killing that from out here
// would leave one behind.
func (q *Queue) Run(ctx context.Context, do func(context.Context, Item) error) []Result {
	q.mu.Lock()
	items := make([]Item, len(q.items))
	copy(items, q.items)
	q.mu.Unlock()

	results := make([]Result, len(items))
	byKind := map[Kind][]int{}
	for i, it := range items {
		results[i] = Result{Item: it}
		byKind[it.Kind] = append(byKind[it.Kind], i)
	}

	var wg sync.WaitGroup
	for kind, positions := range byKind {
		feed := make(chan int)

		// The feeder is part of the WaitGroup so that a cancelled run still
		// accounts for every item: work that never started is reported as
		// skipped rather than silently missing from the results.
		wg.Add(1)
		go func(positions []int) {
			defer wg.Done()
			defer close(feed)
			for i, pos := range positions {
				select {
				case feed <- pos:
				case <-ctx.Done():
					for _, rest := range positions[i:] {
						results[rest].Skipped = true
					}
					return
				}
			}
		}(positions)

		for w := 0; w < q.workers.at(kind); w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for pos := range feed {
					if ctx.Err() != nil {
						results[pos].Skipped = true
						continue
					}
					q.begin(items[pos])
					start := time.Now()
					err := do(ctx, items[pos])
					results[pos].Err = err
					results[pos].Started = start
					results[pos].Elapsed = time.Since(start)
					q.finish(items[pos], err)
				}
			}()
		}
	}
	wg.Wait()

	// Every position written above belongs to exactly one goroutine, so the
	// slice needs no lock -- but the counters do, and this reads them once
	// everything has stopped.
	return results
}

func (q *Queue) begin(it Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.started[it.Index] = true
	q.running[it.Index] = &Running{Item: it, Started: time.Now()}
}

func (q *Queue) finish(it Item, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, it.Index)
	if err != nil {
		q.failed++
		return
	}
	q.done++
}

// SetProgress records how far along a running item is. The encoder reports
// several times a second, so this is deliberately just a field assignment.
func (q *Queue) SetProgress(index int, p encode.Progress) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r, ok := q.running[index]; ok {
		r.Progress = p
	}
}

// Snapshot is what is queued, what is running and how far along it is. It is
// the query the status page asks every couple of seconds.
func (q *Queue) Snapshot() Snapshot {
	q.mu.Lock()
	defer q.mu.Unlock()

	s := Snapshot{Pending: map[Kind]int{}, Done: q.done, Failed: q.failed}
	for _, it := range q.items {
		if !q.started[it.Index] {
			s.Pending[it.Kind]++
		}
	}
	for _, r := range q.running {
		s.Running = append(s.Running, *r)
	}
	return s
}
