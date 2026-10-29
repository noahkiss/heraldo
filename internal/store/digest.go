package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// DigestRun is one row of the digest send log (migration 0016).
type DigestRun struct {
	// Day is the local day the digest covers, YYYY-MM-DD in digest.tz.
	Day     string
	SentAt  time.Time
	Since   time.Time
	Until   time.Time
	EventID string
}

// DigestSent reports whether the digest of day went out.
func (s *Store) DigestSent(ctx context.Context, day string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM digests WHERE day = ?`, day).Scan(&n)
	return n > 0, err
}

// LastDigest returns the newest digest that went out. found is false when
// none has.
func (s *Store) LastDigest(ctx context.Context) (run DigestRun, found bool, err error) {
	var sent, since, until string
	err = s.db.QueryRowContext(ctx, `SELECT day, sent_at, since_at, until_at, event_id FROM digests
		ORDER BY until_at DESC LIMIT 1`).Scan(&run.Day, &sent, &since, &until, &run.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return DigestRun{}, false, nil
	}
	if err != nil {
		return DigestRun{}, false, err
	}
	run.SentAt, run.Since, run.Until = parseTime(sent), parseTime(since), parseTime(until)
	return run, true, nil
}

// RecordDigest logs that the digest of run.Day went out. A day that already
// has a row keeps it: the first send wins.
func (s *Store) RecordDigest(ctx context.Context, run DigestRun) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO digests (day, sent_at, since_at, until_at, event_id)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (day) DO NOTHING`,
		run.Day, fmtTime(run.SentAt), fmtTime(run.Since), fmtTime(run.Until), run.EventID)
	return err
}

// DigestIncident is one incident as the digest reads it.
type DigestIncident struct {
	Key        string
	State      string // open, acked, resolved
	OpenedAt   time.Time
	ResolvedAt time.Time // zero unless resolved
	Project    string
	// LastAt is when the incident's last event arrived.
	LastAt time.Time
	// Event is the incident's last event.
	Event json.RawMessage
}

// DigestIncidents returns the incidents that are not resolved, and those
// that opened or resolved at since or later, oldest opened first.
func (s *Store) DigestIncidents(ctx context.Context, since time.Time) ([]DigestIncident, error) {
	at := since.UTC().Format(timeFormat)
	rows, err := s.db.QueryContext(ctx, `SELECT t.key, t.state, t.opened_at, t.resolved_at, coalesce(t.project, ''),
		e.received_at, e.event FROM incidents t JOIN events e ON e.id = t.last_event
		WHERE t.state != 'resolved' OR t.opened_at >= ? OR t.resolved_at >= ?
		ORDER BY t.opened_at, t.key`, at, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DigestIncident
	for rows.Next() {
		var d DigestIncident
		var opened, last, raw string
		var resolved sql.NullString
		if err := rows.Scan(&d.Key, &d.State, &opened, &resolved, &d.Project, &last, &raw); err != nil {
			return nil, err
		}
		d.OpenedAt, d.ResolvedAt, d.LastAt = parseTime(opened), parseNullTime(resolved), parseTime(last)
		d.Event = json.RawMessage(raw)
		out = append(out, d)
	}
	return out, rows.Err()
}

// EndedProgress returns the progress end events received in [since, until),
// oldest first.
func (s *Store) EndedProgress(ctx context.Context, since, until time.Time) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event FROM events
		WHERE kind = 'progress' AND action = 'end' AND received_at >= ? AND received_at < ? ORDER BY seq`,
		since.UTC().Format(timeFormat), until.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, rows.Err()
}

// CheckFailures counts, per check key, the failed runs received in
// [since, until): action fail, or a run with an exit code other than 0.
func (s *Store) CheckFailures(ctx context.Context, since, until time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, count(*) FROM events
		WHERE kind = 'checkin' AND key IS NOT NULL AND received_at >= ? AND received_at < ?
		AND (coalesce(action, '') = 'fail'
			OR (coalesce(action, '') != 'log' AND coalesce(json_extract(event, '$.checkin.exit_code'), 0) != 0))
		GROUP BY key`, since.UTC().Format(timeFormat), until.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ProjectCounts counts the events received in [since, until) that are not
// routine (RoutineWhere), per project, most events first. Name is empty for events with no
// project; LastAt is not set. Events whose
// source is skipSource and thread is skipThread are left out, so the digest
// does not count itself.
func (s *Store) ProjectCounts(ctx context.Context, since, until time.Time, skipSource, skipThread string) ([]ProjectCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(project, ''), count(*) FROM events
		WHERE received_at >= ? AND received_at < ? AND NOT `+RoutineWhere+`
		AND NOT (source = ? AND coalesce(json_extract(event, '$.thread'), '') = ?)
		GROUP BY 1 ORDER BY 2 DESC, 1`,
		since.UTC().Format(timeFormat), until.UTC().Format(timeFormat), skipSource, skipThread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectCount
	for rows.Next() {
		var p ProjectCount
		if err := rows.Scan(&p.Name, &p.Events); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
