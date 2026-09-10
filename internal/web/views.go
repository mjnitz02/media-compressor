package web

import (
	"net/http"
	"time"

	"github.com/mjnitz02/media-compressor/internal/daemon"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/store"
)

// listLimit is how many rows a listing page shows. The library is 20,000
// files; a page of all of them would be useless as well as slow, and the
// question these pages answer ("is it doing the right thing?") is answered by
// the first screenful plus the count.
const listLimit = 250

// page is what every template can rely on being there: the counts in the nav,
// the clock, and whatever the loop is doing.
type page struct {
	Title   string
	Nav     string
	Version string
	Now     time.Time

	// Activity is nil when this process is not running the loop -- `serve`
	// pointed at a database, or a daemon that has not started yet. The
	// templates say so rather than showing an idle daemon that is not there.
	Activity  *daemon.Activity
	HasEngine bool

	Counts  store.Counts
	Pending int
	Notes   int

	// Message is a one-off line above the page, for the result of a button.
	Message string
}

// base gathers what the layout needs. Every page pays for it, which is a
// handful of COUNT queries against a database this size.
func (s *Server) base(r *http.Request, nav, title string) (page, error) {
	ctx := r.Context()
	p := page{Title: title, Nav: nav, Version: s.opts.Version, Now: s.now()}

	counts, err := s.opts.Store.Counts(ctx)
	if err != nil {
		return p, err
	}
	p.Counts = counts

	if p.Pending, err = s.opts.Store.PendingCount(ctx); err != nil {
		return p, err
	}
	noteFiles, noteJobs, err := s.opts.Store.NoteCounts(ctx)
	if err != nil {
		return p, err
	}
	p.Notes = noteFiles + noteJobs

	if s.opts.Engine != nil {
		a := s.opts.Engine.Activity()
		p.Activity, p.HasEngine = &a, true
	}
	return p, nil
}

// library is one configured library, for the overview. It is read from the
// config rather than the database because it is configuration: what the
// operator asked for, next to what has happened.
type library struct {
	Name      string
	Profile   string
	Container string
	Paths     []string
}

func (s *Server) libraries() []library {
	if s.opts.Config == nil {
		return nil
	}
	out := make([]library, 0, len(s.opts.Config.Libraries))
	for _, l := range s.opts.Config.Libraries {
		out = append(out, library{
			Name:      l.Name,
			Profile:   l.Profile.Name,
			Container: string(l.Profile.Container),
			Paths:     l.Paths,
		})
	}
	return out
}

// minAge is the settle period, which the queue page needs in order to say why
// a file is not eligible yet.
func (s *Server) minAge() time.Duration {
	if s.opts.Config == nil {
		return 0
	}
	return time.Duration(s.opts.Config.Scanner.MinAgeSeconds) * time.Second
}

type overviewPage struct {
	page
	Groups    []store.DecisionGroup
	Jobs      []store.JobRecord
	Libraries []library
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "overview", "Overview")
	if err != nil {
		return err
	}
	groups, err := s.opts.Store.DecisionGroups(r.Context())
	if err != nil {
		return err
	}
	jobs, err := s.opts.Store.RecentJobs(r.Context(), 8)
	if err != nil {
		return err
	}
	return s.render(w, "overview", overviewPage{
		page: base, Groups: groups, Jobs: jobs, Libraries: s.libraries(),
	})
}

// fileRow is a file plus the two questions the database alone cannot answer:
// is it eligible yet, and is something holding it back. Both need the clock
// and the configured settle period, so they are worked out here rather than
// in the query.
type fileRow struct {
	store.FileState
	Waiting string
}

func (s *Server) rows(files []store.FileState) []fileRow {
	now, minAge := s.now(), s.minAge()
	out := make([]fileRow, 0, len(files))
	for _, f := range files {
		row := fileRow{FileState: f}
		if blocked, why := f.Blocked(now); blocked {
			row.Waiting = why
		} else if settled, why := f.Settled(minAge, now); !settled {
			row.Waiting = why
		}
		out = append(out, row)
	}
	return out
}

type queuePage struct {
	page
	Files     []fileRow
	Total     int
	Truncated bool
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "queue", "Outstanding work")
	if err != nil {
		return err
	}
	files, err := s.opts.Store.PendingFiles(r.Context(), listLimit)
	if err != nil {
		return err
	}
	return s.render(w, "queue", queuePage{
		page: base, Files: s.rows(files), Total: base.Pending,
		Truncated: base.Pending > len(files),
	})
}

type skippedPage struct {
	page
	Groups    []store.DecisionGroup
	Files     []fileRow
	Action    string
	Video     string
	Selected  string
	Truncated bool
}

// skipped is the screen this whole package is for. The table at the top is
// the shape of the library in six rows; the list under it is the files in one
// of those buckets, each with the reason it was given.
func (s *Server) skipped(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "skipped", "Decisions")
	if err != nil {
		return err
	}
	groups, err := s.opts.Store.DecisionGroups(r.Context())
	if err != nil {
		return err
	}

	action := decide.Action(r.URL.Query().Get("action"))
	video := decide.VideoDecision(r.URL.Query().Get("video"))
	chosen := action != "" || video != ""
	if !chosen {
		// The default view is the one worth auditing: encodes this tool
		// declined because the target bitrate came out under the floor. The
		// stack this replaces declined 7,880 files that way.
		video = decide.VideoSkippedFloor
	}

	files, err := s.opts.Store.FilesByDecision(r.Context(), action, video, listLimit)
	if err != nil {
		return err
	}
	if len(files) == 0 && !chosen {
		// Nothing has been declined on the floor, which is a fine answer but
		// a poor first screen. Show everything instead of an empty table
		// nobody asked for.
		action, video = "", ""
		if files, err = s.opts.Store.FilesByDecision(r.Context(), "", "", listLimit); err != nil {
			return err
		}
	}
	return s.render(w, "skipped", skippedPage{
		page: base, Groups: groups, Files: s.rows(files),
		Action: string(action), Video: string(video),
		Selected: describeBucket(action, video), Truncated: len(files) == listLimit,
	})
}

type notesPage struct {
	page
	Files []fileRow
	Jobs  []store.JobRecord
}

// notes is the page to read proactively. A note means a safety rule overrode
// the configuration -- most often that dropping the last audio track was
// refused -- which is the config asking for something this tool would not do.
func (s *Server) notes(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "notes", "Safety notes")
	if err != nil {
		return err
	}
	files, err := s.opts.Store.FilesWithNotes(r.Context(), listLimit)
	if err != nil {
		return err
	}
	jobs, err := s.opts.Store.JobsWithNotes(r.Context(), 50)
	if err != nil {
		return err
	}
	return s.render(w, "notes", notesPage{page: base, Files: s.rows(files), Jobs: jobs})
}

type blockedPage struct {
	page
	Files []fileRow
}

func (s *Server) blocked(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "blocked", "Held back")
	if err != nil {
		return err
	}
	files, err := s.opts.Store.BlockedFiles(r.Context(), listLimit)
	if err != nil {
		return err
	}
	return s.render(w, "blocked", blockedPage{page: base, Files: s.rows(files)})
}

type historyPage struct {
	page
	Jobs []store.JobRecord
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) error {
	base, err := s.base(r, "history", "History")
	if err != nil {
		return err
	}
	jobs, err := s.opts.Store.RecentJobs(r.Context(), 100)
	if err != nil {
		return err
	}
	return s.render(w, "history", historyPage{page: base, Jobs: jobs})
}

// activityData is the polled fragment. It deliberately reads nothing from the
// database: it is fetched every couple of seconds, and the one connection the
// store keeps is also the one the pass is writing its sightings through.
type activityData struct {
	Activity  *daemon.Activity
	HasEngine bool
	Now       time.Time
	Message   string
}

func (s *Server) activity(message string) activityData {
	d := activityData{Now: s.now(), Message: message}
	if s.opts.Engine != nil {
		a := s.opts.Engine.Activity()
		d.Activity, d.HasEngine = &a, true
	}
	return d
}

func (s *Server) activityFragment(w http.ResponseWriter, r *http.Request) error {
	return s.renderFragment(w, "activity", s.activity(""))
}

// startPass is the one button that causes work. It asks the loop to make a
// pass now instead of at the next interval -- the same pass, with the same
// settle rules and the same safety checks. There is no way from here to make
// it do anything it would not have done by itself.
func (s *Server) startPass(w http.ResponseWriter, r *http.Request) error {
	if s.opts.Engine == nil {
		w.WriteHeader(http.StatusConflict)
		return s.renderFragment(w, "activity",
			s.activity("Nothing is running here, so there is no pass to start."))
	}

	msg := "A pass is already running; this asks for another as soon as it finishes."
	if s.opts.Engine.Trigger() {
		msg = "Starting a pass now."
		if a := s.opts.Engine.Activity(); a.Busy() {
			msg = "A pass is already running; another will follow it."
		}
	}
	return s.renderFragment(w, "activity", s.activity(msg))
}

// retry clears the backoff on one file that failed. It writes to the database
// and nothing else: the file is tried again on the next pass, through exactly
// the same checks as any other file.
func (s *Server) retry(w http.ResponseWriter, r *http.Request) error {
	path := r.FormValue("path")
	if path == "" {
		http.Error(w, "no path given", http.StatusBadRequest)
		return nil
	}
	if err := s.opts.Store.ClearFailures(r.Context(), path); err != nil {
		return err
	}
	return s.renderFragment(w, "retried", path)
}

// describeBucket names the selected group in the words the rest of the UI
// uses, so the heading of the list matches the row that was clicked.
func describeBucket(a decide.Action, v decide.VideoDecision) string {
	switch {
	case a != "" && v != "":
		return actionLabel(a) + ", " + videoLabel(v)
	case a != "":
		return actionLabel(a)
	case v != "":
		return videoLabel(v)
	}
	return "every decided file"
}
