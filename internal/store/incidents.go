package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// incidentRow is one row of the incidents table.
type incidentRow struct {
	key        string
	state      string // open, acked, resolved
	openedAt   time.Time
	resolvedAt time.Time // zero unless resolved
	lastEvent  string
	// The re-alert schedule: set while the incident is open and its alert
	// event asked for a re-alert.
	realert    time.Duration
	nextAlert  time.Time
	alertEvent string
}

// snapshot is the incident server field a stored incident event carries.
func (r *incidentRow) snapshot() map[string]any {
	m := map[string]any{"state": r.state, "opened_at": r.openedAt.UTC().Format(time.RFC3339Nano)}
	if !r.resolvedAt.IsZero() {
		m["resolved_at"] = r.resolvedAt.UTC().Format(time.RFC3339Nano)
	}
	return m
}

func parseTime(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(timeFormat, s.String)
	return t
}

func formatTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeFormat)
}

// RealertOf returns the re-alert interval an event asks for:
// details.heraldo.realert_s, which a rule's set.realert writes. 0 means none.
func RealertOf(details *map[string]any) time.Duration {
	if details == nil {
		return 0
	}
	h, _ := (*details)["heraldo"].(map[string]any)
	var secs float64
	switch v := h["realert_s"].(type) {
	case float64:
		secs = v
	case json.Number:
		secs, _ = v.Float64()
	case int:
		secs = float64(v)
	}
	if secs < 60 {
		// Shorter than a minute is a mistake, not a schedule.
		return 0
	}
	return time.Duration(secs) * time.Second
}

// realertLevel reports whether level asks for a re-alert at all: only
// time-sensitive and critical incidents repeat.
func realertLevel(l *api.Level) bool {
	return l != nil && (*l == api.LevelTimeSensitive || *l == api.LevelCritical)
}

// nextIncident reads the incident row of ev's key and returns it as it
// stands after ev.
//
//	open, update, none  a new or resolved incident opens at ev; an acked one
//	                    stays acked; an open one stays open
//	ack                 an open incident is acked; a resolved one stays resolved
//	resolve             resolves at ev; a resolved one keeps its first resolve
//
// An open, update or untyped event at level time-sensitive or critical with
// a re-alert interval schedules a re-alert of itself while the incident is
// open. ack and resolve clear the schedule.
func nextIncident(ctx context.Context, tx *sql.Tx, ev NewEvent) (*incidentRow, error) {
	in := ev.Typed
	key := *in.Key
	at := ev.ReceivedAt.UTC()
	var prev *incidentRow
	var opened, resolved, next sql.NullString
	var realert sql.NullInt64
	var alertEvent sql.NullString
	r := incidentRow{key: key}
	err := tx.QueryRowContext(ctx, `SELECT state, opened_at, resolved_at, last_event, realert_s, next_alert_at, alert_event
		FROM incidents WHERE key = ?`, key).Scan(&r.state, &opened, &resolved, &r.lastEvent, &realert, &next, &alertEvent)
	switch {
	case err == nil:
		r.openedAt, r.resolvedAt, r.nextAlert = parseTime(opened), parseTime(resolved), parseTime(next)
		r.realert = time.Duration(realert.Int64) * time.Second
		r.alertEvent = alertEvent.String
		p := r
		prev = &p
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	unschedule := func() { r.realert, r.nextAlert, r.alertEvent = 0, time.Time{}, "" }
	action := ""
	if in.Action != nil {
		action = string(*in.Action)
	}
	switch action {
	case "resolve":
		switch {
		case prev == nil:
			r = incidentRow{key: key, state: "resolved", openedAt: at, resolvedAt: at}
		case prev.state == "resolved":
		default:
			r.state, r.resolvedAt = "resolved", at
		}
		unschedule()
	case "ack":
		switch {
		case prev == nil:
			r = incidentRow{key: key, state: "acked", openedAt: at}
		case prev.state == "resolved":
		default:
			r.state = "acked"
		}
		unschedule()
	default:
		switch {
		case prev == nil || prev.state == "resolved":
			r = incidentRow{key: key, state: "open", openedAt: at}
		case prev.state == "acked":
		default:
			r.state = "open"
		}
		if r.state == "open" {
			if every := RealertOf(in.Details); every > 0 && realertLevel(in.Level) {
				r.realert, r.nextAlert, r.alertEvent = every, at.Add(every), ev.ID
			}
		} else {
			unschedule()
		}
	}
	r.lastEvent = ev.ID
	return &r, nil
}

func writeIncident(ctx context.Context, tx *sql.Tx, key string, r *incidentRow) error {
	var realert any
	if r.realert > 0 {
		realert = int64(r.realert / time.Second)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO incidents
		(key, state, opened_at, resolved_at, last_event, realert_s, next_alert_at, alert_event)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET state = excluded.state, opened_at = excluded.opened_at,
		resolved_at = excluded.resolved_at, last_event = excluded.last_event, realert_s = excluded.realert_s,
		next_alert_at = excluded.next_alert_at, alert_event = excluded.alert_event`,
		key, r.state, formatTime(r.openedAt), formatTime(r.resolvedAt), r.lastEvent, realert,
		formatTime(r.nextAlert), nullStr(r.alertEvent))
	return err
}

// DueAlert is an open incident whose re-alert is due.
type DueAlert struct {
	Key string
	// EventID is the event to push again.
	EventID  string
	OpenedAt time.Time
	Due      time.Time
	Every    time.Duration
}

// DueAlerts returns the open incidents whose re-alert is due at now, oldest
// due first.
func (s *Store) DueAlerts(ctx context.Context, now time.Time) ([]DueAlert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, alert_event, opened_at, next_alert_at, realert_s FROM incidents
		WHERE next_alert_at IS NOT NULL AND next_alert_at <= ? AND state = 'open' AND alert_event IS NOT NULL
		ORDER BY next_alert_at`, now.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueAlert
	for rows.Next() {
		var d DueAlert
		var opened, due sql.NullString
		var every int64
		if err := rows.Scan(&d.Key, &d.EventID, &opened, &due, &every); err != nil {
			return nil, err
		}
		d.OpenedAt, d.Due, d.Every = parseTime(opened), parseTime(due), time.Duration(every)*time.Second
		out = append(out, d)
	}
	return out, rows.Err()
}

// Realerted moves the re-alert of the incident key from due to next. It
// changes nothing, and reports false, when the schedule moved meanwhile: an
// ack, a resolve or a new open event came in.
func (s *Store) Realerted(ctx context.Context, key string, due, next time.Time) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE incidents SET next_alert_at = ?
		WHERE key = ? AND next_alert_at = ? AND state = 'open'`,
		next.UTC().Format(timeFormat), key, due.UTC().Format(timeFormat))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OpenIncidentKeys returns the keys of the incidents that are not resolved,
// acked ones included, in key order.
func (s *Store) OpenIncidentKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM incidents WHERE state != 'resolved' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
