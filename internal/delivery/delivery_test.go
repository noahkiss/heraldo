package delivery

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/floor"
)

var cfg = Config{NewSourceLevel: api.LevelPassive, DigestOn: true}

func msg(source string, level api.Level) Event {
	return Event{Source: source, Kind: api.KindMessage, Level: level}
}

func TestOutcomes(t *testing.T) {
	for _, c := range []struct {
		deliver   Outcome
		digestOn  bool
		wantAs    Outcome
		wantLevel api.Level
		wanted    string
	}{
		{Banner, true, Banner, api.LevelActive, ""},
		{Silent, true, Silent, api.LevelPassive, ""},
		{Digest, true, Digest, api.LevelPassive, ""},
		{Drop, true, Drop, api.LevelPassive, ""},
		{Digest, false, Silent, api.LevelPassive, "digest"},
	} {
		rows := []Row{{ID: "dlv_1", Match: Match{Source: "printer"}, Deliver: c.deliver}}
		cf := cfg
		cf.DigestOn = c.digestOn
		res := Resolve(msg("Printer", api.LevelActive), rows, false, cf)
		if res.Stamp == nil {
			t.Fatalf("%s: no stamp", c.deliver)
		}
		if res.As() != c.wantAs || res.Effective != c.wantLevel {
			t.Errorf("%s: as %s at %s, want %s at %s", c.deliver, res.As(), res.Effective, c.wantAs, c.wantLevel)
		}
		if got := res.SetLevel; got != (c.wantLevel != api.LevelActive) {
			t.Errorf("%s: SetLevel = %v", c.deliver, got)
		}
		w := ""
		if res.Stamp.Wanted != nil {
			w = string(*res.Stamp.Wanted)
		}
		if w != c.wanted {
			t.Errorf("%s: wanted %q, want %q", c.deliver, w, c.wanted)
		}
		if *res.Stamp.By != "dlv_1" || res.Stamp.LevelWas == nil || *res.Stamp.LevelWas != api.LevelActive {
			t.Errorf("%s: stamp %+v", c.deliver, res.Stamp)
		}
	}
}

func TestNeverLouder(t *testing.T) {
	for _, l := range []api.Level{api.LevelPassive, api.LevelActive, api.LevelTimeSensitive, api.LevelCritical, ""} {
		for _, o := range []Outcome{Banner, Silent, Digest, Drop, ""} {
			for _, b := range []Badge{BadgeAuto, BadgeOn, BadgeOff} {
				r := Row{ID: "dlv_x", Deliver: o, Badge: b}
				if r.Validate() != nil {
					continue
				}
				e := msg("s", l)
				res := Resolve(e, []Row{r}, false, cfg)
				before := l
				if before == "" {
					before = api.LevelActive
				}
				if floor.Rank(res.Effective) > floor.Rank(before) {
					t.Errorf("%s %s %s: %s is louder than %s", l, o, b, res.Effective, before)
				}
				if res.SetLevel && floor.Rank(res.Level) > floor.Rank(before) {
					t.Errorf("%s %s %s: stored %s is louder", l, o, b, res.Level)
				}
			}
		}
	}
}

func TestFirstRowWins(t *testing.T) {
	rows := []Row{
		{ID: "a", Match: Match{Source: "other"}, Deliver: Drop},
		{ID: "b", Match: Match{Source: "s"}, Deliver: Silent},
		{ID: "c", Match: Match{Source: "s"}, Deliver: Drop},
	}
	res := Resolve(msg("s", api.LevelActive), rows, false, cfg)
	if res.Row != 1 || res.As() != Silent {
		t.Fatalf("row %d as %s", res.Row, res.As())
	}
}

func TestMatchFields(t *testing.T) {
	e := Event{Source: "PrintBox", Tags: []string{"Printer", "x"}, Level: api.LevelActive, Kind: api.KindMessage,
		Project: "old", Rule: "printer-news", Key: "maint:p1"}
	alias := func(p string) string {
		if p == "old" {
			return "new"
		}
		return p
	}
	for _, c := range []struct {
		m    Match
		want bool
	}{
		{Match{}, true},
		{Match{Source: "printbox"}, true},
		{Match{Source: "print"}, false},
		{Match{Tag: "printer"}, true},
		{Match{Tag: "y"}, false},
		{Match{Level: []api.Level{api.LevelPassive, api.LevelActive}}, true},
		{Match{Level: []api.Level{api.LevelTimeSensitive}}, false},
		{Match{Kind: api.KindMessage}, true},
		{Match{Kind: api.KindIncident}, false},
		{Match{Project: "new"}, true},
		{Match{Project: "old"}, true},
		{Match{Project: "other"}, false},
		{Match{Rule: "printer-news"}, true},
		{Match{Rule: "printer"}, false},
		{Match{Key: "maint:"}, true},
		{Match{Key: "checkin:"}, false},
		{Match{Source: "printbox", Rule: "other"}, false},
	} {
		if got := c.m.Matches(e, api.LevelActive, alias); got != c.want {
			t.Errorf("%+v: match = %v, want %v", c.m, got, c.want)
		}
	}
}

func TestFill(t *testing.T) {
	rows := []Row{{ID: "fill", Match: Match{Source: "tests"}, DefaultLevel: api.LevelPassive}}
	// A row fills a missing level.
	res := Resolve(msg("tests", ""), rows, false, cfg)
	if res.Stamp == nil || !res.SetLevel || res.Level != api.LevelPassive || *res.Stamp.Filled != api.LevelPassive {
		t.Fatalf("fill by row: %+v", res)
	}
	if res.Stamp.LevelWas != nil {
		t.Errorf("level_was set on a level-less event")
	}
	// Never an explicit level.
	if res := Resolve(msg("tests", api.LevelActive), rows, false, cfg); res.Stamp != nil {
		t.Errorf("explicit level stamped: %+v", res.Stamp)
	}
	// A new source gets new_source_level; a known one keeps today's active.
	res = Resolve(msg("fresh", ""), nil, true, cfg)
	if res.Stamp == nil || res.Level != api.LevelPassive || res.Stamp.NewSource == nil {
		t.Errorf("new source: %+v", res)
	}
	if res := Resolve(msg("known", ""), nil, false, cfg); res.Stamp != nil || res.SetLevel {
		t.Errorf("known source changed: %+v", res)
	}
	// new_source_level active fills active and leaves the level missing.
	cf := cfg
	cf.NewSourceLevel = api.LevelActive
	res = Resolve(msg("fresh", ""), nil, true, cf)
	if res.Stamp == nil || res.SetLevel || *res.Stamp.Filled != api.LevelActive {
		t.Errorf("new source at active: %+v", res)
	}
	// A new source that sends a level keeps it (Q5).
	if res := Resolve(msg("fresh", api.LevelActive), nil, true, cfg); res.Stamp != nil {
		t.Errorf("explicit level on a new source stamped: %+v", res.Stamp)
	}
	// The filled level is what rows match on.
	rows = append(rows, Row{ID: "q", Match: Match{Level: []api.Level{api.LevelPassive}}, Deliver: Drop})
	res = Resolve(msg("tests", ""), rows, false, cfg)
	if res.As() != Drop || *res.Stamp.By != "q" {
		t.Errorf("filled level not matched: %+v", res.Stamp)
	}
}

func TestFloors(t *testing.T) {
	rows := []Row{{ID: "all", Deliver: Drop, Badge: BadgeOff}}
	for _, c := range []struct {
		e     Event
		floor string
	}{
		{Event{Source: "s", Kind: api.KindMessage, Level: api.LevelCritical}, "critical floor"},
		{Event{Source: "s", Kind: api.KindMessage, Level: api.LevelTimeSensitive}, "time-sensitive floor"},
		{Event{Source: "s", Kind: api.KindIncident, Action: "open", Level: api.LevelActive, Key: "k"}, "incident floor"},
		{Event{Source: "s", Kind: api.KindIncident, Action: "update", Level: api.LevelActive, Key: "k"}, "incident floor"},
		{Event{Source: "s", Kind: api.KindIncident, Action: "ack", Key: "k"}, "incident floor"},
		{Event{Source: "s", Kind: api.KindIncident, Action: "resolve", Level: api.LevelPassive, Key: "k"}, "incident floor"},
		{Event{Source: "s", Kind: api.KindCheckin, Action: "fail", Key: "k", Failed: true}, "check-in fail floor"},
		{Event{Source: "heraldo", Kind: api.KindMessage, Level: api.LevelPassive}, "heraldo's own event"},
	} {
		res := Resolve(c.e, rows, false, cfg)
		if res.Stamp == nil || res.As() != Banner || res.Stamp.Refused == nil || *res.Stamp.Refused != c.floor {
			t.Errorf("%+v: stamp %+v, want refused by %s", c.e, res.Stamp, c.floor)
			continue
		}
		if *res.Stamp.Wanted != "drop" || res.SetLevel {
			t.Errorf("%+v: wanted %v, SetLevel %v", c.e, *res.Stamp.Wanted, res.SetLevel)
		}
		// The floors never protect the badge (Q6).
		if res.BadgeChoice() != BadgeOff {
			t.Errorf("%+v: badge %s, want off", c.e, res.BadgeChoice())
		}
	}
}

func TestNoStamp(t *testing.T) {
	rows := []Row{{ID: "all", Deliver: Drop}}
	for _, e := range []Event{
		{Source: "s", Kind: api.KindProgress, Key: "k"},
		{Source: "s", Kind: api.KindStatus, Key: "k"},
		{Source: "s", Kind: api.KindCheckin, Action: "success", Key: "k"},
	} {
		if res := Resolve(e, rows, true, cfg); res.Stamp != nil || res.SetLevel {
			t.Errorf("routine %+v stamped", e)
		}
	}
	if res := Resolve(msg("s", api.LevelActive), nil, false, cfg); res.Stamp != nil {
		t.Errorf("zero rows stamped")
	}
	rows = []Row{{ID: "x", Match: Match{Source: "other"}, Deliver: Drop}}
	if res := Resolve(msg("s", api.LevelActive), rows, false, cfg); res.Stamp != nil {
		t.Errorf("no match stamped")
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		r   Row
		err string
	}{
		{Row{Deliver: Silent}, ""},
		{Row{Badge: BadgeOn}, ""},
		{Row{DefaultLevel: api.LevelPassive}, ""},
		{Row{}, "does nothing"},
		{Row{Badge: BadgeAuto}, "does nothing"},
		{Row{Deliver: "loud"}, "deliver"},
		{Row{Badge: "maybe"}, "badge"},
		{Row{DefaultLevel: api.LevelTimeSensitive}, "default_level"},
		{Row{DefaultLevel: api.LevelPassive, Match: Match{Level: []api.Level{api.LevelActive}}}, "may not match on level"},
		{Row{Deliver: Digest, Badge: BadgeOn}, "hides"},
		{Row{Deliver: Drop, Badge: BadgeOn}, "hides"},
		{Row{Deliver: Silent, Badge: BadgeOn}, ""},
		{Row{Deliver: Silent, Match: Match{Kind: api.KindProgress}}, "routine"},
		{Row{Deliver: Silent, Match: Match{Kind: "nope"}}, "not a kind"},
		{Row{Deliver: Silent, Match: Match{Level: []api.Level{"loud"}}}, "not a level"},
	} {
		err := c.r.Validate()
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: %v, want %q", c.r, err, c.err)
		}
	}
}

func TestFromStoredUndoesTheStamp(t *testing.T) {
	var ev api.Event
	raw := `{"id":"x","received_at":"2026-10-26T12:00:00Z","ts":"2026-10-26T12:00:00Z","source":"s","kind":"message",
		"level":"passive","deliver":{"as":"silent","badge":"auto","level_was":"time-sensitive"},
		"details":{"heraldo":{"rule":"r1"}}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	e := FromStored(&ev)
	if e.Level != api.LevelTimeSensitive || e.Rule != "r1" {
		t.Fatalf("%+v", e)
	}
	raw = `{"id":"x","received_at":"2026-10-26T12:00:00Z","ts":"2026-10-26T12:00:00Z","source":"s","kind":"message",
		"level":"passive","deliver":{"as":"banner","badge":"auto","filled":"passive"}}`
	ev = api.Event{}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if e := FromStored(&ev); e.Level != "" {
		t.Fatalf("filled level kept: %+v", e)
	}
}

func TestBadge(t *testing.T) {
	inc := Event{Source: "s", Kind: api.KindIncident, Action: "open", Level: api.LevelActive, Key: "k"}
	ts := msg("s", api.LevelTimeSensitive)
	act := msg("s", api.LevelActive)
	if !DefaultBadge(inc, api.LevelActive, ScopeAttention) || DefaultBadge(act, api.LevelActive, ScopeAttention) ||
		!DefaultBadge(ts, api.LevelTimeSensitive, ScopeAttention) {
		t.Error("attention defaults")
	}
	if !DefaultBadge(act, api.LevelActive, ScopePushed) || DefaultBadge(act, api.LevelPassive, ScopePushed) {
		t.Error("pushed defaults")
	}
	on := []Row{{ID: "on", Deliver: Silent, Badge: BadgeOn}}
	res := Resolve(act, on, false, cfg)
	if !InBadge(act, res, ScopeAttention) || !InBadge(act, res, ScopePushed) || InBadge(act, res, ScopeNone) {
		t.Error("badge on")
	}
	off := []Row{{ID: "off", Match: Match{Kind: api.KindIncident}, Badge: BadgeOff}}
	res = Resolve(inc, off, false, cfg)
	if InBadge(inc, res, ScopeAttention) {
		t.Error("badge off on an incident still counts")
	}
	silent := []Row{{ID: "s", Deliver: Silent}}
	res = Resolve(act, silent, false, cfg)
	if InBadge(act, res, ScopePushed) {
		t.Error("a silent auto event counts under pushed")
	}
}
