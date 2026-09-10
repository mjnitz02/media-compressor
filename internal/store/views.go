package store

import (
	"context"

	"github.com/mjnitz02/media-compressor/internal/decide"
)

// The queries in this file exist for the web UI, and they are all reads. The
// question they answer between them is the one the operator actually has:
// "what has this thing decided about my library, and do I agree with it?"
//
// Nothing here interprets a decision. The reason strings are the ones decide
// produced, verbatim, which is deliberate -- a status code the UI translates
// back into English is a second place for the meaning to drift, and this has
// to make sense to somebody who last looked six months ago.

// DecisionGroup is one bucket of "what did it conclude, and about how many
// files". Action and Video are separate because they answer different
// questions: a file can be left alone as a whole while its video was
// specifically declined on the bitrate floor, and that pairing is the single
// most interesting number in the database.
type DecisionGroup struct {
	Action decide.Action
	Video  decide.VideoDecision
	Count  int
}

// DecisionGroups counts every cached decision by what it concluded, commonest
// first. This is the top of the skipped view: the shape of the whole library
// in six or seven rows.
func (s *Store) DecisionGroups(ctx context.Context) ([]DecisionGroup, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT action, video, COUNT(*) FROM files
		WHERE decided_at > 0
		GROUP BY action, video
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DecisionGroup
	for rows.Next() {
		var g DecisionGroup
		var action, video string
		if err := rows.Scan(&action, &video, &g.Count); err != nil {
			return nil, err
		}
		g.Action, g.Video = decide.Action(action), decide.VideoDecision(video)
		out = append(out, g)
	}
	return out, rows.Err()
}

// FilesByDecision lists the files in one bucket, with the reason each was
// given. An empty action or video means "any", so the caller can ask for one
// axis without inventing a value for the other.
func (s *Store) FilesByDecision(ctx context.Context, action decide.Action, video decide.VideoDecision, limit int) ([]FileState, error) {
	if limit <= 0 {
		limit = 200
	}
	// Built rather than written out because either half may be absent, and
	// two optional filters is four static queries otherwise.
	where := "decided_at > 0"
	args := []any{}
	if action != "" {
		where += " AND action = ?"
		args = append(args, string(action))
	}
	if video != "" {
		where += " AND video = ?"
		args = append(args, string(video))
	}
	args = append(args, limit)

	return s.queryFiles(ctx,
		"SELECT "+fileColumns+" FROM files WHERE "+where+" ORDER BY path LIMIT ?", args...)
}

// FilesWithNotes lists files whose decision recorded a safety note.
//
// A note means a rule overrode the configuration -- most often that dropping
// the last audio track was refused. It is the one thing in here worth reading
// proactively rather than looking up, because it says the config asked for
// something this tool would not do.
func (s *Store) FilesWithNotes(ctx context.Context, limit int) ([]FileState, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.queryFiles(ctx, "SELECT "+fileColumns+` FROM files
		WHERE decided_at > 0 AND notes <> '' ORDER BY path LIMIT ?`, limit)
}

// JobsWithNotes lists finished work that recorded a note, newest first. These
// are the notes from the encode itself -- a chown that could not be done, a
// source with other hardlinks to it -- rather than from the decision.
func (s *Store) JobsWithNotes(ctx context.Context, limit int) ([]JobRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryJobs(ctx, "SELECT "+jobColumns+` FROM jobs
		WHERE notes <> '' ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
}

// NoteCounts is how many files and finished jobs carry a note, so the nav can
// say whether the notes page is worth opening without loading it.
func (s *Store) NoteCounts(ctx context.Context) (files, jobs int, err error) {
	if err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files WHERE notes <> ''").Scan(&files); err != nil {
		return 0, 0, err
	}
	if err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE notes <> ''").Scan(&jobs); err != nil {
		return 0, 0, err
	}
	return files, jobs, nil
}

// PendingCount is how much work is outstanding, without listing it. The
// dashboard wants the number even when the list is 4,000 rows long.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files
		WHERE decided_at > 0 AND action IN (?, ?) AND failures = 0`,
		string(decide.ActionRemux), string(decide.ActionEncode)).Scan(&n)
	return n, err
}

func (s *Store) queryFiles(ctx context.Context, query string, args ...any) ([]FileState, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FileState
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) queryJobs(ctx context.Context, query string, args ...any) ([]JobRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobRecord
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
