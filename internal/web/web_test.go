package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/daemon"
	"github.com/mjnitz02/media-compressor/internal/decide"
	"github.com/mjnitz02/media-compressor/internal/encode"
	"github.com/mjnitz02/media-compressor/internal/queue"
	"github.com/mjnitz02/media-compressor/internal/store"
)

var now = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// fakeEngine stands in for the daemon. The web package only ever asks it two
// questions, which is the point of the interface being this small.
type fakeEngine struct {
	activity  daemon.Activity
	triggered int
	accept    bool
}

func (f *fakeEngine) Activity() daemon.Activity { return f.activity }
func (f *fakeEngine) Trigger() bool {
	f.triggered++
	return f.accept
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// populate writes one of everything the pages have to render: a file already
// as the profile wants it, one whose encode was declined on the floor, one
// with work outstanding, one held back after a failure, one carrying a safety
// note, and both outcomes of a job.
func populate(t *testing.T, db *store.Store) {
	t.Helper()
	ctx := context.Background()
	mtime := now.Add(-72 * time.Hour)

	paths := []string{
		"/mnt/media/movies/Already HEVC.mkv",
		"/mnt/media/movies/Under The Floor.mkv",
		"/mnt/media/movies/Needs An Encode.mkv",
		"/mnt/media/movies/Broken.mkv",
		"/mnt/media/movies/Only One Audio Track.mkv",
	}
	seen := make([]store.Sighting, len(paths))
	for i, p := range paths {
		seen[i] = store.Sighting{Path: p, Size: int64(i+1) << 30, ModTime: mtime}
	}
	// Twice, so the files are settled and the queue page can say "ready".
	if _, err := db.Observe(ctx, seen, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Observe(ctx, seen, now); err != nil {
		t.Fatal(err)
	}

	decisions := []struct {
		path string
		size int64
		d    store.Decision
	}{
		{paths[0], 1 << 30, store.Decision{Action: decide.ActionNone, Video: decide.VideoCopy,
			SourceKbps: 2100}},
		{paths[1], 2 << 30, store.Decision{Action: decide.ActionNone, Video: decide.VideoSkippedFloor,
			Reason: "target 1470 kbps is below floor 3000 kbps", SourceKbps: 2940, TargetKbps: 1470}},
		{paths[2], 3 << 30, store.Decision{Action: decide.ActionEncode, Video: decide.VideoEncode,
			SourceKbps: 8412, TargetKbps: 4206}},
		{paths[4], 5 << 30, store.Decision{Action: decide.ActionRemux, Video: decide.VideoCopy,
			Notes: []string{`kept audio track 0 (aac, language "chi") even though it matches no keep rule: dropping it would leave the file with no audio`}}},
	}
	for _, d := range decisions {
		d.d.Profile, d.d.Fingerprint, d.d.DecidedAt = "standard", "abc123", now
		if err := db.RecordDecision(ctx, d.path, d.size, mtime, d.d); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.RecordFailure(ctx, paths[3], 4<<30, mtime,
		"the re-encode was larger than the source, so it was rejected", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	done, err := db.StartJob(ctx, store.JobStart{Path: "/mnt/media/movies/Finished.mkv",
		Library: "movies", Profile: "standard", Action: decide.ActionEncode,
		SizeBefore: 10 << 30, StartedAt: now.Add(-3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, done, store.JobFinish{FinishedAt: now.Add(-time.Hour),
		Elapsed: 2 * time.Hour, SizeAfter: 6 << 30,
		Notes: []string{"could not change the owner: not running as root"}}); err != nil {
		t.Fatal(err)
	}

	failed, err := db.StartJob(ctx, store.JobStart{Path: "/mnt/media/movies/Went Wrong.mkv",
		Library: "movies", Action: decide.ActionRemux,
		SizeBefore: 2 << 30, StartedAt: now.Add(-20 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, failed, store.JobFinish{FinishedAt: now.Add(-19 * time.Minute),
		Err:        errors.New("ffmpeg exited 1"),
		FFmpegTail: "Invalid data found when processing input"}); err != nil {
		t.Fatal(err)
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Libraries: []config.Library{{
			Name: "movies", ProfileName: "standard",
			Paths:   []string{"/mnt/media/movies"},
			Profile: decide.Profile{Name: "standard", Container: decide.ContainerMKV},
		}},
		Scanner: config.Scanner{MinAgeSeconds: 900, IntervalMinutes: 180},
	}
}

func newServer(t *testing.T, db *store.Store, engine Engine) *Server {
	t.Helper()
	s, err := New(Options{
		Store: db, Config: testConfig(), Engine: engine,
		Version: "test", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", path, rec.Code, rec.Body.String())
	}
	return rec
}

func mustContain(t *testing.T, body, want, why string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("%s: page does not mention %q", why, want)
	}
}

func TestEveryPageRenders(t *testing.T) {
	db := newStore(t)
	populate(t, db)
	s := newServer(t, db, &fakeEngine{accept: true})

	for _, tc := range []struct{ path, want, why string }{
		{"/", "files seen", "the overview leads with what it has looked at"},
		{"/", "Finished.mkv", "recent work is on the overview"},
		{"/queue", "Needs An Encode.mkv", "the outstanding file is listed"},
		{"/queue", "8412 kbps", "the queue shows the bitrate arithmetic"},
		{"/skipped", "below floor 3000 kbps", "the reason is quoted verbatim"},
		{"/notes", "would leave the file with no audio", "the safety note is readable"},
		{"/notes", "not running as root", "notes from work that was carried out are here too"},
		{"/blocked", "larger than the source", "the failure is explained"},
		{"/history", "Finished.mkv", "completed work is in the history"},
		{"/history", "Invalid data found", "the ffmpeg tail is kept for failures"},
	} {
		body := get(t, s, tc.path).Body.String()
		mustContain(t, body, tc.want, tc.path+": "+tc.why)
	}
}

// The overview must not lead with a file the queue page would call ready but
// is actually the recent job list; this pins that the two tables are distinct.
func TestOverviewCountsWhatWasDecided(t *testing.T) {
	db := newStore(t)
	populate(t, db)
	body := get(t, newServer(t, db, nil), "/").Body.String()

	mustContain(t, body, "encodes declined on the floor", "the floor count is the number worth watching")
	mustContain(t, body, "leave alone", "the decision groups are broken out")
	mustContain(t, body, "4.0 GiB", "what it has actually saved is on the front page")
}

// Every page must work against a database nothing has been written to yet,
// because that is what the operator sees first.
func TestPagesSurviveAnEmptyDatabase(t *testing.T) {
	s := newServer(t, newStore(t), nil)
	for _, path := range []string{"/", "/queue", "/skipped", "/notes", "/blocked", "/history"} {
		body := get(t, s, path).Body.String()
		if strings.Contains(body, "<no value>") {
			t.Errorf("%s rendered a missing value", path)
		}
	}
	mustContain(t, get(t, s, "/").Body.String(), "Nothing has been scanned yet",
		"an empty database says so plainly")
}

// The default view of the decisions page is the one worth auditing: encodes
// this tool declined on the bitrate floor.
func TestDecisionsPageDefaultsToTheDeclinedEncodes(t *testing.T) {
	db := newStore(t)
	populate(t, db)
	body := get(t, newServer(t, db, nil), "/skipped").Body.String()

	mustContain(t, body, "Under The Floor.mkv", "the declined encode is listed by default")
	if strings.Contains(body, "Already HEVC.mkv") {
		t.Error("the default view listed a file from another group")
	}
}

// Landing on "no files in this group" tells a new operator nothing, so when
// nothing has been declined on the floor the page shows what there is.
func TestTheDecisionsPageDoesNotDeadEndWhenNothingWasDeclined(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	mtime := now.Add(-72 * time.Hour)
	path := "/mnt/media/movies/Already HEVC.mkv"

	if _, err := db.Observe(ctx, []store.Sighting{{Path: path, Size: 1 << 30, ModTime: mtime}}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDecision(ctx, path, 1<<30, mtime, store.Decision{
		Fingerprint: "x", DecidedAt: now,
		Action: decide.ActionNone, Video: decide.VideoCopy}); err != nil {
		t.Fatal(err)
	}

	mustContain(t, get(t, newServer(t, db, nil), "/skipped").Body.String(),
		"Already HEVC.mkv", "with no declined encodes the page still shows the decisions there are")
}

func TestDecisionsPageFiltersToOneGroup(t *testing.T) {
	db := newStore(t)
	populate(t, db)
	body := get(t, newServer(t, db, nil), "/skipped?action=none&video=copy").Body.String()

	mustContain(t, body, "Already HEVC.mkv", "the selected group is listed")
	if strings.Contains(body, "Under The Floor.mkv") {
		t.Error("a file outside the selected group was listed")
	}
}

func TestRunningWorkIsShownWithItsProgress(t *testing.T) {
	db := newStore(t)
	engine := &fakeEngine{activity: daemon.Activity{
		Phase:       daemon.PhaseWorking,
		Target:      "library movies (profile standard, container mkv)",
		PassStarted: now.Add(-10 * time.Minute),
		Considered:  1204,
		Queued:      8,
		Running: []daemon.Running{{
			Path: "/mnt/media/movies/Blade Runner.mkv", Kind: queue.KindEncode,
			Started:  now.Add(-9 * time.Minute),
			Progress: encode.Progress{Fraction: 0.42, Speed: 1.8},
		}},
	}}

	body := get(t, newServer(t, db, engine), "/partials/activity").Body.String()
	mustContain(t, body, "Blade Runner.mkv", "the running file is named")
	mustContain(t, body, `value="42"`, "progress is shown as a proportion")
	mustContain(t, body, "1.8x", "the encoding speed is shown")
	mustContain(t, body, "1204 files considered", "the pass's own counts are shown")
}

func TestAnIdleDaemonSaysWhenItWillLookAgain(t *testing.T) {
	engine := &fakeEngine{activity: daemon.Activity{
		Phase: daemon.PhaseIdle, NextPass: now.Add(90 * time.Minute)}}
	body := get(t, newServer(t, newStore(t), engine), "/partials/activity").Body.String()

	mustContain(t, body, "idle", "an idle daemon says so")
	mustContain(t, body, "in 1h 30m", "and says when the next pass is due")
}

func post(t *testing.T, s *Server, path, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func TestTheButtonAsksTheDaemonForAPass(t *testing.T) {
	engine := &fakeEngine{accept: true}
	rec := post(t, newServer(t, newStore(t), engine), "/pass", "", "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /pass: %d\n%s", rec.Code, rec.Body.String())
	}
	if engine.triggered != 1 {
		t.Fatalf("the daemon was asked %d times, want once", engine.triggered)
	}
	mustContain(t, rec.Body.String(), "Starting a pass now", "the answer says what happened")
}

func TestTriggeringWhileBusyIsHonestAboutIt(t *testing.T) {
	engine := &fakeEngine{accept: true, activity: daemon.Activity{Phase: daemon.PhaseWorking}}
	rec := post(t, newServer(t, newStore(t), engine), "/pass", "", "")
	mustContain(t, rec.Body.String(), "already running", "it says the pass will follow the current one")
}

func TestWithNothingRunningThereIsNoPassToStart(t *testing.T) {
	rec := post(t, newServer(t, newStore(t), nil), "/pass", "", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("POST /pass with no engine: %d, want 409", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Nothing is running here", "and says so")
}

// There is no login on this UI, as there was none on the stack it replaces.
// That is a decision about a LAN service, not a licence for any page on the
// internet to start an encode in the operator's browser.
func TestACrossOriginPostIsRefused(t *testing.T) {
	engine := &fakeEngine{accept: true}
	rec := post(t, newServer(t, newStore(t), engine), "/pass", "", "http://elsewhere.invalid")

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d, want 403", rec.Code)
	}
	if engine.triggered != 0 {
		t.Error("a cross-origin POST reached the daemon")
	}
}

func TestRetryClearsTheBackoffOnOneFile(t *testing.T) {
	db := newStore(t)
	populate(t, db)
	s := newServer(t, db, nil)

	blocked, err := db.BlockedFiles(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 {
		t.Fatalf("expected one blocked file, got %d", len(blocked))
	}

	rec := post(t, s, "/retry", "path="+blocked[0].Path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /retry: %d\n%s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "will be tried again", "the row says what happened")

	after, err := db.BlockedFiles(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("the file is still held back: %+v", after[0])
	}
}

func TestRetryNeedsAPath(t *testing.T) {
	rec := post(t, newServer(t, newStore(t), nil), "/retry", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /retry with no path: %d, want 400", rec.Code)
	}
}

// `serve` points the UI at a database whose daemon is somewhere else. It must
// say that rather than showing an idle daemon that is not there.
func TestWithoutAnEngineThePageSaysNothingIsRunningHere(t *testing.T) {
	body := get(t, newServer(t, newStore(t), nil), "/").Body.String()
	mustContain(t, body, "not running here", "a read-only view says what it is")
	if strings.Contains(body, "Run a pass now") {
		t.Error("offered a button that cannot do anything")
	}
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	s := newServer(t, newStore(t), nil)
	for _, path := range []string{"/static/app.css", "/static/htmx.min.js"} {
		if body := get(t, s, path).Body.Len(); body == 0 {
			t.Errorf("%s came back empty", path)
		}
	}
}

func TestHealthzAnswersWithoutTouchingTheDatabase(t *testing.T) {
	mustContain(t, get(t, newServer(t, newStore(t), nil), "/healthz").Body.String(), "ok",
		"the container's health check has something to ask")
}
