package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// State is the latest event per key from the derived tables: incidents that
// are not resolved, progress that has not ended, every check-in, and set
// statuses. Each list is in key order.
type State struct {
	Incidents []json.RawMessage
	Progress  []json.RawMessage
	Checkins  []json.RawMessage
	Statuses  []json.RawMessage
}

// State reads the current state. projects, when not empty, keeps the rows
// in these projects, as Filter.Projects does.
func (s *Store) State(ctx context.Context, projects []string) (State, error) {
	var st State
	pw, pargs := projectWhere("t.project", projects)
	if pw != "" {
		pw = " AND " + pw
	}
	for _, q := range []struct {
		dst   *[]json.RawMessage
		table string
		where string
	}{
		{&st.Incidents, "incidents", "t.state != 'resolved'"},
		{&st.Progress, "activities", "t.state != 'ended'"},
		{&st.Checkins, "checkins", "1"},
		{&st.Statuses, "status", "1"},
	} {
		rows, err := s.db.QueryContext(ctx, `SELECT e.event FROM `+q.table+` t
			JOIN events e ON e.id = t.last_event WHERE `+q.where+pw+` ORDER BY t.key`, pargs...)
		if err != nil {
			return State{}, err
		}
		*q.dst = []json.RawMessage{}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return State{}, err
			}
			*q.dst = append(*q.dst, json.RawMessage(raw))
		}
		if err := rows.Close(); err != nil {
			return State{}, err
		}
	}
	if err := s.unflap(ctx, st.Incidents, s.now()); err != nil {
		return State{}, err
	}
	return st, nil
}

// Cursor returns the read cursor called name. found is false when there is
// none.
func (s *Store) Cursor(ctx context.Context, name string) (seq int64, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM cursors WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// StartCursor makes the read cursor called name at the newest event, by by,
// unless it exists. It reports whether it made it. Every event before it
// then counts as read.
func (s *Store) StartCursor(ctx context.Context, name, by string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `INSERT INTO cursors (name, seq, updated_at, updated_by)
		SELECT ?, coalesce(max(seq), 0), ?, ? FROM events WHERE true
		ON CONFLICT (name) DO NOTHING`, name, s.now().UTC().Format(timeFormat), by)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetCursor moves the read cursor called name to seq; by names the token
// that moved it. A cursor only moves forward: a seq at or below the stored
// one changes nothing, so clients that share the cursor cannot move each
// other back. It returns the stored seq.
func (s *Store) SetCursor(ctx context.Context, name string, seq int64, by string) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	at := s.now().UTC().Format(timeFormat)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cursors (name, seq, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET seq = excluded.seq, updated_at = excluded.updated_at,
		updated_by = excluded.updated_by
		WHERE excluded.seq > cursors.seq`, name, seq, at, by); err != nil {
		return 0, err
	}
	cur, _, err := s.Cursor(ctx, name)
	return cur, err
}

// Head returns the seq of the newest event, or 0 when there is none.
func (s *Store) Head(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(seq) FROM events`).Scan(&seq)
	return seq.Int64, err
}
