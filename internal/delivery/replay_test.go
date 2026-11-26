package delivery

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

// fixture is a hand-written export: every name is made up.
const fixture = `
{"id":"e1","received_at":"2026-11-01T00:00:00Z","ts":"2026-11-01T00:00:00Z","source":"printer","kind":"message","title":"Print finished","why":"pushed active to 1 device"}
{"id":"e2","received_at":"2026-11-01T00:01:00Z","ts":"2026-11-01T00:01:00Z","source":"printer","kind":"message","title":"Nozzle clog","why":"pushed active to 1 device","details":{"heraldo":{"rule":"printer-fault"}}}
{"id":"e3","received_at":"2026-11-01T00:02:00Z","ts":"2026-11-01T00:02:00Z","source":"printer","kind":"message","title":"Maintenance due","key":"maint:p1","why":"pushed active to 1 device","details":{"heraldo":{"rule":"printer-maint"}}}
{"id":"e4","received_at":"2026-11-01T00:03:00Z","ts":"2026-11-01T00:03:00Z","source":"printer","kind":"message","title":"Maintenance due","key":"maint:p1","why":"pushed active to 1 device","details":{"heraldo":{"rule":"printer-maint"}}}
{"id":"e5","received_at":"2026-11-01T00:04:00Z","ts":"2026-11-01T00:04:00Z","source":"uptime","kind":"incident","key":"web","action":"open","level":"time-sensitive","title":"web down","why":"pushed time-sensitive to 1 device"}
{"id":"e6","received_at":"2026-11-01T00:05:00Z","ts":"2026-11-01T00:05:00Z","source":"jobs","kind":"checkin","key":"backup","action":"success"}
{"id":"e7","received_at":"2026-11-01T00:06:00Z","ts":"2026-11-01T00:06:00Z","source":"housework","kind":"message","level":"passive","title":"done","why":"passive: inbox only","deliver":{"as":"silent","badge":"auto","level_was":"active","by":"dlv_old"}}
`

func events(t *testing.T) []api.Event {
	t.Helper()
	var out []api.Event
	for _, l := range strings.Split(strings.TrimSpace(fixture), "\n") {
		var ev api.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func rowsOf(t *testing.T, js string) []Row {
	t.Helper()
	rows, err := ParseRows([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

var keepFaults = []Pair{{"Printer", "Nozzle clog"}, {"uptime", "web down"}}

func TestReplayIdentity(t *testing.T) {
	rep := Replay(events(t), nil, keepFaults, ScopeAttention, cfg)
	if len(rep.Failures) != 0 || rep.Identity != 0 || len(rep.Events) != 6 {
		t.Fatalf("%+v", rep)
	}
	// The stamped event replays from its unstamped self: active, loud
	// before the old row, unchanged without rows.
	for _, e := range rep.Events {
		if e.ID == "e7" && (e.Before != api.LevelActive || e.After != api.LevelActive) {
			t.Fatalf("e7: %+v", e)
		}
	}
	var buf bytes.Buffer
	rep.Print(&buf, true)
	if !strings.Contains(buf.String(), "loud (stored why says pushed active or above): before 5, after 5") ||
		!strings.HasSuffix(buf.String(), "PASS\n") {
		t.Fatal(buf.String())
	}
}

func TestReplayGoodRows(t *testing.T) {
	rows := rowsOf(t, `[
		{"match":{"rule":"printer-maint"},"deliver":"digest","badge":"off"},
		{"match":{"source":"printer","rule":"printer-fault"},"deliver":"banner"},
		{"match":{"source":"printer"},"deliver":"silent"},
		{"match":{"source":"uptime"},"badge":"off"}]`)
	rep := Replay(events(t), rows, keepFaults, ScopeAttention, cfg)
	var buf bytes.Buffer
	rep.Print(&buf, true)
	out := buf.String()
	if len(rep.Failures) != 0 {
		t.Fatal(out)
	}
	for _, want := range []string{"before 5, after 2", "leave the inbox (digest or drop): 2", "x2", "PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if rep.RowHits["row0"] != 2 || rep.RowHits["row1"] != 1 || len(rep.Filed) != 1 || rep.Filed[0].Count != 2 {
		t.Fatalf("%+v %+v", rep.RowHits, rep.Filed)
	}
}

func TestReplayFailures(t *testing.T) {
	for _, c := range []struct {
		rows, want string
	}{
		// A source-wide row lowers the keep-loud fault.
		{`[{"match":{"source":"printer"},"deliver":"silent"}]`, "keep-loud lowered"},
		// A tag-less, source-less row takes the incident out of the badge.
		{`[{"match":{"kind":"incident"},"badge":"off"}]`, "leaves the badge without a row that names it"},
	} {
		rep := Replay(events(t), rowsOf(t, c.rows), keepFaults, ScopeAttention, cfg)
		if !strings.Contains(strings.Join(rep.Failures, "\n"), c.want) {
			t.Errorf("%s: failures %v, want %q", c.rows, rep.Failures, c.want)
		}
	}
}

func TestParseRows(t *testing.T) {
	rows := rowsOf(t, `{"enabled":true,"new_source_level":"passive","settings":[
		{"id":"dlv_a","match":{"source":"x"},"deliver":"drop","badge":"auto","position":0,
		 "created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}]}`)
	if len(rows) != 1 || rows[0].ID != "dlv_a" || rows[0].Deliver != Drop {
		t.Fatalf("%+v", rows)
	}
	if _, err := ParseRows([]byte(`[{"deliver":"drop","badge":"on"}]`)); err == nil {
		t.Fatal("an invalid row parsed")
	}
}
