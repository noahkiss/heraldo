// Package digest builds and sends the daily digest: one message, source
// heraldo, thread digest, that sums up the window since the last digest.
// It also owns the stale rule for incidents.
//
// The digest runs on the primary only (heraldo-server gates the ticker).
// It is off by default: digest.enabled, digest.at, digest.tz. Each local day
// sends at most once, at or after digest.at; the send log (store.DigestRun)
// survives restarts. A server that was down at digest.at sends once when it
// comes back, never once per missed day.
//
// The text is deterministic. Config.Summary is the hook for a summary
// paragraph (the LLM, wave 3); without it, or when it fails, the digest is
// the same plain list.
package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

// Source and Thread are the digest event's source and thread.
const (
	Source = "heraldo"
	Thread = "digest"
)

// DefaultWindow is the window of a digest with no earlier digest to start
// from; MaxWindow caps any window.
const (
	DefaultWindow = 24 * time.Hour
	MaxWindow     = 7 * 24 * time.Hour
)

// listCap caps each list in the text; the JSON keeps every item.
const listCap = 10

// summaryTimeout bounds the Summary hook.
const summaryTimeout = 30 * time.Second

// Summarizer writes a summary paragraph for a built digest. It must not
// change d. An error or an empty answer leaves the digest without one.
type Summarizer func(ctx context.Context, d *Digest) (string, error)

// Emitted is the stored digest event.
type Emitted struct {
	ID string
	// Created is false when the dedup id matched an earlier event.
	Created bool
}

// Config configures a Runner.
type Config struct {
	Store    *store.Store
	Settings *settings.Manager
	// Emit stores and pushes an event the server makes itself.
	Emit   func(ctx context.Context, fields map[string]any) (Emitted, error)
	Now    func() time.Time
	Logger *slog.Logger
	// Summary is the summary paragraph hook. nil means none.
	Summary Summarizer
}

// Runner builds digests and sends the daily one.
type Runner struct {
	cfg Config
	// mu serialises sends, so a tick and a manual send never race.
	mu sync.Mutex
}

// New returns a Runner.
func New(c Config) *Runner {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return &Runner{cfg: c}
}

// SetSummary sets the summary paragraph hook.
func (r *Runner) SetSummary(s Summarizer) {
	r.mu.Lock()
	r.cfg.Summary = s
	r.mu.Unlock()
}

// Incident is one incident in a digest.
type Incident struct {
	Key        string     `json:"key"`
	State      string     `json:"state"`
	Title      string     `json:"title"`
	Source     string     `json:"source,omitempty"`
	Project    string     `json:"project,omitempty"`
	Level      string     `json:"level,omitempty"`
	OpenedAt   time.Time  `json:"opened_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	LastAt     time.Time  `json:"last_at"`
	EventID    string     `json:"event_id"`
	// New is set on an incident that opened inside the window.
	New bool `json:"new,omitempty"`
}

// Incidents is the digest's incident section.
type Incidents struct {
	// Opened counts the incidents that opened inside the window.
	Opened int `json:"opened"`
	// Resolved lists the incidents that resolved inside the window.
	Resolved []Incident `json:"resolved"`
	// Open lists the incidents not resolved and not stale.
	Open []Incident `json:"open"`
	// Stale lists the incidents not resolved whose last event is older
	// than stale.after. Empty while stale.enabled is off.
	Stale []Incident `json:"stale"`
}

// Check is one check in a digest: down or late now, or failed inside the
// window.
type Check struct {
	Key        string `json:"key"`
	State      string `json:"state"`
	DownReason string `json:"down_reason,omitempty"`
	// Failures counts the failed runs inside the window.
	Failures int `json:"failures"`
}

// Checks is the digest's check-in section.
type Checks struct {
	// Total counts the checks that are not paused; OK those of them that
	// are not down or late.
	Total  int     `json:"total"`
	OK     int     `json:"ok"`
	Paused int     `json:"paused"`
	Items  []Check `json:"items"`
}

// Job is one progress key that ended inside the window: a print or a job.
type Job struct {
	Key     string    `json:"key"`
	Title   string    `json:"title"`
	Project string    `json:"project,omitempty"`
	EndedAt time.Time `json:"ended_at"`
	EventID string    `json:"event_id"`
}

// HeldEntry is one alert the hold still keeps back.
type HeldEntry struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// Held is the digest's hold section.
type Held struct {
	// Open lists the entries still held.
	Open []HeldEntry `json:"open"`
	// Closed counts the entries triage closed inside the window.
	Closed int `json:"closed"`
}

// Project is the count of events of one project inside the window, routine
// events left out. Name is empty for events with no project.
type Project struct {
	Name   string `json:"name"`
	Events int    `json:"events"`
}

// Digest is one built digest.
type Digest struct {
	// Day is the local day it is for, YYYY-MM-DD in TZ.
	Day   string    `json:"day"`
	TZ    string    `json:"tz"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Level is passive, or active when an incident is open or a check is
	// down.
	Level     string    `json:"level"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Body      string    `json:"body"`
	Incidents Incidents `json:"incidents"`
	Checkins  Checks    `json:"checkins"`
	Finished  []Job     `json:"finished"`
	Held      Held      `json:"held"`
	Projects  []Project `json:"projects"`
	// Prose is the summary paragraph from the Summary hook; empty without
	// one.
	Prose string `json:"prose,omitempty"`
}

// Schedule is the digest's settings as they stand.
type Schedule struct {
	Enabled bool
	At      string
	TZ      string
	Loc     *time.Location
}

// Schedule reads the digest settings.
func (r *Runner) Schedule() Schedule {
	s := Schedule{
		Enabled: settings.From[bool](r.cfg.Settings, "digest.enabled"),
		At:      settings.From[string](r.cfg.Settings, "digest.at"),
		TZ:      settings.From[string](r.cfg.Settings, "digest.tz"),
	}
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		loc = time.UTC
	}
	s.Loc = loc
	return s
}

// Due returns the local day of now and the time the digest of that day is
// due.
func (s Schedule) Due(now time.Time) (day string, due time.Time) {
	local := now.In(s.Loc)
	hh, mm := 7, 30
	if t, err := time.Parse("15:04", s.At); err == nil {
		hh, mm = t.Hour(), t.Minute()
	}
	y, m, d := local.Date()
	return local.Format("2006-01-02"), time.Date(y, m, d, hh, mm, 0, 0, s.Loc)
}

// StaleAfter returns how old an open incident's last event must be for it
// to be stale, and whether the stale sweep is on.
func StaleAfter(m *settings.Manager) (time.Duration, bool) {
	return settings.From[time.Duration](m, "stale.after"), settings.From[bool](m, "stale.enabled")
}

// IsStale reports whether an incident not resolved, whose last event
// arrived at last, is stale at now.
func IsStale(last, now time.Time, after time.Duration) bool {
	return after > 0 && !last.IsZero() && now.Sub(last) >= after
}

// Run sends the daily digest when it is due, checking every interval until
// ctx ends. The first check is at once: a server that starts after
// digest.at catches up.
func (r *Runner) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Error("digest", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick sends today's digest when the digest is on, digest.at has passed,
// and today's has not gone out. It reports whether it sent one.
func (r *Runner) Tick(ctx context.Context) (bool, error) {
	s := r.Schedule()
	if !s.Enabled {
		return false, nil
	}
	now := r.cfg.Now()
	day, due := s.Due(now)
	if now.Before(due) {
		return false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sent, err := r.cfg.Store.DigestSent(ctx, day)
	if err != nil || sent {
		return false, err
	}
	d, err := r.build(ctx, now, s)
	if err != nil {
		return false, err
	}
	// The dedup id makes a retry after a crash between the store and the
	// log store nothing new.
	ev, err := r.cfg.Emit(ctx, d.Fields("digest:"+day))
	if err != nil {
		return false, fmt.Errorf("emit: %w", err)
	}
	if err := r.cfg.Store.RecordDigest(ctx, store.DigestRun{
		Day: day, SentAt: now, Since: d.Since, Until: d.Until, EventID: ev.ID,
	}); err != nil {
		return false, fmt.Errorf("record: %w", err)
	}
	r.cfg.Logger.Info("digest: sent", "day", day, "event", ev.ID, "level", d.Level, "new", ev.Created)
	return ev.Created, nil
}

// Send builds and sends a digest now, outside the schedule. It does not
// count as the day's digest. It returns the digest and the stored event.
func (r *Runner) Send(ctx context.Context) (*Digest, Emitted, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, err := r.build(ctx, r.cfg.Now(), r.Schedule())
	if err != nil {
		return nil, Emitted{}, err
	}
	ev, err := r.cfg.Emit(ctx, d.Fields(""))
	return d, ev, err
}

// Preview builds the digest as it would go out now.
func (r *Runner) Preview(ctx context.Context) (*Digest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.build(ctx, r.cfg.Now(), r.Schedule())
}

// Fields is the digest as the fields of an event the server emits. dedup,
// when set, is the event's dedup id.
func (d *Digest) Fields(dedup string) map[string]any {
	f := map[string]any{
		"source": Source, "kind": "message", "thread": Thread, "level": d.Level,
		"title": d.Title, "summary": d.Summary, "body": d.Body,
		"details": map[string]any{"heraldo": map[string]any{"digest": map[string]any{
			"day": d.Day, "since": d.Since.UTC().Format(time.RFC3339), "until": d.Until.UTC().Format(time.RFC3339),
			"opened": d.Incidents.Opened, "resolved": len(d.Incidents.Resolved),
			"open": len(d.Incidents.Open) + len(d.Incidents.Stale), "stale": len(d.Incidents.Stale),
			"checks_ok": d.Checkins.OK, "checks_total": d.Checkins.Total,
			"finished": len(d.Finished), "held": len(d.Held.Open),
		}}},
	}
	if dedup != "" {
		f["dedup"] = dedup
	}
	return f
}

// window returns the digest window that ends at until: from the end of the
// last digest, or DefaultWindow back without one, never more than
// MaxWindow.
func (r *Runner) window(ctx context.Context, until time.Time) (time.Time, error) {
	last, found, err := r.cfg.Store.LastDigest(ctx)
	if err != nil {
		return time.Time{}, err
	}
	since := until.Add(-DefaultWindow)
	if found && last.Until.Before(until) {
		since = last.Until
	}
	if until.Sub(since) > MaxWindow {
		since = until.Add(-MaxWindow)
	}
	return since, nil
}

// eventHead is the part of a stored event the digest reads.
type eventHead struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Key     string `json:"key"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Level   string `json:"level"`
	Project string `json:"project"`
	// ReceivedAt is when the event arrived.
	ReceivedAt time.Time `json:"received_at"`
}

func head(raw json.RawMessage) eventHead {
	var h eventHead
	_ = json.Unmarshal(raw, &h)
	return h
}

func (h eventHead) headline() string {
	switch {
	case h.Title != "":
		return h.Title
	case h.Summary != "":
		return h.Summary
	}
	return h.Key
}

func (r *Runner) build(ctx context.Context, until time.Time, s Schedule) (*Digest, error) {
	st := r.cfg.Store
	if st == nil {
		return nil, errors.New("no store")
	}
	since, err := r.window(ctx, until)
	if err != nil {
		return nil, err
	}
	day, _ := s.Due(until)
	d := &Digest{Day: day, TZ: s.Loc.String(), Since: since.UTC(), Until: until.UTC(),
		Incidents: Incidents{Resolved: []Incident{}, Open: []Incident{}, Stale: []Incident{}},
		Checkins:  Checks{Items: []Check{}}, Finished: []Job{}, Held: Held{Open: []HeldEntry{}},
		Projects: []Project{}}
	in := func(t time.Time) bool { return !t.Before(since) && t.Before(until) }

	incs, err := st.DigestIncidents(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("incidents: %w", err)
	}
	staleAfter, staleOn := StaleAfter(r.cfg.Settings)
	for _, x := range incs {
		h := head(x.Event)
		it := Incident{Key: x.Key, State: x.State, Title: h.headline(), Source: h.Source, Project: x.Project,
			Level: h.Level, OpenedAt: x.OpenedAt.UTC(), LastAt: x.LastAt.UTC(), EventID: h.ID, New: in(x.OpenedAt)}
		if it.New {
			d.Incidents.Opened++
		}
		switch {
		case x.State == "resolved":
			if !in(x.ResolvedAt) {
				continue
			}
			t := x.ResolvedAt.UTC()
			it.ResolvedAt = &t
			d.Incidents.Resolved = append(d.Incidents.Resolved, it)
		case staleOn && IsStale(x.LastAt, until, staleAfter):
			d.Incidents.Stale = append(d.Incidents.Stale, it)
		default:
			d.Incidents.Open = append(d.Incidents.Open, it)
		}
	}

	checks, err := st.Checks(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("checks: %w", err)
	}
	fails, err := st.CheckFailures(ctx, since, until)
	if err != nil {
		return nil, fmt.Errorf("check failures: %w", err)
	}
	for _, c := range checks {
		if c.State == checkin.StatePaused {
			d.Checkins.Paused++
			continue
		}
		d.Checkins.Total++
		bad := c.State == checkin.StateDown || c.State == checkin.StateLate
		if !bad {
			d.Checkins.OK++
		}
		if bad || fails[c.Key] > 0 {
			d.Checkins.Items = append(d.Checkins.Items, Check{Key: c.Key, State: c.State,
				DownReason: c.DownReason, Failures: fails[c.Key]})
		}
	}
	sort.SliceStable(d.Checkins.Items, func(i, j int) bool {
		return checkRank(d.Checkins.Items[i].State) < checkRank(d.Checkins.Items[j].State)
	})

	ended, err := st.EndedProgress(ctx, since, until)
	if err != nil {
		return nil, fmt.Errorf("progress: %w", err)
	}
	for _, raw := range ended {
		h := head(raw)
		d.Finished = append(d.Finished, Job{Key: h.Key, Title: h.headline(), Project: h.Project,
			EndedAt: h.ReceivedAt.UTC(), EventID: h.ID})
	}

	holds, err := st.Holds(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("hold: %w", err)
	}
	for _, e := range holds {
		switch {
		case e.Open():
			t := e.Title
			if t == "" {
				t = e.Tag
			}
			d.Held.Open = append(d.Held.Open, HeldEntry{ID: e.ID, Title: t, At: e.TS.UTC()})
		case !e.ClosedAt.IsZero() && in(e.ClosedAt):
			d.Held.Closed++
		}
	}

	pcs, err := st.ProjectCounts(ctx, since, until, Source, Thread)
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	for _, p := range pcs {
		d.Projects = append(d.Projects, Project{Name: p.Name, Events: int(p.Events)})
	}

	d.Level = "passive"
	if d.attention() {
		d.Level = "active"
	}
	d.Title = "Digest for " + until.In(s.Loc).Format("Mon 2 Jan")
	d.Summary = d.summaryLine()
	if r.cfg.Summary != nil {
		sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
		p, err := r.cfg.Summary(sctx, d)
		cancel()
		if err != nil {
			r.cfg.Logger.Warn("digest: summary paragraph failed; sending the plain digest", "err", err)
		} else {
			d.Prose = strings.TrimSpace(p)
		}
	}
	d.Body = d.render(s.Loc)
	return d, nil
}

// attention reports whether something still needs the owner: an incident
// open and not acked, or a check down or late.
func (d *Digest) attention() bool {
	for _, l := range [][]Incident{d.Incidents.Open, d.Incidents.Stale} {
		for _, i := range l {
			if i.State == "open" {
				return true
			}
		}
	}
	return d.Checkins.OK < d.Checkins.Total
}

func checkRank(state string) int {
	switch state {
	case checkin.StateDown:
		return 0
	case checkin.StateLate:
		return 1
	}
	return 2
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// summaryLine is the one-line push text.
func (d *Digest) summaryLine() string {
	var parts []string
	open := len(d.Incidents.Open) + len(d.Incidents.Stale)
	if open > 0 {
		parts = append(parts, plural(open, "incident open", "incidents open"))
	} else {
		parts = append(parts, "no open incidents")
	}
	if n := len(d.Incidents.Stale); n > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", n))
	}
	if d.Checkins.Total > 0 {
		parts = append(parts, fmt.Sprintf("checks %d/%d ok", d.Checkins.OK, d.Checkins.Total))
	}
	if n := len(d.Finished); n > 0 {
		parts = append(parts, fmt.Sprintf("%d finished", n))
	}
	if n := len(d.Held.Open); n > 0 {
		parts = append(parts, fmt.Sprintf("%d held", n))
	}
	return strings.Join(parts, ", ")
}

// render writes the Markdown body. Times are in loc.
func (d *Digest) render(loc *time.Location) string {
	at := func(t time.Time) string { return t.In(loc).Format("Mon 15:04") }
	var b strings.Builder
	fmt.Fprintf(&b, "From %s to %s (%s).\n", at(d.Since), at(d.Until), loc.String())
	if d.Prose != "" {
		fmt.Fprintf(&b, "\n%s\n", d.Prose)
	}
	list := func(lines []string) {
		for i, l := range lines {
			if i == listCap {
				fmt.Fprintf(&b, "- and %d more\n", len(lines)-listCap)
				break
			}
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	inc := func(i Incident) string {
		s := fmt.Sprintf("`%s` %s", i.Key, i.Title)
		if i.Project != "" {
			s += " (" + i.Project + ")"
		}
		return s
	}

	open := len(d.Incidents.Open) + len(d.Incidents.Stale)
	fmt.Fprintf(&b, "\n### Incidents\n%d opened, %d resolved, %d still open.\n",
		d.Incidents.Opened, len(d.Incidents.Resolved), open)
	var lines []string
	for _, i := range d.Incidents.Open {
		l := inc(i) + ", " + i.State + " since " + at(i.OpenedAt)
		if i.New {
			l += ", new"
		}
		lines = append(lines, l)
	}
	list(lines)
	lines = nil
	for _, i := range d.Incidents.Resolved {
		lines = append(lines, inc(i)+", resolved "+at(*i.ResolvedAt))
	}
	list(lines)

	if len(d.Incidents.Stale) > 0 {
		fmt.Fprintf(&b, "\n### Stale\n%s with no update for a long time.\n",
			plural(len(d.Incidents.Stale), "incident open", "incidents open"))
		lines = nil
		for _, i := range d.Incidents.Stale {
			lines = append(lines, inc(i)+", "+i.State+" since "+at(i.OpenedAt)+", last update "+at(i.LastAt))
		}
		list(lines)
	}

	fmt.Fprintf(&b, "\n### Check-ins\n%d/%d ok", d.Checkins.OK, d.Checkins.Total)
	if d.Checkins.Paused > 0 {
		fmt.Fprintf(&b, ", %d paused", d.Checkins.Paused)
	}
	b.WriteString(".\n")
	lines = nil
	for _, c := range d.Checkins.Items {
		l := "`" + c.Key + "` " + c.State
		if c.DownReason != "" {
			l += " (" + c.DownReason + ")"
		}
		if c.Failures > 0 {
			l += ", failed " + plural(c.Failures, "time", "times")
		}
		lines = append(lines, l)
	}
	list(lines)

	fmt.Fprintf(&b, "\n### Finished\n%s.\n", plural(len(d.Finished), "job or print ended", "jobs and prints ended"))
	lines = nil
	for _, j := range d.Finished {
		l := "`" + j.Key + "` " + j.Title
		if j.Project != "" {
			l += " (" + j.Project + ")"
		}
		lines = append(lines, l+", ended "+at(j.EndedAt))
	}
	list(lines)

	fmt.Fprintf(&b, "\n### Held\n%d still held, %d closed.\n", len(d.Held.Open), d.Held.Closed)
	lines = nil
	for _, h := range d.Held.Open {
		lines = append(lines, h.Title+", held since "+at(h.At)+" ("+h.ID+")")
	}
	list(lines)

	b.WriteString("\n### Projects\n")
	if len(d.Projects) == 0 {
		b.WriteString("No events.\n")
	} else {
		var ps []string
		for _, p := range d.Projects {
			name := p.Name
			if name == "" {
				name = "no project"
			}
			ps = append(ps, fmt.Sprintf("%s %d", name, p.Events))
		}
		b.WriteString(strings.Join(ps, ", ") + ".\n")
	}
	return b.String()
}
