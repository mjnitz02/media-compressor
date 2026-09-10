package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// Job statuses. A job is written the moment it starts, so that a crash
// mid-encode leaves evidence rather than a gap.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// JobStart is what is known before the work begins.
type JobStart struct {
	Path       string
	FinalPath  string
	Library    string
	Profile    string
	Action     decide.Action
	SizeBefore int64
	StartedAt  time.Time
}

// JobFinish is what is known afterwards.
type JobFinish struct {
	FinishedAt time.Time
	Elapsed    time.Duration
	SizeAfter  int64
	Err        error
	FFmpegTail string
	Notes      []string
}

// JobRecord is one row of history.
type JobRecord struct {
	ID         int64
	Path       string
	FinalPath  string
	Library    string
	Profile    string
	Action     decide.Action
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	Elapsed    time.Duration
	SizeBefore int64
	SizeAfter  int64
	Error      string
	FFmpegTail string
	Notes      []string
}

// Saved is how many bytes this job reclaimed. Negative is possible in
// principle and is exactly what verification refuses to accept, so seeing one
// here would mean AllowLargerOutput was set deliberately.
func (j JobRecord) Saved() int64 {
	if j.Status != StatusDone || j.SizeAfter == 0 {
		return 0
	}
	return j.SizeBefore - j.SizeAfter
}

// StartJob records a job about to begin and returns its id.
func (s *Store) StartJob(ctx context.Context, j JobStart) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (path, final_path, library, profile, action, status, started_at, size_before)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Path, j.FinalPath, j.Library, j.Profile, string(j.Action), StatusRunning,
		unix(j.StartedAt), j.SizeBefore)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishJob completes the row StartJob wrote.
func (s *Store) FinishJob(ctx context.Context, id int64, f JobFinish) error {
	status, msg := StatusDone, ""
	if f.Err != nil {
		status, msg = StatusFailed, f.Err.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, finished_at = ?, elapsed_ms = ?, size_after = ?,
			error = ?, ffmpeg_tail = ?, notes = ?
		WHERE id = ?`,
		status, unix(f.FinishedAt), f.Elapsed.Milliseconds(), f.SizeAfter,
		msg, f.FFmpegTail, joinNotes(f.Notes), id)
	return err
}

// AbandonRunningJobs closes off jobs left marked running by a process that
// died mid-encode. Called at startup: an encode that was interrupted did not
// replace anything -- that only happens after verification -- so the file is
// intact and the row is just untidy.
func (s *Store) AbandonRunningJobs(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, finished_at = ?, error = ?
		WHERE status = ?`,
		StatusFailed, unix(now), "interrupted: the process stopped while this was running; "+
			"nothing was replaced, so the original is untouched", StatusRunning)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const jobColumns = `id, path, final_path, library, profile, action, status, started_at,
	finished_at, elapsed_ms, size_before, size_after, error, ffmpeg_tail, notes`

func scanJob(sc scanner) (JobRecord, error) {
	var (
		j                     JobRecord
		action, notes         string
		startedAt, finishedAt int64
		elapsedMs             int64
	)
	err := sc.Scan(&j.ID, &j.Path, &j.FinalPath, &j.Library, &j.Profile, &action, &j.Status,
		&startedAt, &finishedAt, &elapsedMs, &j.SizeBefore, &j.SizeAfter, &j.Error,
		&j.FFmpegTail, &notes)
	if err != nil {
		return JobRecord{}, err
	}
	j.Action = decide.Action(action)
	j.StartedAt = fromUnix(startedAt)
	j.FinishedAt = fromUnix(finishedAt)
	j.Elapsed = time.Duration(elapsedMs) * time.Millisecond
	j.Notes = splitNotes(notes)
	return j, nil
}

// RecentJobs returns the newest jobs first.
func (s *Store) RecentJobs(ctx context.Context, limit int) ([]JobRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.queryJobs(ctx,
		"SELECT "+jobColumns+" FROM jobs ORDER BY started_at DESC, id DESC LIMIT ?", limit)
}

// Counts is the summary the status command and the Phase 5 dashboard show.
type Counts struct {
	Files   int
	Decided int

	// ByAction counts cached decisions, so it answers "how much work is
	// outstanding" without touching the disk at all.
	ByAction map[decide.Action]int

	// SkippedFloor is how many files were declined on the bitrate floor. It
	// gets its own number because it is the main quality lever: the old stack
	// declined 7,880 files this way, and watching that number is how you tell
	// whether a change to the floor did what you meant.
	SkippedFloor int

	Blocked int

	JobsDone   int
	JobsFailed int
	BytesSaved int64
}

// Counts summarises the whole database.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	c := Counts{ByAction: map[decide.Action]int{}}

	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&c.Files); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files WHERE decided_at > 0").Scan(&c.Decided); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files WHERE failures > 0").Scan(&c.Blocked); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files WHERE video = ?", string(decide.VideoSkippedFloor)).
		Scan(&c.SkippedFloor); err != nil {
		return c, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT action, COUNT(*) FROM files WHERE decided_at > 0 GROUP BY action")
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return c, err
		}
		c.ByAction[decide.Action(action)] = n
	}
	if err := rows.Err(); err != nil {
		return c, err
	}

	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE status = ?", StatusDone).Scan(&c.JobsDone); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE status = ?", StatusFailed).Scan(&c.JobsFailed); err != nil {
		return c, err
	}

	var saved sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT SUM(size_before - size_after) FROM jobs WHERE status = ? AND size_after > 0",
		StatusDone).Scan(&saved); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	c.BytesSaved = saved.Int64
	return c, nil
}

// BlockedFiles lists files held back by a previous failure, worst first. This
// is the list worth reading after six months away.
func (s *Store) BlockedFiles(ctx context.Context, limit int) ([]FileState, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryFiles(ctx,
		"SELECT "+fileColumns+" FROM files WHERE failures > 0 ORDER BY failures DESC, failed_at DESC LIMIT ?",
		limit)
}

// PendingFiles lists files whose cached decision says there is work to do.
// The queue view in Phase 5 is this query.
func (s *Store) PendingFiles(ctx context.Context, limit int) ([]FileState, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.queryFiles(ctx, "SELECT "+fileColumns+` FROM files
		WHERE decided_at > 0 AND action IN (?, ?) AND failures = 0
		ORDER BY path LIMIT ?`,
		string(decide.ActionRemux), string(decide.ActionEncode), limit)
}
