package runner

import (
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/queue"
)

// Phase says what an Event is about.
type Phase string

const (
	// PhaseWalked reports how many candidate files the walk found.
	PhaseWalked Phase = "walked"
	// PhaseProbe is one file about to be probed.
	PhaseProbe Phase = "probe"
	// PhaseQueued reports how many jobs are about to run.
	PhaseQueued Phase = "queued"
	// PhaseStart is one job beginning.
	PhaseStart Phase = "start"
	// PhaseProgress is ffmpeg reporting on a running job.
	PhaseProgress Phase = "progress"
	// PhaseDone is one job finished, successfully or not.
	PhaseDone Phase = "done"
	// PhaseWarning is something that went wrong but did not stop the pass --
	// almost always the database, which is not worth failing a run over.
	PhaseWarning Phase = "warning"
)

// Event is progress reporting for a pass. It exists so that the terminal and
// the Phase 5 web UI can both watch the same run without runner knowing
// anything about either.
type Event struct {
	Phase Phase
	Path  string
	Count int

	Item     *queue.Item
	Progress encode.Progress
	Result   *encode.Result
	Err      error
}
