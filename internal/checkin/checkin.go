// Package checkin holds the check-in rules: when a check expects its next
// ping, how a ping changes a check, and how time alone changes it. It has no
// storage and no clock of its own, so tests drive it with fixed times.
//
// A check is in one of these states:
//
//	new      known, but no ping has come yet. Time does not judge it.
//	up       the last run succeeded and the next ping is not due yet.
//	started  a run started and has not ended.
//	late     the next ping is due, and the grace period has not run out.
//	down     a run failed, ran past max_runtime_s, or the grace ran out.
//	paused   not judged. Pings are still recorded. Resume makes it new.
package checkin

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/robfig/cron/v3"
)

// States.
const (
	StateNew     = "new"
	StateUp      = "up"
	StateStarted = "started"
	StateLate    = "late"
	StateDown    = "down"
	StatePaused  = "paused"
)

// Reasons a check is down.
const (
	ReasonFailed  = "failed"
	ReasonMissed  = "missed"
	ReasonOverran = "overran"
)

// Defaults for a schedule that leaves them out.
const (
	DefaultGrace    = time.Hour
	DefaultReminder = 24 * time.Hour
)

// IncidentPrefix starts the key of a check's incident: checkin:<slug>.
const IncidentPrefix = "checkin:"

// Slug is what a check's key may hold. It stays well under the 255 of an
// event key, so the incident key fits too.
var Slug = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// Schedule is when a check expects a ping. Period and Cron are exclusive;
// with neither, the check is unscheduled and only a failed or overrun run
// takes it down.
type Schedule struct {
	Period     time.Duration
	Cron       string
	TZ         string
	Grace      time.Duration
	MaxRuntime time.Duration
	// Reminder is the gap between reminders while the check's incident is
	// open. 0 sends none.
	Reminder time.Duration
}

// Scheduled reports whether the schedule expects pings at all.
func (s Schedule) Scheduled() bool { return s.Period > 0 || s.Cron != "" }

// Validate checks the schedule's fields against each other.
func (s Schedule) Validate() error {
	if s.Period > 0 && s.Cron != "" {
		return errors.New("set a period or a cron expression, not both")
	}
	if s.TZ != "" && s.Cron == "" {
		return errors.New("tz needs cron")
	}
	if s.Period < 0 || s.Grace < 0 || s.MaxRuntime < 0 || s.Reminder < 0 {
		return errors.New("durations may not be negative")
	}
	if s.Cron != "" {
		if _, err := s.parse(); err != nil {
			return err
		}
	}
	return nil
}

func (s Schedule) parse() (cron.Schedule, error) {
	loc := time.UTC
	if s.TZ != "" {
		l, err := time.LoadLocation(s.TZ)
		if err != nil {
			return nil, fmt.Errorf("tz %q: %v", s.TZ, err)
		}
		loc = l
	}
	sched, err := cron.ParseStandard(s.Cron)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %v", s.Cron, err)
	}
	if spec, ok := sched.(*cron.SpecSchedule); ok {
		spec.Location = loc
	}
	return sched, nil
}

// Next returns when the ping after one at t is due. ok is false for an
// unscheduled check, or a cron expression that never fires again.
func (s Schedule) Next(t time.Time) (time.Time, bool) {
	switch {
	case s.Period > 0:
		return t.Add(s.Period), true
	case s.Cron != "":
		sched, err := s.parse()
		if err != nil {
			return time.Time{}, false
		}
		n := sched.Next(t)
		return n, !n.IsZero()
	}
	return time.Time{}, false
}

// Check is one check's state.
type Check struct {
	Key        string
	Host       string
	Schedule   Schedule
	State      string
	StateSince time.Time
	CreatedAt  time.Time
	// LastPing is the last ping of any kind; zero before the first.
	LastPing time.Time
	// StartedAt is the start of the run in progress, zero when none is.
	StartedAt   time.Time
	StartRID    string
	LastSuccess time.Time
	// LastDuration is the last measured run; -1 when none was.
	LastDuration time.Duration
	LastEvent    string
	LastLogEvent string
	// DueAt is when the next ping is due; zero when nothing is expected.
	DueAt      time.Time
	DownReason string
	// RemindedAt is when the check's incident last opened or reminded.
	RemindedAt time.Time
}

// New returns a check created at t.
func New(key string, sched Schedule, t time.Time) Check {
	return Check{Key: key, Schedule: sched, State: StateNew, StateSince: t, CreatedAt: t, LastDuration: -1}
}

// Ping is one check-in event as it applies to a check.
type Ping struct {
	// Action is start, success, fail or log; an exit code other than 0
	// turns anything but log into fail.
	Action   string
	At       time.Time
	RID      string
	ExitCode *int
	// Duration is the sender's measurement, or -1.
	Duration time.Duration
	HasLog   bool
	EventID  string
}

// Apply changes c for one ping. A paused check records the ping but stays
// paused.
func (c *Check) Apply(p Ping) {
	action := p.Action
	if action == "" {
		action = "success"
	}
	if p.ExitCode != nil && action != "log" {
		if *p.ExitCode != 0 {
			action = "fail"
		} else if action != "start" {
			action = "success"
		}
	}
	c.LastPing = p.At
	c.LastEvent = p.EventID
	if p.HasLog {
		c.LastLogEvent = p.EventID
	}
	state := ""
	switch action {
	case "log":
	case "start":
		c.StartedAt = p.At
		c.StartRID = p.RID
		state = StateStarted
	case "success", "fail":
		c.endRun(p)
		if next, ok := c.Schedule.Next(p.At); ok {
			c.DueAt = next
		} else {
			c.DueAt = time.Time{}
		}
		if action == "success" {
			c.LastSuccess = p.At
			state = StateUp
		} else {
			state = StateDown
		}
	default:
		return
	}
	if state == "" || c.State == StatePaused {
		return
	}
	c.setState(state, p.At)
	if state == StateDown {
		c.DownReason = ReasonFailed
	} else {
		c.DownReason = ""
	}
}

// endRun measures the run that p ends. With a run id, only the start with
// the same id counts; a mismatch leaves the run in progress alone.
func (c *Check) endRun(p Ping) {
	if p.RID != "" && p.RID != c.StartRID {
		if p.Duration >= 0 {
			c.LastDuration = p.Duration
		}
		return
	}
	switch {
	case p.Duration >= 0:
		c.LastDuration = p.Duration
	case !c.StartedAt.IsZero():
		c.LastDuration = p.At.Sub(c.StartedAt)
	}
	c.StartedAt = time.Time{}
	c.StartRID = ""
}

func (c *Check) setState(state string, at time.Time) {
	if c.State != state {
		c.State = state
		c.StateSince = at
	}
}

// Tick applies the passage of time at now and reports whether c changed.
// up turns late at DueAt and down when the grace runs out; a started run
// goes down past max_runtime_s, or past the grace like up. Nothing goes down
// before notBefore plus the check's grace: the server sets notBefore to its
// own start, so pings lost while it was down raise nothing.
func (c *Check) Tick(now, notBefore time.Time) bool {
	switch c.State {
	case StateUp, StateLate, StateStarted:
	default:
		return false
	}
	grace := c.Schedule.Grace
	if c.State == StateStarted && c.Schedule.MaxRuntime > 0 && !c.StartedAt.IsZero() {
		end := later(c.StartedAt.Add(c.Schedule.MaxRuntime), notBefore)
		if !now.Before(end) {
			c.setState(StateDown, now)
			c.DownReason = ReasonOverran
			return true
		}
	}
	if c.DueAt.IsZero() {
		return false
	}
	if !now.Before(later(c.DueAt, notBefore).Add(grace)) {
		c.setState(StateDown, now)
		c.DownReason = ReasonMissed
		return true
	}
	if c.State == StateUp && !now.Before(c.DueAt) {
		c.setState(StateLate, now)
		return true
	}
	return false
}

// DownAfter is when c goes down if no ping comes, or zero.
func (c Check) DownAfter() time.Time {
	if c.DueAt.IsZero() {
		return time.Time{}
	}
	return c.DueAt.Add(c.Schedule.Grace)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
