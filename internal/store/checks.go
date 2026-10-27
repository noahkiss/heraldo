package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
)

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var checkColList = []string{"key", "host", "period_s", "cron", "tz", "grace_s", "max_runtime_s", "reminder_s",
	"state", "state_since", "down_reason", "created_at", "last_ping", "last_event", "started_at", "start_rid",
	"last_success", "last_duration_ms", "last_log_event", "due_at", "reminded_at"}

var checkCols = strings.Join(checkColList, ", ")

func scanCheck(sc interface{ Scan(...any) error }) (checkin.Check, error) {
	var c checkin.Check
	var host, cronExpr, tz, reason, lastEvent, rid, logEvent sql.NullString
	var period, maxRun, dur sql.NullInt64
	var grace, reminder int64
	var since, created string
	var lastPing, started, success, due, reminded sql.NullString
	err := sc.Scan(&c.Key, &host, &period, &cronExpr, &tz, &grace, &maxRun, &reminder, &c.State, &since,
		&reason, &created, &lastPing, &lastEvent, &started, &rid, &success, &dur, &logEvent, &due, &reminded)
	if err != nil {
		return c, err
	}
	c.Host, c.DownReason, c.LastEvent, c.StartRID, c.LastLogEvent = host.String, reason.String, lastEvent.String, rid.String, logEvent.String
	c.Schedule = checkin.Schedule{
		Period: secs(period.Int64), Cron: cronExpr.String, TZ: tz.String,
		Grace: secs(grace), MaxRuntime: secs(maxRun.Int64), Reminder: secs(reminder),
	}
	c.StateSince, c.CreatedAt = parseTime(since), parseTime(created)
	c.LastPing, c.StartedAt, c.LastSuccess = parseTime(lastPing.String), parseTime(started.String), parseTime(success.String)
	c.DueAt, c.RemindedAt = parseTime(due.String), parseTime(reminded.String)
	c.LastDuration = -1
	if dur.Valid {
		c.LastDuration = time.Duration(dur.Int64) * time.Millisecond
	}
	return c, nil
}

func secs(n int64) time.Duration { return time.Duration(n) * time.Second }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(timeFormat, s)
	return t
}

// fmtTime is a stored time, or NULL for the zero time.
func fmtTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeFormat)
}

func nullSecs(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return int64(d / time.Second)
}

func loadCheck(ctx context.Context, q queryer, key string) (checkin.Check, bool, error) {
	c, err := scanCheck(q.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checkins WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return checkin.Check{}, false, nil
	}
	return c, err == nil, err
}

func saveCheck(ctx context.Context, q queryer, c checkin.Check) error {
	var dur any
	if c.LastDuration >= 0 {
		dur = c.LastDuration.Milliseconds()
	}
	s := c.Schedule
	_, err := q.ExecContext(ctx, `INSERT INTO checkins (`+checkCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET host = excluded.host, period_s = excluded.period_s,
		  cron = excluded.cron, tz = excluded.tz, grace_s = excluded.grace_s,
		  max_runtime_s = excluded.max_runtime_s, reminder_s = excluded.reminder_s,
		  state = excluded.state, state_since = excluded.state_since, down_reason = excluded.down_reason,
		  last_ping = excluded.last_ping, last_event = excluded.last_event, started_at = excluded.started_at,
		  start_rid = excluded.start_rid, last_success = excluded.last_success,
		  last_duration_ms = excluded.last_duration_ms, last_log_event = excluded.last_log_event,
		  due_at = excluded.due_at, reminded_at = excluded.reminded_at`,
		c.Key, nullStr(c.Host), nullSecs(s.Period), nullStr(s.Cron), nullStr(s.TZ), int64(s.Grace/time.Second),
		nullSecs(s.MaxRuntime), int64(s.Reminder/time.Second), c.State, fmtTime(c.StateSince),
		nullStr(c.DownReason), fmtTime(c.CreatedAt), fmtTime(c.LastPing), nullStr(c.LastEvent),
		fmtTime(c.StartedAt), nullStr(c.StartRID), fmtTime(c.LastSuccess), dur, nullStr(c.LastLogEvent),
		fmtTime(c.DueAt), fmtTime(c.RemindedAt))
	return err
}

// ScheduleFrom converts a sender's schedule, filling the defaults: an hour of
// grace and a reminder a day. nil is an unscheduled check with the defaults.
func ScheduleFrom(in *api.Schedule) checkin.Schedule {
	s := checkin.Schedule{Grace: checkin.DefaultGrace, Reminder: checkin.DefaultReminder}
	if in == nil {
		return s
	}
	if in.PeriodS != nil {
		s.Period = secs(int64(*in.PeriodS))
	}
	if in.Cron != nil {
		s.Cron = *in.Cron
	}
	if in.Tz != nil {
		s.TZ = *in.Tz
	}
	if in.GraceS != nil {
		s.Grace = secs(int64(*in.GraceS))
	}
	if in.MaxRuntimeS != nil {
		s.MaxRuntime = secs(int64(*in.MaxRuntimeS))
	}
	return s
}

// deriveCheckin applies a check-in event to its check, creating the check
// when it is new. A new check takes the event's schedule; a known one keeps
// its own, as the envelope's Schedule says.
func deriveCheckin(ctx context.Context, tx *sql.Tx, ev NewEvent, key string) error {
	in := ev.Typed
	c, found, err := loadCheck(ctx, tx, key)
	if err != nil {
		return err
	}
	if !found {
		var sched *api.Schedule
		if in.Checkin != nil {
			sched = in.Checkin.Schedule
		}
		s := ScheduleFrom(sched)
		if s.Validate() != nil {
			// A bad cron or tz must not lose the event; the check starts
			// unscheduled and the API can fix it.
			s = ScheduleFrom(nil)
		}
		c = checkin.New(key, s, ev.ReceivedAt)
		if in.Host != nil {
			c.Host = *in.Host
		}
	}
	p := checkin.Ping{At: ev.ReceivedAt, Duration: -1, EventID: ev.ID, HasLog: in.Body != nil && *in.Body != ""}
	if in.Action != nil {
		p.Action = string(*in.Action)
	}
	if ck := in.Checkin; ck != nil {
		if ck.Rid != nil {
			p.RID = *ck.Rid
		}
		p.ExitCode = ck.ExitCode
		if ck.DurationMs != nil {
			p.Duration = time.Duration(*ck.DurationMs) * time.Millisecond
		}
	}
	c.Apply(p)
	return saveCheck(ctx, tx, c)
}

// CheckView is a check with what the read API shows beside it.
type CheckView struct {
	checkin.Check
	// LastLog is the body of the last ping that carried one.
	LastLog string
	// LastLogAt is when that ping came.
	LastLogAt time.Time
	// Incident is the state of the check's incident, checkin:<key>: open,
	// acked or resolved, or empty when it never had one.
	Incident string
}

// Check returns one check.
func (s *Store) Check(ctx context.Context, key string) (CheckView, error) {
	vs, err := s.checkViews(ctx, `WHERE c.key = ?`, key)
	if err != nil {
		return CheckView{}, err
	}
	if len(vs) == 0 {
		return CheckView{}, ErrNotFound
	}
	return vs[0], nil
}

// Checks returns every check in key order.
func (s *Store) Checks(ctx context.Context) ([]CheckView, error) {
	return s.checkViews(ctx, ``)
}

func (s *Store) checkViews(ctx context.Context, where string, args ...any) ([]CheckView, error) {
	cols := "c." + strings.Join(checkColList, ", c.")
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+`,
		  json_extract(l.event, '$.body'), l.received_at, i.state
		FROM checkins c
		LEFT JOIN events l ON l.id = c.last_log_event
		LEFT JOIN incidents i ON i.key = '`+checkin.IncidentPrefix+`' || c.key
		`+where+` ORDER BY c.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckView{}
	for rows.Next() {
		var v CheckView
		var log, logAt, inc sql.NullString
		c, err := scanCheck(scanFunc(func(dst ...any) error {
			return rows.Scan(append(dst, &log, &logAt, &inc)...)
		}))
		if err != nil {
			return nil, err
		}
		v.Check, v.LastLog, v.LastLogAt, v.Incident = c, log.String, parseTime(logAt.String), inc.String
		out = append(out, v)
	}
	return out, rows.Err()
}

type scanFunc func(...any) error

func (f scanFunc) Scan(dst ...any) error { return f(dst...) }

// PutCheck creates the check called key, or replaces a known check's host
// and schedule. A changed schedule moves the due time from the last ping
// that ended a run. It returns whether it created the check.
func (s *Store) PutCheck(ctx context.Context, key, host string, sched checkin.Schedule) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, found, err := loadCheck(ctx, tx, key)
	if err != nil {
		return false, err
	}
	now := s.now()
	if !found {
		c = checkin.New(key, sched, now)
	}
	c.Host = host
	c.Schedule = sched
	if found && c.State != checkin.StateNew {
		last := c.LastSuccess
		if c.State == checkin.StateDown && c.DownReason == checkin.ReasonFailed {
			last = c.LastPing
		}
		c.DueAt = time.Time{}
		if !last.IsZero() {
			if next, ok := sched.Next(last); ok {
				c.DueAt = next
			}
		}
		// A check that is late or down for a missed ping is judged afresh
		// under the new schedule.
		if c.State == checkin.StateLate || (c.State == checkin.StateDown && c.DownReason == checkin.ReasonMissed) {
			c.State, c.StateSince, c.DownReason = checkin.StateUp, now, ""
		}
	}
	if err := saveCheck(ctx, tx, c); err != nil {
		return false, err
	}
	return !found, tx.Commit()
}

// SetPaused pauses or resumes a check. Resuming makes it new: it is not
// judged until its next ping. It returns ErrNotFound for an unknown key.
func (s *Store) SetPaused(ctx context.Context, key string, paused bool) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	c, found, err := loadCheck(ctx, s.db, key)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	now := s.now()
	switch {
	case paused && c.State != checkin.StatePaused:
		c.State, c.StateSince, c.DownReason = checkin.StatePaused, now, ""
	case !paused && c.State == checkin.StatePaused:
		c.State, c.StateSince = checkin.StateNew, now
		c.DueAt = time.Time{}
	default:
		return nil
	}
	return saveCheck(ctx, s.db, c)
}

// JudgedChecks returns the checks whose state time can change, and every
// check with an open incident or one that should have one, in key order.
func (s *Store) JudgedChecks(ctx context.Context) ([]checkin.Check, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+checkCols+` FROM checkins
		WHERE state IN ('up', 'late', 'started', 'down')
		   OR key IN (SELECT substr(key, `+strconv.Itoa(len(checkin.IncidentPrefix)+1)+`) FROM incidents
		              WHERE key LIKE '`+checkin.IncidentPrefix+`%' AND state != 'resolved')
		ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []checkin.Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadCheck returns one check without the read API's extras.
func (s *Store) LoadCheck(ctx context.Context, key string) (checkin.Check, bool, error) {
	return loadCheck(ctx, s.db, key)
}

// SaveTicked stores c after Tick changed it, unless a ping changed the check
// since it was read: was is the check as read. It reports whether it
// stored c.
func (s *Store) SaveTicked(ctx context.Context, was, c checkin.Check) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	cur, found, err := loadCheck(ctx, tx, c.Key)
	if err != nil || !found {
		return false, err
	}
	if cur.LastEvent != was.LastEvent || cur.State != was.State || !cur.DueAt.Equal(was.DueAt) ||
		cur.Schedule != was.Schedule {
		return false, nil
	}
	if err := saveCheck(ctx, tx, c); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SetReminded records when a check's incident opened or last reminded.
func (s *Store) SetReminded(ctx context.Context, key string, at time.Time) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE checkins SET reminded_at = ? WHERE key = ?`, fmtTime(at), key)
	return err
}

// IncidentState returns the state of the incident called key: open, acked
// or resolved, or empty when there is none.
func (s *Store) IncidentState(ctx context.Context, key string) (string, error) {
	var st string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM incidents WHERE key = ?`, key).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return st, err
}
