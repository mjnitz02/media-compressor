package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// schemaVersion is bumped whenever schema below changes. It is stored in
// SQLite's own PRAGMA user_version, so no table of our own is needed to know
// how old a database is.
const schemaVersion = 1

const schema = `
CREATE TABLE files (
	path            TEXT PRIMARY KEY,
	size            INTEGER NOT NULL,
	mtime_ns        INTEGER NOT NULL,
	first_seen      INTEGER NOT NULL,
	last_seen       INTEGER NOT NULL,
	unchanged_since INTEGER NOT NULL,
	sightings       INTEGER NOT NULL,

	profile         TEXT    NOT NULL DEFAULT '',
	fingerprint     TEXT    NOT NULL DEFAULT '',
	decided_at      INTEGER NOT NULL DEFAULT 0,
	action          TEXT    NOT NULL DEFAULT '',
	video           TEXT    NOT NULL DEFAULT '',
	reason          TEXT    NOT NULL DEFAULT '',
	source_kbps     INTEGER NOT NULL DEFAULT 0,
	target_kbps     INTEGER NOT NULL DEFAULT 0,
	notes           TEXT    NOT NULL DEFAULT '',

	failures        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT    NOT NULL DEFAULT '',
	failed_at       INTEGER NOT NULL DEFAULT 0,
	retry_after     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX files_action    ON files(action);
CREATE INDEX files_last_seen ON files(last_seen);

CREATE TABLE jobs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	path        TEXT    NOT NULL,
	final_path  TEXT    NOT NULL DEFAULT '',
	library     TEXT    NOT NULL DEFAULT '',
	profile     TEXT    NOT NULL DEFAULT '',
	action      TEXT    NOT NULL DEFAULT '',
	status      TEXT    NOT NULL,
	started_at  INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	elapsed_ms  INTEGER NOT NULL DEFAULT 0,
	size_before INTEGER NOT NULL DEFAULT 0,
	size_after  INTEGER NOT NULL DEFAULT 0,
	error       TEXT    NOT NULL DEFAULT '',
	ffmpeg_tail TEXT    NOT NULL DEFAULT '',
	notes       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX jobs_started ON jobs(started_at DESC);
CREATE INDEX jobs_path    ON jobs(path);
`

// migrate brings an empty or older database up to schemaVersion.
//
// There is exactly one version so far, so this is deliberately the simplest
// thing that will still work when there are three: read user_version, apply
// the steps above it in order, write it back.
func (s *Store) migrate() error {
	var have int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	if have == schemaVersion {
		return nil
	}
	if have > schemaVersion {
		return fmt.Errorf("database was written by a newer version (schema %d, this build understands %d)",
			have, schemaVersion)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if have < 1 {
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}
	}
	// PRAGMA does not take a bind parameter.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

const fileColumns = `path, size, mtime_ns, first_seen, last_seen, unchanged_since, sightings,
	profile, fingerprint, decided_at, action, video, reason, source_kbps, target_kbps, notes,
	failures, last_error, failed_at, retry_after`

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface{ Scan(dest ...any) error }

func scanFile(sc scanner) (FileState, error) {
	var (
		f                                   FileState
		mtimeNs                             int64
		firstSeen, lastSeen, unchangedSince int64
		decidedAt, failedAt, retryAfter     int64
		action, video, notes                string
	)
	err := sc.Scan(&f.Path, &f.Size, &mtimeNs, &firstSeen, &lastSeen, &unchangedSince, &f.Sightings,
		&f.Decision.Profile, &f.Decision.Fingerprint, &decidedAt, &action, &video,
		&f.Decision.Reason, &f.Decision.SourceKbps, &f.Decision.TargetKbps, &notes,
		&f.Failures, &f.LastError, &failedAt, &retryAfter)
	if err != nil {
		return FileState{}, err
	}
	f.ModTime = time.Unix(0, mtimeNs)
	f.FirstSeen = fromUnix(firstSeen)
	f.LastSeen = fromUnix(lastSeen)
	f.UnchangedSince = fromUnix(unchangedSince)
	f.Decision.DecidedAt = fromUnix(decidedAt)
	f.Decision.Action = decide.Action(action)
	f.Decision.Video = decide.VideoDecision(video)
	f.Decision.Notes = splitNotes(notes)
	f.FailedAt = fromUnix(failedAt)
	f.RetryAfter = fromUnix(retryAfter)
	return f, nil
}

// Observe records that the scanner has just seen these files, and returns
// what the store now knows about each.
//
// The whole batch is one transaction. A library of 20,000 files is 20,000 of
// these, and a transaction each would make a scan slower than the encoding.
func (s *Store) Observe(ctx context.Context, seen []Sighting, now time.Time) (map[string]FileState, error) {
	states := make(map[string]FileState, len(seen))
	if len(seen) == 0 {
		return states, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sel, err := tx.PrepareContext(ctx, "SELECT "+fileColumns+" FROM files WHERE path = ?")
	if err != nil {
		return nil, err
	}
	defer sel.Close()

	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO files (path, size, mtime_ns, first_seen, last_seen, unchanged_since, sightings)
		VALUES (?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		return nil, err
	}
	defer ins.Close()

	// A file whose size or mtime moved is a different file as far as every
	// cached answer is concerned: the decision, the failure count and the
	// settle clock all reset together. Half-resetting them is how a stale
	// "nothing to do" outlives the file it was about.
	changed, err := tx.PrepareContext(ctx, `
		UPDATE files SET size = ?, mtime_ns = ?, last_seen = ?, unchanged_since = ?, sightings = 1,
			profile = '', fingerprint = '', decided_at = 0, action = '', video = '', reason = '',
			source_kbps = 0, target_kbps = 0, notes = '',
			failures = 0, last_error = '', failed_at = 0, retry_after = 0
		WHERE path = ?`)
	if err != nil {
		return nil, err
	}
	defer changed.Close()

	same, err := tx.PrepareContext(ctx,
		"UPDATE files SET last_seen = ?, sightings = sightings + 1 WHERE path = ?")
	if err != nil {
		return nil, err
	}
	defer same.Close()

	for _, sight := range seen {
		prev, err := scanFile(sel.QueryRowContext(ctx, sight.Path))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := ins.ExecContext(ctx, sight.Path, sight.Size, sight.ModTime.UnixNano(),
				unix(now), unix(now), unix(now)); err != nil {
				return nil, err
			}
			states[sight.Path] = FileState{
				Path: sight.Path, Size: sight.Size, ModTime: sight.ModTime,
				FirstSeen: now, LastSeen: now, UnchangedSince: now, Sightings: 1, New: true,
			}

		case err != nil:
			return nil, err

		case prev.Size != sight.Size || prev.ModTime.UnixNano() != sight.ModTime.UnixNano():
			if _, err := changed.ExecContext(ctx, sight.Size, sight.ModTime.UnixNano(),
				unix(now), unix(now), sight.Path); err != nil {
				return nil, err
			}
			states[sight.Path] = FileState{
				Path: sight.Path, Size: sight.Size, ModTime: sight.ModTime,
				FirstSeen: prev.FirstSeen, LastSeen: now, UnchangedSince: now, Sightings: 1,
			}

		default:
			if _, err := same.ExecContext(ctx, unix(now), sight.Path); err != nil {
				return nil, err
			}
			prev.LastSeen = now
			prev.Sightings++
			states[sight.Path] = prev
		}
	}
	return states, tx.Commit()
}

// Lookup reads the state of these paths without recording a sighting. It is
// what `plan` uses: a dry run reports on the world, it does not change it --
// including the part of the world kept in here.
func (s *Store) Lookup(ctx context.Context, paths []string) (map[string]FileState, error) {
	states := make(map[string]FileState, len(paths))
	if len(paths) == 0 {
		return states, nil
	}

	sel, err := s.db.PrepareContext(ctx, "SELECT "+fileColumns+" FROM files WHERE path = ?")
	if err != nil {
		return nil, err
	}
	defer sel.Close()

	for _, path := range paths {
		f, err := scanFile(sel.QueryRowContext(ctx, path))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		states[path] = f
	}
	return states, nil
}

// RecordDecision caches what decide concluded about a file.
//
// The (size, mtime) in the WHERE clause is the point of the method: it pins
// the answer to the exact bytes it was computed from, so a file that changed
// while it was being probed gets no cached decision at all rather than one
// belonging to its previous contents.
func (s *Store) RecordDecision(ctx context.Context, path string, size int64, mtime time.Time, d Decision) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE files SET profile = ?, fingerprint = ?, decided_at = ?, action = ?, video = ?,
			reason = ?, source_kbps = ?, target_kbps = ?, notes = ?
		WHERE path = ? AND size = ? AND mtime_ns = ?`,
		d.Profile, d.Fingerprint, unix(d.DecidedAt), string(d.Action), string(d.Video),
		d.Reason, d.SourceKbps, d.TargetKbps, joinNotes(d.Notes),
		path, size, mtime.UnixNano())
	return err
}

// RecordFailure notes that work on this file failed, and sets when it may be
// tried again. See FileState.Blocked for why this is not simply retried.
func (s *Store) RecordFailure(ctx context.Context, path string, size int64, mtime time.Time, cause string, now time.Time) error {
	var failures int
	err := s.db.QueryRowContext(ctx, "SELECT failures FROM files WHERE path = ?", path).Scan(&failures)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	failures++

	var retryAfter int64
	if d := RetryDelay(failures); d > 0 {
		retryAfter = unix(now.Add(d))
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE files SET failures = ?, last_error = ?, failed_at = ?, retry_after = ?
		WHERE path = ? AND size = ? AND mtime_ns = ?`,
		failures, cause, unix(now), retryAfter, path, size, mtime.UnixNano())
	return err
}

// ClearFailures forgets the failure history for a path, which is what
// -retry-failed does before trying again.
func (s *Store) ClearFailures(ctx context.Context, paths ...string) error {
	for _, path := range paths {
		if _, err := s.db.ExecContext(ctx,
			"UPDATE files SET failures = 0, last_error = '', failed_at = 0, retry_after = 0 WHERE path = ?",
			path); err != nil {
			return err
		}
	}
	return nil
}

// Forget drops what is known about these paths.
//
// It is called after a file has been successfully replaced: the bytes on disk
// are new, so every cached answer about them is about a file that no longer
// exists. The next scan sees the replacement as a new arrival, probes it once,
// and caches "nothing to do" -- which is both correct and cheap.
func (s *Store) Forget(ctx context.Context, paths ...string) error {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "DELETE FROM files WHERE path = ?", path); err != nil {
			return err
		}
	}
	return nil
}

// Sweep removes rows for files under these roots that this scan did not see,
// i.e. that have been deleted or moved away by somebody else.
//
// It only ever deletes rows in this table. Nothing in this package touches
// the filesystem at all.
func (s *Store) Sweep(ctx context.Context, roots []string, notSeenSince time.Time) (int, error) {
	total := 0
	for _, root := range roots {
		prefix := strings.TrimSuffix(root, "/") + "/"
		res, err := s.db.ExecContext(ctx,
			"DELETE FROM files WHERE last_seen < ? AND (path = ? OR path LIKE ? ESCAPE '\\')",
			unix(notSeenSince), root, escapeLike(prefix)+"%")
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func joinNotes(notes []string) string { return strings.Join(notes, "\n") }

func splitNotes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// escapeLike protects a path used as a LIKE prefix. Media filenames contain
// underscores constantly, and an unescaped one is LIKE's single-character
// wildcard -- which would make Sweep delete rows for a neighbouring library.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
