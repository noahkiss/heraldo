package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Activity is the current activity on one progress key.
type Activity struct {
	Key    string
	ID     string
	State  string // active or ended
	Source string
	Level  string
	// Content is the merged LiveContent, a JSON object.
	Content   json.RawMessage
	StartedAt time.Time
	// UpdatedAt is when the sender's last event on the key arrived.
	UpdatedAt time.Time
	// Interval is the gap between the last two sender events; 0 when there
	// was only one.
	Interval  time.Duration
	EndedAt   time.Time
	EndReason string
	LastEvent string
}

// Active reports whether the activity has not ended.
func (a Activity) Active() bool { return a.State == "active" }

// End reasons of an activity.
const (
	EndSender    = "sender"    // the sender posted action end
	EndAbandoned = "abandoned" // the sender went silent; the server ended it
	EndRestarted = "restarted" // a start on the same key replaced it
)

// deriveActivity keeps the activities row for a progress event. A start, or
// any event on a key whose activity ended or never existed, begins a new
// activity with the id act_ plus the event id. An update merges its content;
// an end merges it and ends the activity. An end on an ended activity
// changes nothing.
func deriveActivity(ctx context.Context, tx *sql.Tx, ev NewEvent, key, action, at string) error {
	cur, err := scanActivity(tx.QueryRowContext(ctx, `SELECT `+activityCols+` FROM activities WHERE key = ?`, key))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	content := ev.Fields["content"]
	if action == "end" && found && !cur.Active() {
		// A repeated end changes nothing.
		return nil
	}
	if action == "start" || !found || !cur.Active() || cur.ID == "" {
		merged, err := mergeContent("", content)
		if err != nil {
			return err
		}
		state, endedAt, reason := "active", any(nil), any(nil)
		if action == "end" {
			state, endedAt, reason = "ended", at, EndSender
		}
		var level any
		if ev.Typed.Level != nil {
			level = string(*ev.Typed.Level)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO activities
			(key, state, last_event, id, source, content, started_at, updated_at, interval_ms, ended_at, end_reason, level)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET state = excluded.state, last_event = excluded.last_event, id = excluded.id,
			source = excluded.source, content = excluded.content, started_at = excluded.started_at,
			updated_at = excluded.updated_at, interval_ms = NULL, ended_at = excluded.ended_at,
			end_reason = excluded.end_reason, level = excluded.level`,
			key, state, ev.ID, "act_"+ev.ID, ev.Typed.Source, merged, at, at, endedAt, reason, level)
		return err
	}
	merged, err := mergeContent(string(cur.Content), content)
	if err != nil {
		return err
	}
	gap := ev.ReceivedAt.Sub(cur.UpdatedAt).Milliseconds()
	if action == "end" {
		_, err = tx.ExecContext(ctx, `UPDATE activities SET state = 'ended', last_event = ?, content = ?,
			updated_at = ?, interval_ms = ?, ended_at = ?, end_reason = ? WHERE key = ?`,
			ev.ID, merged, at, gap, at, EndSender, key)
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE activities SET last_event = ?, content = ?, updated_at = ?, interval_ms = ?
		WHERE key = ?`, ev.ID, merged, at, gap, key)
	return err
}

const activityCols = `key, coalesce(id, ''), state, coalesce(source, ''), coalesce(level, ''), coalesce(content, '{}'),
	coalesce(started_at, ''), coalesce(updated_at, ''), coalesce(interval_ms, 0), coalesce(ended_at, ''),
	coalesce(end_reason, ''), last_event`

func scanActivity(r scanner) (Activity, error) {
	var a Activity
	var content, started, updated, ended string
	var interval int64
	if err := r.Scan(&a.Key, &a.ID, &a.State, &a.Source, &a.Level, &content, &started, &updated, &interval,
		&ended, &a.EndReason, &a.LastEvent); err != nil {
		return Activity{}, err
	}
	a.Content = json.RawMessage(content)
	a.StartedAt = parseTime(started)
	a.UpdatedAt = parseTime(updated)
	a.EndedAt = parseTime(ended)
	a.Interval = time.Duration(interval) * time.Millisecond
	return a, nil
}

// GetActivity returns the activity on key, or ErrNotFound.
func (s *Store) GetActivity(ctx context.Context, key string) (Activity, error) {
	a, err := scanActivity(s.db.QueryRowContext(ctx, `SELECT `+activityCols+` FROM activities WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Activity{}, ErrNotFound
	}
	return a, err
}

// Activities lists the activities in key order; active keeps those that have
// not ended. Rows from before Live Activities, without an id, are left out.
func (s *Store) Activities(ctx context.Context, active bool) ([]Activity, error) {
	q := `SELECT ` + activityCols + ` FROM activities WHERE id IS NOT NULL`
	if active {
		q += ` AND state = 'active'`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EndActivity ends the activity id on key with reason, when it is still the
// active one. It reports whether it ended it.
func (s *Store) EndActivity(ctx context.Context, key, id, reason string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE activities SET state = 'ended', ended_at = ?, end_reason = ?
		WHERE key = ? AND id = ? AND state = 'active'`, s.now().UTC().Format(timeFormat), reason, key, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		s.changed(DomainActivities, DomainState)
	}
	return n == 1, nil
}

// TimelineEntry is one sender event of an activity.
type TimelineEntry struct {
	EventID    string
	ReceivedAt time.Time
	Action     string
	Event      json.RawMessage
}

// Timeline returns the progress events on key from since on, oldest first,
// at most limit.
func (s *Store) Timeline(ctx context.Context, key string, since time.Time, limit int) ([]TimelineEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, received_at, coalesce(action, ''), event FROM events
		WHERE key = ? AND kind = 'progress' AND received_at >= ? ORDER BY seq LIMIT ?`,
		key, since.UTC().Format(timeFormat), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineEntry
	for rows.Next() {
		var e TimelineEntry
		var at, raw string
		if err := rows.Scan(&e.EventID, &at, &e.Action, &raw); err != nil {
			return nil, err
		}
		e.ReceivedAt = parseTime(at)
		e.Event = json.RawMessage(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- instances ---------------------------------------------------------

// Instance states.
const (
	InstanceStarting     = "starting"      // push-to-start sent, no update token yet
	InstanceActive       = "active"        // the app reported the update token
	InstanceEnded        = "ended"         // ended by the server or the app
	InstanceDismissed    = "dismissed"     // the person removed it
	InstanceNeverStarted = "never-started" // no update token came back in time
	InstanceDead         = "dead"          // APNs refused its token
)

// Instance is one Live Activity on one device: one link of an activity's
// chain.
type Instance struct {
	ID          string
	ActivityID  string
	Key         string
	DeviceID    string
	Chain       int
	State       string
	EndReason   string
	UpdateToken string
	RequestedAt time.Time
	ConfirmedAt time.Time
	EndedAt     time.Time
	LastPushAt  time.Time
}

// Open reports whether the instance may still be on the device.
func (i Instance) Open() bool { return i.State == InstanceStarting || i.State == InstanceActive }

const instanceCols = `id, activity_id, key, device_id, chain, state, coalesce(end_reason, ''),
	coalesce(update_token, ''), requested_at, coalesce(confirmed_at, ''), coalesce(ended_at, ''),
	coalesce(last_push_at, '')`

func scanInstance(r scanner) (Instance, error) {
	var i Instance
	var req, conf, ended, push string
	if err := r.Scan(&i.ID, &i.ActivityID, &i.Key, &i.DeviceID, &i.Chain, &i.State, &i.EndReason,
		&i.UpdateToken, &req, &conf, &ended, &push); err != nil {
		return Instance{}, err
	}
	i.RequestedAt, i.ConfirmedAt, i.EndedAt, i.LastPushAt = parseTime(req), parseTime(conf), parseTime(ended), parseTime(push)
	return i, nil
}

// SaveInstance inserts or replaces an instance.
func (s *Store) SaveInstance(ctx context.Context, i Instance) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO live_instances (`+instanceColsPlain+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET state = excluded.state, end_reason = excluded.end_reason,
		update_token = excluded.update_token, confirmed_at = excluded.confirmed_at, ended_at = excluded.ended_at,
		last_push_at = excluded.last_push_at`,
		i.ID, i.ActivityID, i.Key, i.DeviceID, i.Chain, i.State, nullStr(i.EndReason), nullStr(i.UpdateToken),
		i.RequestedAt.UTC().Format(timeFormat), fmtTime(i.ConfirmedAt), fmtTime(i.EndedAt), fmtTime(i.LastPushAt))
	return s.bumpIf(err, DomainActivities)
}

const instanceColsPlain = `id, activity_id, key, device_id, chain, state, end_reason, update_token, requested_at,
	confirmed_at, ended_at, last_push_at`

// GetInstance returns the instance id, or ErrNotFound.
func (s *Store) GetInstance(ctx context.Context, id string) (Instance, error) {
	i, err := scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM live_instances WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Instance{}, ErrNotFound
	}
	return i, err
}

// Instances lists the instances of the activity, by device and chain.
func (s *Store) Instances(ctx context.Context, activityID string) ([]Instance, error) {
	return s.instances(ctx, `WHERE activity_id = ? ORDER BY device_id, chain`, activityID)
}

// OpenInstances lists every instance that is starting or active.
func (s *Store) OpenInstances(ctx context.Context) ([]Instance, error) {
	return s.instances(ctx, `WHERE state IN ('starting', 'active') ORDER BY activity_id, device_id, chain`)
}

func (s *Store) instances(ctx context.Context, where string, args ...any) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+instanceCols+` FROM live_instances `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// SetLiveActivityToken stores the push-to-start token of device id; empty
// clears it.
func (s *Store) SetLiveActivityToken(ctx context.Context, id, token string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET live_activity_token = ? WHERE id = ?`, nullStr(token), id)
	return err
}
