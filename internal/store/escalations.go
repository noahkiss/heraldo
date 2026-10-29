package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Escalation is one critical event sent to an emergency provider, such as
// Pushover's priority 2, which retries until someone acknowledges it.
type Escalation struct {
	EventID   string
	Key       string
	Provider  string
	Receipt   string
	SentAt    time.Time
	ExpiresAt time.Time
	// CancelledAt is zero while the escalation runs.
	CancelledAt  time.Time
	CancelReason string
}

// AddEscalation records an escalation.
func (s *Store) AddEscalation(ctx context.Context, e Escalation) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO escalations (event_id, key, provider, receipt, sent_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, e.EventID, e.Key, e.Provider, e.Receipt,
		e.SentAt.UTC().Format(timeFormat), e.ExpiresAt.UTC().Format(timeFormat))
	return err
}

// RunningEscalations returns the escalations of key that are not cancelled
// and not past their expiry at now, oldest first. An empty key matches
// nothing: an event without a key cannot be acked.
func (s *Store) RunningEscalations(ctx context.Context, key string, now time.Time) ([]Escalation, error) {
	if key == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, key, provider, receipt, sent_at, expires_at
		FROM escalations WHERE key = ? AND cancelled_at IS NULL AND expires_at > ? ORDER BY sent_at, event_id`,
		key, now.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Escalation
	for rows.Next() {
		var e Escalation
		var sent, exp string
		if err := rows.Scan(&e.EventID, &e.Key, &e.Provider, &e.Receipt, &sent, &exp); err != nil {
			return nil, err
		}
		e.SentAt, _ = time.Parse(timeFormat, sent)
		e.ExpiresAt, _ = time.Parse(timeFormat, exp)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Escalation returns the escalation of an event, or ErrNotFound.
func (s *Store) Escalation(ctx context.Context, eventID string) (Escalation, error) {
	var e Escalation
	var sent, exp string
	var cancelled, reason sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT event_id, key, provider, receipt, sent_at, expires_at, cancelled_at,
		cancel_reason FROM escalations WHERE event_id = ?`, eventID).
		Scan(&e.EventID, &e.Key, &e.Provider, &e.Receipt, &sent, &exp, &cancelled, &reason)
	if err == sql.ErrNoRows {
		return Escalation{}, ErrNotFound
	}
	if err != nil {
		return Escalation{}, err
	}
	e.SentAt, _ = time.Parse(timeFormat, sent)
	e.ExpiresAt, _ = time.Parse(timeFormat, exp)
	if cancelled.Valid {
		e.CancelledAt, _ = time.Parse(timeFormat, cancelled.String)
	}
	e.CancelReason = reason.String
	return e, nil
}

// CancelEscalation marks an event's escalation cancelled at at, because of
// reason. A cancelled one keeps its first reason.
func (s *Store) CancelEscalation(ctx context.Context, eventID string, at time.Time, reason string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE escalations SET cancelled_at = ?, cancel_reason = ?
		WHERE event_id = ? AND cancelled_at IS NULL`, at.UTC().Format(timeFormat), reason, eventID)
	return err
}

// Unescalated returns the critical events received since since that an
// escalation has not seen yet, in order: a rule or a critical token allowed
// their level (details.heraldo.critical), they have no escalation and no
// escalate trace row, they were not imported, and no ack or resolve of their
// key came after them. They are what was still queued when the server last
// stopped. The escalator's start-up sweep decides each one as if it were
// new.
func (s *Store) Unescalated(ctx context.Context, since time.Time) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, id, event FROM events
		WHERE received_at >= ? AND level = 'critical'
		AND json_extract(event, '$.details.heraldo.critical') IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM escalations e WHERE e.event_id = events.id)
		AND NOT EXISTS (SELECT 1 FROM push_decisions d WHERE d.event_id = events.id AND d.stage = ?)
		AND id NOT IN (SELECT event_id FROM imports)
		AND (key IS NULL OR key = '' OR NOT EXISTS (SELECT 1 FROM events later
			WHERE later.key = events.key AND later.seq > events.seq AND later.kind = 'incident'
			AND later.action IN ('ack', 'resolve')))
		ORDER BY seq`, since.UTC().Format(timeFormat), AppendStage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var raw string
		if err := rows.Scan(&r.Seq, &r.ID, &raw); err != nil {
			return nil, err
		}
		r.Event = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}
