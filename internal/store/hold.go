package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Hold entry states.
const (
	HoldHeld       = "held"
	HoldReleased   = "released"
	HoldSuppressed = "suppressed"
	HoldExpired    = "expired"
)

// HoldEntry is one alert in the hold (docs/api.md, "Hold").
type HoldEntry struct {
	ID       string
	EventID  string
	TS       time.Time
	Deadline time.Time
	Tag      string
	Title    string
	Body     string
	Client   string
	Source   string
	// Input is the post as a message, the fields a forward sends again.
	Input     map[string]any
	Forwarded bool
	Shadow    bool
	Status    string
	ClosedAt  time.Time
	Verdict   string
	Reason    string
}

// Open reports whether triage has not closed the entry yet.
func (e HoldEntry) Open() bool { return e.Status == HoldHeld }

const holdCols = `id, event_id, ts, deadline, tag, title, body, client, source, input,
	forwarded, shadow, status, closed_at, verdict, reason`

func scanHold(r scanner) (HoldEntry, error) {
	var e HoldEntry
	var eventID, tag, title, body, client, source, closed, verdict, reason sql.NullString
	var ts, deadline, input string
	if err := r.Scan(&e.ID, &eventID, &ts, &deadline, &tag, &title, &body, &client, &source, &input,
		&e.Forwarded, &e.Shadow, &e.Status, &closed, &verdict, &reason); err != nil {
		return HoldEntry{}, err
	}
	e.EventID, e.Tag, e.Title, e.Body = eventID.String, tag.String, title.String, body.String
	e.Client, e.Source, e.Verdict, e.Reason = client.String, source.String, verdict.String, reason.String
	e.TS, e.Deadline, e.ClosedAt = parseTime(ts), parseTime(deadline), parseTime(closed.String)
	if err := json.Unmarshal([]byte(input), &e.Input); err != nil {
		return HoldEntry{}, err
	}
	return e, nil
}

// AddHold stores a new hold entry.
func (s *Store) AddHold(ctx context.Context, e HoldEntry) error {
	input, err := json.Marshal(e.Input)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO hold_entries (`+holdCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, nullStr(e.EventID), fmtTime(e.TS), fmtTime(e.Deadline), nullStr(e.Tag), nullStr(e.Title),
		nullStr(e.Body), nullStr(e.Client), nullStr(e.Source), string(input), e.Forwarded, e.Shadow,
		e.Status, fmtTime(e.ClosedAt), nullStr(e.Verdict), nullStr(e.Reason))
	return err
}

// SetHoldEvent records the event stored for entry id on arrival.
func (s *Store) SetHoldEvent(ctx context.Context, id, eventID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE hold_entries SET event_id = ? WHERE id = ?`, eventID, id)
	return err
}

// GetHold returns one hold entry.
func (s *Store) GetHold(ctx context.Context, id string) (HoldEntry, error) {
	e, err := scanHold(s.db.QueryRowContext(ctx, `SELECT `+holdCols+` FROM hold_entries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return HoldEntry{}, ErrNotFound
	}
	return e, err
}

// Holds returns, oldest first, the open entries, the expired entries not
// yet forwarded, and the entries closed after since.
func (s *Store) Holds(ctx context.Context, since time.Time) ([]HoldEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+holdCols+` FROM hold_entries
		WHERE status = 'held' OR (status = 'expired' AND forwarded = 0) OR closed_at > ?
		ORDER BY ts, id`, since.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HoldEntry
	for rows.Next() {
		e, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetHoldForwarded marks an entry as forwarded: a pushing event carries it.
func (s *Store) SetHoldForwarded(ctx context.Context, id string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE hold_entries SET forwarded = 1 WHERE id = ?`, id)
	return err
}

// SplitHolds sorts ids into the open entries and the rest (unknown or
// closed), keeping their order.
func (s *Store) SplitHolds(ctx context.Context, ids []string) (open, other []string, err error) {
	for _, id := range ids {
		var st string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM hold_entries WHERE id = ?`, id).Scan(&st)
		switch {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && st != HoldHeld):
			other = append(other, id)
		case err != nil:
			return nil, nil, err
		default:
			open = append(open, id)
		}
	}
	return open, other, nil
}

// CloseHolds closes the open entries among ids with status, and returns the
// ones it closed. verdict and reason may be empty.
func (s *Store) CloseHolds(ctx context.Context, ids []string, status, verdict, reason string, at time.Time) ([]string, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var closed []string
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE hold_entries SET status = ?, closed_at = ?,
			verdict = COALESCE(?, verdict), reason = COALESCE(?, reason)
			WHERE id = ? AND status = 'held'`,
			status, fmtTime(at), nullStr(verdict), nullStr(reason), id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			closed = append(closed, id)
		}
	}
	return closed, tx.Commit()
}

// HoldLastPoll returns the time of the last triage poll, or the zero time.
func (s *Store) HoldLastPoll(ctx context.Context) (time.Time, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM hold_meta WHERE name = 'last_poll'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return parseTime(v), err
}

// SetHoldLastPoll records a triage poll.
func (s *Store) SetHoldLastPoll(ctx context.Context, at time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO hold_meta (name, value) VALUES ('last_poll', ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, at.UTC().Format(timeFormat))
	return err
}
