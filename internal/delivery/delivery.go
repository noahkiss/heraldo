// Package delivery is the delivery settings: rows the owner keeps on the
// server that say how matching events are delivered (banner, silent,
// digest or drop), whether they count toward the badge, and what level an
// event that carries none gets.
//
// Resolve decides once, at ingest, after the ingest rules typed the event.
// The server stamps its answer on the stored event as the deliver field.
// The push pipeline, the badge, the inbox list and the digest read the
// stamp. Resolve is a pure function: the server, the preview and the
// replay command share it.
package delivery

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/floor"
)

// Outcome is how a matching event is delivered.
type Outcome string

// The outcomes.
const (
	// Banner delivers at the event's own level.
	Banner Outcome = "banner"
	// Silent stores the event passive: in the inbox, no push.
	Silent Outcome = "silent"
	// Digest stores it passive and hidden from the inbox; the digest lists
	// it once per item. While the digest is off it acts as Silent.
	Digest Outcome = "digest"
	// Drop stores it passive and hidden from the inbox and the digest.
	Drop Outcome = "drop"
)

// Hidden reports whether o hides the event from the inbox.
func (o Outcome) Hidden() bool { return o == Digest || o == Drop }

// Badge is whether a matching event counts toward the badge.
type Badge string

// The badge choices.
const (
	// BadgeAuto leaves it to badge.scope.
	BadgeAuto Badge = "auto"
	BadgeOn   Badge = "on"
	BadgeOff  Badge = "off"
)

// Match selects events. Every field set must match; an empty Match matches
// every event that is not routine.
type Match struct {
	Source  string      `json:"source,omitempty"`
	Tag     string      `json:"tag,omitempty"`
	Level   []api.Level `json:"level,omitempty"`
	Kind    api.Kind    `json:"kind,omitempty"`
	Project string      `json:"project,omitempty"`
	Rule    string      `json:"rule,omitempty"`
	// Key is a key prefix.
	Key string `json:"key,omitempty"`
}

// Row is one delivery setting.
type Row struct {
	ID      string
	Match   Match
	Deliver Outcome // "" means no change
	Badge   Badge   // "" reads as auto
	// DefaultLevel is passive or active, or "".
	DefaultLevel api.Level
	Position     int
	Note         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	UpdatedBy    string
}

// badge is r's badge choice, auto when unset.
func (r Row) badge() Badge {
	if r.Badge == "" {
		return BadgeAuto
	}
	return r.Badge
}

// Decides reports whether r decides delivery or the badge, as opposed to
// only filling a level.
func (r Row) Decides() bool { return r.Deliver != "" || r.badge() != BadgeAuto }

// Names reports whether r names its events explicitly: its match sets a
// source, a key prefix or an ingest rule. A tag, level or empty match does
// not. The replay requires a naming row before an incident or a keep-loud
// event leaves the badge.
func (r Row) Names() bool { return r.Match.Source != "" || r.Match.Key != "" || r.Match.Rule != "" }

// Validate checks r on its own: the reasons a write is refused.
func (r Row) Validate() error {
	switch r.Deliver {
	case "", Banner, Silent, Digest, Drop:
	default:
		return fmt.Errorf("deliver: %q is not banner, silent, digest or drop", r.Deliver)
	}
	switch r.Badge {
	case "", BadgeAuto, BadgeOn, BadgeOff:
	default:
		return fmt.Errorf("badge: %q is not auto, on or off", r.Badge)
	}
	switch r.DefaultLevel {
	case "", api.LevelPassive, api.LevelActive:
	default:
		return fmt.Errorf("default_level: want passive or active, not %q", r.DefaultLevel)
	}
	if !r.Decides() && r.DefaultLevel == "" {
		return errors.New("the row does nothing: set deliver, a badge other than auto, or default_level")
	}
	if r.DefaultLevel != "" && len(r.Match.Level) > 0 {
		return errors.New("default_level fills a missing level, so the row may not match on level")
	}
	if r.badge() == BadgeOn && r.Deliver.Hidden() {
		return fmt.Errorf("badge on with deliver %s would count events the inbox hides", r.Deliver)
	}
	for _, l := range r.Match.Level {
		if !l.Valid() {
			return fmt.Errorf("match.level: %q is not a level", l)
		}
	}
	if r.Match.Kind != "" {
		if !r.Match.Kind.Valid() {
			return fmt.Errorf("match.kind: %q is not a kind", r.Match.Kind)
		}
		if r.Match.Kind == api.KindProgress || r.Match.Kind == api.KindStatus {
			return fmt.Errorf("match.kind: %s events are routine; delivery settings never reach them", r.Match.Kind)
		}
	}
	return nil
}

// Event is what Resolve reads of an event.
type Event struct {
	Source string
	Tags   []string
	// Level is the event's level, "" when it carries none.
	Level   api.Level
	Kind    api.Kind
	Action  string
	Project string
	// Rule is the ingest rule that typed the event, "" for none.
	Rule string
	Key  string
	// Failed marks a failed check-in run.
	Failed bool
}

// FromInput is the view of a typed input. rule is the name of the ingest
// rule that typed it, as rules.Type returns it.
func FromInput(in *api.EventInput, rule string) Event {
	e := Event{Source: in.Source, Rule: rule}
	if in.Kind != nil {
		e.Kind = *in.Kind
	}
	if in.Tags != nil {
		e.Tags = *in.Tags
	}
	if in.Level != nil {
		e.Level = *in.Level
	}
	if in.Action != nil {
		e.Action = string(*in.Action)
	}
	if in.Project != nil {
		e.Project = *in.Project
	}
	if in.Key != nil {
		e.Key = *in.Key
	}
	if e.Kind == api.KindCheckin {
		e.Failed = e.Action == "fail" ||
			(e.Action != "log" && in.Checkin != nil && in.Checkin.ExitCode != nil && *in.Checkin.ExitCode != 0)
	}
	return e
}

// FromStored is the view of a stored event as it was before any stamp:
// the level it carried before the delivery setting, and the ingest rule
// named in details.heraldo.rule. The preview and the replay use it.
func FromStored(ev *api.Event) Event {
	e := Event{Source: ev.Source, Kind: ev.Kind}
	if ev.Tags != nil {
		e.Tags = *ev.Tags
	}
	if ev.Level != nil {
		e.Level = *ev.Level
	}
	if d := ev.Deliver; d != nil {
		switch {
		case d.LevelWas != nil:
			e.Level = *d.LevelWas
		case d.Filled != nil:
			e.Level = ""
		}
	}
	if ev.Action != nil {
		e.Action = string(*ev.Action)
	}
	if ev.Project != nil {
		e.Project = *ev.Project
	}
	if ev.Key != nil {
		e.Key = *ev.Key
	}
	if ev.Details != nil {
		own, _ := (*ev.Details)["heraldo"].(map[string]any)
		e.Rule, _ = own["rule"].(string)
	}
	if e.Kind == api.KindCheckin {
		e.Failed = e.Action == "fail" ||
			(e.Action != "log" && ev.Checkin != nil && ev.Checkin.ExitCode != nil && *ev.Checkin.ExitCode != 0)
	}
	return e
}

// Routine reports whether e is routine: progress, status, or a check-in
// that did not fail. The push table never pushes those (routine.Is), and
// delivery settings never stamp them.
func (e Event) Routine() bool {
	switch e.Kind {
	case api.KindProgress, api.KindStatus:
		return true
	case api.KindCheckin:
		return !e.Failed
	}
	return false
}

// Config is what Resolve needs beyond the rows.
type Config struct {
	// NewSourceLevel is delivery.new_source_level.
	NewSourceLevel api.Level
	// DigestOn is digest.enabled: while it is off, digest acts as silent.
	DigestOn bool
	// Alias maps a project name to the name it is an alias of; nil keeps
	// names as they are.
	Alias func(string) string
}

// Result is Resolve's answer.
type Result struct {
	// Stamp is the deliver field to store; nil means no stamp.
	Stamp *api.DeliverStamp
	// Level is the level to store when SetLevel is true.
	Level    api.Level
	SetLevel bool
	// Row is the index in rows of the row that decided, or -1.
	Row int
	// Effective is the level the event delivers at after the setting:
	// the stored level, or active for none.
	Effective api.Level
}

// As is the outcome Resolve applied: banner without a stamp.
func (r Result) As() Outcome {
	if r.Stamp == nil {
		return Banner
	}
	return Outcome(r.Stamp.As)
}

// BadgeChoice is the stamp's badge, auto without a stamp.
func (r Result) BadgeChoice() Badge {
	if r.Stamp == nil {
		return BadgeAuto
	}
	return Badge(r.Stamp.Badge)
}

// Resolve decides e's delivery by rows, in order. newSource is true when
// e's source is new: not yet reviewed. Rows must be valid (Row.Validate).
// Resolve never raises a level. A routine event, and one no row matched
// and no level was filled for, gets no stamp.
func Resolve(e Event, rows []Row, newSource bool, c Config) Result {
	res := Result{Row: -1, Effective: e.Level}
	if res.Effective == "" {
		res.Effective = api.LevelActive
	}
	if e.Routine() {
		return res
	}
	// Fill a missing level. A fill may lower an event to passive, so the
	// floors guard it too: a protected event keeps today's active.
	var filled api.Level
	effective := e.Level
	protected := floor.Check(floor.Delivery,
		floor.Event{Source: e.Source, Kind: e.Kind, Action: e.Action, Level: e.Level, Failed: e.Failed}) != ""
	if effective == "" && !protected {
		for _, r := range rows {
			if r.DefaultLevel != "" && r.Match.matches(e, "", c.Alias) {
				filled = r.DefaultLevel
				break
			}
		}
		if filled == "" && newSource {
			filled = c.NewSourceLevel
		}
		effective = filled
	}
	if effective == "" {
		effective = api.LevelActive
	}
	// The first row that decides delivery or the badge.
	idx := -1
	for i, r := range rows {
		if r.Decides() && r.Match.matches(e, effective, c.Alias) {
			idx = i
			break
		}
	}
	if idx < 0 && filled == "" {
		return res
	}
	st := &api.DeliverStamp{As: api.DeliverStampAs(Banner), Badge: api.DeliverStampBadge(BadgeAuto)}
	if e.Level != "" {
		lw := e.Level
		st.LevelWas = &lw
	}
	if filled != "" {
		f := filled
		st.Filled = &f
	}
	if newSource {
		t := true
		st.NewSource = &t
	}
	level := effective
	if idx >= 0 {
		r := rows[idx]
		res.Row = idx
		id := r.ID
		st.By = &id
		st.Badge = api.DeliverStampBadge(r.badge())
		want := r.Deliver
		if want == "" {
			want = Banner
		}
		as := want
		if as == Digest && !c.DigestOn {
			as = Silent
		}
		if as != Banner {
			fe := floor.Event{Source: e.Source, Kind: e.Kind, Action: e.Action, Level: effective, Failed: e.Failed}
			if f := floor.Check(floor.Delivery, fe); f != "" {
				as = Banner
				st.Refused = &f
			}
		}
		if as != want {
			w := api.DeliverStampWanted(want)
			st.Wanted = &w
		}
		st.As = api.DeliverStampAs(as)
		if as != Banner {
			level = api.LevelPassive
		}
	}
	// Never louder than the effective level.
	if floor.Rank(level) > floor.Rank(effective) {
		level = effective
	}
	res.Stamp = st
	res.Effective = level
	// The stored level changes only when the setting lowered it, or a fill
	// lowered a missing level; a missing level that stays active stays
	// missing.
	switch {
	case e.Level == "" && level == api.LevelActive:
	case level != e.Level:
		res.Level, res.SetLevel = level, true
	}
	return res
}

// matches reports whether e at level matches m. level "" skips the level
// test: the fill step runs before any level is known.
func (m Match) matches(e Event, level api.Level, alias func(string) string) bool {
	if m.Source != "" && !strings.EqualFold(m.Source, e.Source) {
		return false
	}
	if m.Tag != "" && !slices.ContainsFunc(e.Tags, func(t string) bool { return strings.EqualFold(t, m.Tag) }) {
		return false
	}
	if len(m.Level) > 0 && level != "" && !slices.Contains(m.Level, level) {
		return false
	}
	if m.Kind != "" && m.Kind != e.Kind {
		return false
	}
	if m.Project != "" {
		want, got := m.Project, e.Project
		if alias != nil {
			want, got = alias(want), alias(got)
		}
		if got == "" || !strings.EqualFold(want, got) {
			return false
		}
	}
	if m.Rule != "" && m.Rule != e.Rule {
		return false
	}
	if m.Key != "" && !strings.HasPrefix(e.Key, m.Key) {
		return false
	}
	return true
}

// Matches reports whether e at level matches m; level "" reads as active.
func (m Match) Matches(e Event, level api.Level, alias func(string) string) bool {
	if level == "" {
		level = api.LevelActive
	}
	return m.matches(e, level, alias)
}
