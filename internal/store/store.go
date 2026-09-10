// Package store is this program's memory: what it has seen on disk, what it
// decided about each file, and what it actually did.
//
// The split from config is deliberate and absolute. Configuration lives in
// YAML, where a person writes it and can read it back six months later. State
// lives here, in SQLite, where nobody has to read it by hand. Reading the old
// Tdarr configuration required cracking open a SQLite blob, and that is the
// anti-pattern this project exists to avoid -- so nothing in this package ever
// decides anything about quality, and nothing in config ever persists.
//
// Three jobs:
//
//   - Sightings. A file is only eligible for work once it has stopped moving,
//     which on this server means more than "it is an hour old". See Settled.
//   - The decision cache, keyed on (path, size, mtime, profile). A library of
//     20,000 files is mostly files that are already exactly as the profile
//     wants them, and re-probing all of them every three hours is the bulk of
//     the work a scan would otherwise do.
//   - Job history, so that a failure is still visible months later. This runs
//     unattended and gets looked at about twice a year.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mjnitz02/media-compressor/internal/decide"

	// The pure-Go SQLite driver, so the binary still builds with CGO_ENABLED=0
	// and the container's second stage stays a single copied file.
	_ "modernc.org/sqlite"
)

// Store is the database handle. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (and if necessary creates) the database at path. ":memory:" is
// accepted for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One connection. The writes here are tiny and rare -- a scan is one
	// transaction, a job is two rows -- and a single connection makes
	// "database is locked" structurally impossible rather than something to
	// handle. If this ever becomes the bottleneck, the encodes will have got
	// a great deal faster first.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the web UI's read-only queries in Phase 5.
func (s *Store) DB() *sql.DB { return s.db }

// Sighting is one file as the scanner just found it on disk.
type Sighting struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// FileState is everything the store knows about one path.
type FileState struct {
	Path    string
	Size    int64
	ModTime time.Time

	FirstSeen time.Time
	LastSeen  time.Time

	// UnchangedSince is when this exact (size, mtime) was first observed, and
	// Sightings how many separate observations have found it that way. Both
	// reset the moment either number moves. Together they are the "it has
	// stopped moving" half of eligibility; see Settled.
	UnchangedSince time.Time
	Sightings      int

	// Decision is the cached decision for this exact (size, mtime), or the
	// zero value when there is none. It is discarded automatically when the
	// file changes.
	Decision Decision

	// Failures counts consecutive failed attempts at this exact (size,
	// mtime). RetryAfter is when the next attempt is due; a zero RetryAfter
	// with a non-zero Failures means the file has been given up on until
	// somebody asks for it explicitly or the file itself changes.
	Failures   int
	LastError  string
	FailedAt   time.Time
	RetryAfter time.Time

	// New is true when this sighting was the first the store had ever seen of
	// the path. It is not persisted; it is the answer to "did I just create
	// this row", which the report wants.
	New bool
}

// Decision is a cached decide.Plan, reduced to the parts worth keeping.
//
// The full plan is not stored, and that is on purpose: the ffmpeg arguments
// are derived from the probe, and a job must always be built from a fresh
// probe of the file it is about to replace. What is cached is only enough to
// answer "does this file need anything doing to it at all?", which is the
// question asked 20,000 times per scan and answered "no" almost every time.
type Decision struct {
	Profile     string
	Fingerprint string
	DecidedAt   time.Time
	Action      decide.Action
	Video       decide.VideoDecision
	Reason      string
	SourceKbps  int
	TargetKbps  int
	Notes       []string
}

// Plan rebuilds as much of a decide.Plan as the cache holds, for reporting a
// file that was not re-probed. It is never handed to the encoder.
func (d Decision) Plan() decide.Plan {
	return decide.Plan{
		Profile: d.Profile,
		Action:  d.Action,
		Video:   d.Video,
		Reason:  d.Reason,
		Notes:   d.Notes,
		Bitrate: decide.Bitrate{SourceKbps: d.SourceKbps, TargetKbps: d.TargetKbps},
	}
}

// ProfileFingerprint is a short hash of everything in a profile that could
// change a decision.
//
// It is the other half of the cache key. Without it, editing the bitrate
// floor in config.yaml would leave 20,000 cached "nothing to do" answers in
// place and the change would appear to do nothing at all -- the worst kind of
// bug, because it looks like the tool working.
func ProfileFingerprint(p decide.Profile) string {
	// decide.Profile is plain data with no unexported fields, so its JSON
	// encoding covers every input to a decision, and Go writes struct fields
	// in declaration order, so the encoding is stable.
	b, err := json.Marshal(p)
	if err != nil {
		// Unreachable for a struct of plain data, but a wrong fingerprint
		// must never silently become a cache hit.
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// CachedDecision returns the stored decision if it was made against this
// profile, and reports whether it is usable.
func (f FileState) CachedDecision(fingerprint string) (Decision, bool) {
	if f.Decision.DecidedAt.IsZero() || f.Decision.Fingerprint != fingerprint {
		return Decision{}, false
	}
	return f.Decision, true
}

// Settled reports whether the file has stopped moving, and if not, why not.
//
// Age alone is not enough here. Files arrive on this server by themselves and
// then keep moving: a share-to-share move or an rsync preserves the mtime, so
// a file can be hours old by its own timestamp while its bytes are still being
// written. The only reliable evidence that a file is finished is that a later
// look found it exactly as an earlier one did.
//
// So a file is settled when all three hold:
//
//   - its mtime is at least minAge old -- nothing has written to it recently;
//   - at least two separate scans have seen it;
//   - and its size and mtime have been unchanged for at least minAge.
//
// The consequence, worth knowing before it surprises you: on a brand new
// database nothing is eligible until a second scan has run. That is the rule
// working, not a bug, and `plan` shows you the whole library regardless.
func (f FileState) Settled(minAge time.Duration, now time.Time) (bool, string) {
	if f.Sightings == 0 {
		return false, "never scanned before; eligible once a later scan finds it unchanged"
	}
	if age := now.Sub(f.ModTime); age < minAge {
		return false, fmt.Sprintf("written %s ago, waiting %s for it to settle",
			short(age), short(minAge))
	}
	if f.Sightings < 2 {
		return false, "seen once; eligible when a later scan finds it unchanged"
	}
	if held := now.Sub(f.UnchangedSince); held < minAge {
		return false, fmt.Sprintf("unchanged for only %s of the %s needed", short(held), short(minAge))
	}
	return true, ""
}

// Blocked reports whether a previous failure on this exact file is still
// holding it back.
//
// This exists because of a specific way an unattended optimiser goes wrong: a
// file that fails for a reason inherent to the file -- an encode that comes
// out bigger than the source, say -- fails identically on every scan, forever,
// filling the log with the same error a few thousand times. Backing off, and
// eventually giving up until either the file changes or somebody asks, is the
// difference between a report worth reading and one nobody opens twice.
func (f FileState) Blocked(now time.Time) (bool, string) {
	if f.Failures == 0 {
		return false, ""
	}
	first := strings.SplitN(strings.TrimSpace(f.LastError), "\n", 2)[0]
	if f.RetryAfter.IsZero() {
		return true, fmt.Sprintf("failed %d times, most recently %s ago, and will not be retried "+
			"on its own; run with -retry-failed once the cause is understood: %s",
			f.Failures, short(now.Sub(f.FailedAt)), first)
	}
	if now.Before(f.RetryAfter) {
		return true, fmt.Sprintf("failed %s ago (attempt %d), next try in %s: %s",
			short(now.Sub(f.FailedAt)), f.Failures, short(f.RetryAfter.Sub(now)), first)
	}
	return false, ""
}

// RetryDelay is how long to wait before trying a file again, given how many
// times in a row it has now failed. Zero means "not on its own".
//
// The shape matters more than the numbers: retry soon enough that a transient
// cause (a full work dir, a busy GPU) clears by itself, then give up rather
// than repeat a permanent failure indefinitely.
func RetryDelay(failures int) time.Duration {
	switch {
	case failures <= 1:
		return time.Hour
	case failures == 2:
		return 6 * time.Hour
	case failures == 3:
		return 24 * time.Hour
	}
	return 0
}

func short(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	case d < 48*time.Hour:
		return d.Round(time.Hour).String()
	}
	return fmt.Sprintf("%.0f days", d.Hours()/24)
}
