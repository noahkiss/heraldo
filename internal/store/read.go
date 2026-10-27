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

// State reads the current state.
func (s *Store) State(ctx context.Context) (State, error) {
	var st State
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
			JOIN events e ON e.id = t.last_event WHERE `+q.where+` ORDER BY t.key`)
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
	return st, nil
}

// Cursor returns the read cursor that owner keeps under name. found is false
// when there is none.
func (s *Store) Cursor(ctx context.Context, owner, name string) (seq int64, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM cursors WHERE owner = ? AND name = ?`,
		owner, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return seq, err == nil, err
}

// SetCursor stores owner's read cursor under name. A cursor only moves
// forward: a seq at or below the stored one changes nothing, so two copies
// of one client cannot move each other back. It returns the stored seq.
func (s *Store) SetCursor(ctx context.Context, owner, name string, seq int64) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	at := s.now().UTC().Format(timeFormat)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cursors (owner, name, seq, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (owner, name) DO UPDATE SET seq = excluded.seq, updated_at = excluded.updated_at
		WHERE excluded.seq > cursors.seq`, owner, name, seq, at); err != nil {
		return 0, err
	}
	cur, _, err := s.Cursor(ctx, owner, name)
	return cur, err
}

// Head returns the seq of the newest event, or 0 when there is none.
func (s *Store) Head(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT max(seq) FROM events`).Scan(&seq)
	return seq.Int64, err
}
