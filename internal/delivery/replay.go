package delivery

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/floor"
)

// Pair is a source and title: one line of a keep-loud list.
type Pair struct{ Source, Title string }

// ReplayEvent is one event's outcome in a replay.
type ReplayEvent struct {
	ID, Source, Title string
	Kind              api.Kind
	Before, After     api.Level
	As                Outcome
	// Row is the id of the row that decided, or "".
	Row     string
	Refused string
	// BadgeBefore and BadgeAfter: whether the event counts toward the
	// badge without the rows and with them.
	BadgeBefore, BadgeAfter bool
	// PushedBefore is the stored why: the event pushed at active or above.
	PushedBefore bool
	Keep         bool
}

// LoudAfter reports whether an event that pushed loud still does: its
// level is not passive and no row hid it.
func (e ReplayEvent) LoudAfter() bool {
	return e.PushedBefore && e.After != api.LevelPassive && !e.As.Hidden()
}

// FiledLine is one line the digest's Filed section would show.
type FiledLine struct {
	Group, Title string
	Count        int
}

// ReplayReport is what a replay found.
type ReplayReport struct {
	Events []ReplayEvent
	// Failures are the reasons the replay fails; empty passes.
	Failures []string
	// Identity counts the events the identity run (no rows) changed.
	Identity int
	// RowHits counts each row's hits, by id.
	RowHits map[string]int
	Filed   []FiledLine
}

// pushedLoud matches a stored why of an event that pushed with sound.
var pushedLoud = regexp.MustCompile(`^pushed (active|time-sensitive|critical)`)

// Replay runs rows over evs, each from its unstamped self, with every
// source known, as delivery.enabled on would at ingest. keep is the
// keep-loud list. scope is badge.scope. It never stores anything.
func Replay(evs []api.Event, rows []Row, keep []Pair, scope string, cfg Config) ReplayReport {
	rep := ReplayReport{RowHits: map[string]int{}}
	keepSet := map[Pair]bool{}
	for _, p := range keep {
		keepSet[Pair{strings.ToLower(p.Source), p.Title}] = true
	}
	byID := map[string]Row{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	fail := func(format string, a ...any) { rep.Failures = append(rep.Failures, fmt.Sprintf(format, a...)) }
	filed := map[string]*FiledLine{}
	var order []string
	for i := range evs {
		ev := &evs[i]
		e := FromStored(ev)
		if e.Routine() {
			continue
		}
		title := ""
		if ev.Title != nil {
			title = *ev.Title
		}
		before := e.Level
		if before == "" {
			before = api.LevelActive
		}
		// The identity run: no rows change nothing.
		if id := Resolve(e, nil, false, cfg); id.Stamp != nil || id.SetLevel {
			rep.Identity++
		}
		res := Resolve(e, rows, false, cfg)
		re := ReplayEvent{ID: ev.Id, Source: ev.Source, Title: title, Kind: ev.Kind, Before: before,
			After: res.Effective, As: res.As(), BadgeBefore: DefaultBadge(e, before, scope),
			BadgeAfter: InBadge(e, res, scope), Keep: keepSet[Pair{strings.ToLower(ev.Source), title}]}
		re.PushedBefore = ev.Why != nil && pushedLoud.MatchString(*ev.Why)
		var row Row
		if res.Row >= 0 {
			row = rows[res.Row]
			re.Row = row.ID
			rep.RowHits[row.ID]++
		}
		if res.Stamp != nil && res.Stamp.Refused != nil {
			re.Refused = *res.Stamp.Refused
		}
		rep.Events = append(rep.Events, re)
		name := fmt.Sprintf("%s %q (%s)", ev.Source, title, ev.Id)
		if floor.Rank(re.After) > floor.Rank(before) {
			fail("louder: %s went %s -> %s", name, before, re.After)
		}
		lowered := floor.Rank(re.After) < floor.Rank(before) || re.As.Hidden()
		if re.Keep && lowered {
			fail("keep-loud lowered: %s went %s -> %s as %s by %s", name, before, re.After, re.As, re.Row)
		}
		fe := floor.Event{Source: e.Source, Kind: e.Kind, Action: e.Action, Level: e.Level, Failed: e.Failed}
		if f := floor.Check(floor.Delivery, fe); f != "" && lowered {
			fail("protected (%s) changed: %s went %s -> %s as %s", f, name, before, re.After, re.As)
		}
		if re.BadgeBefore && !re.BadgeAfter && (re.Keep || ev.Kind == api.KindIncident) {
			if res.Row < 0 || !row.Names() {
				fail("badge: %s leaves the badge without a row that names it (row %q)", name, re.Row)
			}
		}
		if re.As == Digest {
			g := ev.Source + "\x00" + title
			if ev.Key != nil && *ev.Key != "" {
				g = "key\x00" + *ev.Key
			}
			if filed[g] == nil {
				filed[g] = &FiledLine{Group: g, Title: title}
				order = append(order, g)
			}
			filed[g].Count++
		}
	}
	if rep.Identity > 0 {
		fail("identity: no rows changed %d events", rep.Identity)
	}
	for _, g := range order {
		rep.Filed = append(rep.Filed, *filed[g])
	}
	return rep
}

// Print writes the report: one line per changed event when verbose, then
// the totals.
func (rep ReplayReport) Print(w io.Writer, verbose bool) {
	type count struct{ before, after int }
	bySource := map[string]*count{}
	var loudBefore, loudAfter, modelBefore, hidden, badgeIn, badgeOut, badgeJoined, badgeLeft int
	for _, e := range rep.Events {
		c := bySource[e.Source]
		if c == nil {
			c = &count{}
			bySource[e.Source] = c
		}
		if e.PushedBefore {
			loudBefore++
			c.before++
		}
		if e.LoudAfter() {
			loudAfter++
			c.after++
		}
		if e.Before != api.LevelPassive && (e.Kind == api.KindMessage || e.Kind == api.KindIncident || e.Kind == api.KindCheckin) {
			modelBefore++
		}
		if e.As.Hidden() {
			hidden++
		}
		if e.BadgeBefore {
			badgeIn++
		}
		if e.BadgeAfter {
			badgeOut++
		}
		switch {
		case e.BadgeAfter && !e.BadgeBefore:
			badgeJoined++
		case e.BadgeBefore && !e.BadgeAfter:
			badgeLeft++
		}
		changed := e.As != Banner || e.After != e.Before || e.BadgeAfter != e.BadgeBefore
		if verbose && changed {
			fmt.Fprintf(w, "%s  %-20s %-9s -> %-9s %-6s row=%-8s badge %v->%v keep=%v %s %q\n", e.ID, e.Source, e.Before, e.After,
				e.As, e.Row, e.BadgeBefore, e.BadgeAfter, e.Keep, e.Refused, e.Title)
		}
	}
	fmt.Fprintf(w, "events (not routine): %d\n", len(rep.Events))
	fmt.Fprintf(w, "loud (stored why says pushed active or above): before %d, after %d\n", loudBefore, loudAfter)
	fmt.Fprintf(w, "leave the inbox (digest or drop): %d\n", hidden)
	fmt.Fprintf(w, "badge: %d count before, %d after; %d join, %d leave\n", badgeIn, badgeOut, badgeJoined, badgeLeft)
	fmt.Fprintf(w, "identity run (no rows): %d changed\n", rep.Identity)
	var srcs []string
	for s, c := range bySource {
		if c.before != c.after {
			srcs = append(srcs, s)
		}
	}
	sort.Slice(srcs, func(i, j int) bool {
		return bySource[srcs[i]].before-bySource[srcs[i]].after > bySource[srcs[j]].before-bySource[srcs[j]].after
	})
	if len(srcs) > 0 {
		fmt.Fprintln(w, "loud by source, where it changed:")
		for _, s := range srcs {
			fmt.Fprintf(w, "  %-24s %d -> %d\n", s, bySource[s].before, bySource[s].after)
		}
	}
	var ids []string
	for id := range rep.RowHits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		fmt.Fprintln(w, "hits per row:")
		for _, id := range ids {
			fmt.Fprintf(w, "  %-12s %d\n", id, rep.RowHits[id])
		}
	}
	if len(rep.Filed) > 0 {
		fmt.Fprintln(w, "the digest's Filed section:")
		for _, f := range rep.Filed {
			g := strings.ReplaceAll(f.Group, "\x00", " ")
			fmt.Fprintf(w, "  x%-3d %s  (%s)\n", f.Count, f.Title, g)
		}
	}
	if len(rep.Failures) == 0 {
		fmt.Fprintln(w, "PASS")
		return
	}
	fmt.Fprintf(w, "FAIL: %d problems\n", len(rep.Failures))
	for _, f := range rep.Failures {
		fmt.Fprintln(w, "  "+f)
	}
}
