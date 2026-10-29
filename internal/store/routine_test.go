package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/routine"
)

// routineCases are events with whether routine.Is calls them routine.
var routineCases = []struct {
	kind    api.Kind
	action  string
	exit    *int
	routine bool
}{
	{api.KindMessage, "", nil, false},
	{api.KindDigest, "", nil, false},
	{api.KindIncident, "open", nil, false},
	{api.KindIncident, "resolve", nil, false},
	{api.KindCheckin, "start", nil, true},
	{api.KindCheckin, "success", nil, true},
	{api.KindCheckin, "success", ptr(0), true},
	{api.KindCheckin, "success", ptr(2), false},
	{api.KindCheckin, "log", nil, true},
	// A log never pushes, exit code or not: the push table's rule.
	{api.KindCheckin, "log", ptr(2), true},
	{api.KindCheckin, "fail", nil, false},
	{api.KindProgress, "start", nil, true},
	{api.KindProgress, "update", nil, true},
	{api.KindStatus, "set", nil, true},
}

func ptr(n int) *int { return &n }

// The SQL rule and routine.Is agree, and the filter keeps paging in SQL.
func TestRoutineFilter(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	want := map[string]bool{}
	for i, c := range routineCases {
		id := fmt.Sprintf("01K3ZX0000AAAAAAAAAAAAAA%02d", i)
		ev := typed(t, id, at.Add(time.Duration(i)*time.Second), c.kind, fmt.Sprintf("k%d", i), c.action)
		if c.exit != nil {
			ev.Fields["checkin"] = map[string]any{"exit_code": *c.exit}
			ev.Typed.Checkin = &api.Checkin{ExitCode: c.exit}
		}
		if c.kind == api.KindStatus || c.kind == api.KindProgress {
			ev.Fields["content"] = map[string]any{"title": "t"}
		}
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want[id] = c.routine
	}
	all, err := s.List(ctx, Filter{})
	if err != nil || len(all) != len(routineCases) {
		t.Fatalf("all: %d, %v", len(all), err)
	}
	for _, r := range all {
		var ev api.Event
		if err := json.Unmarshal(r.Event, &ev); err != nil {
			t.Fatal(err)
		}
		if got := routine.Is(&ev); got != want[r.ID] {
			t.Errorf("%s (%s %v): routine.Is = %v, want %v", r.ID, ev.Kind, ev.Action, got, want[r.ID])
		}
	}
	for _, mode := range []string{routine.Exclude, routine.Only} {
		recs, err := s.List(ctx, Filter{Routine: mode, Desc: true})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for id, r := range want {
			if r == (mode == routine.Only) {
				n++
				_ = id
			}
		}
		if len(recs) != n {
			t.Fatalf("%s: %d events, want %d", mode, len(recs), n)
		}
		for _, r := range recs {
			if want[r.ID] != (mode == routine.Only) {
				t.Errorf("%s listed %s", mode, r.ID)
			}
		}
		// A limit of 1 pages through the same events in SQL.
		var paged []string
		before := int64(0)
		for {
			p, err := s.List(ctx, Filter{Routine: mode, Desc: true, Limit: 1, BeforeSeq: before})
			if err != nil {
				t.Fatal(err)
			}
			if len(p) == 0 {
				break
			}
			paged = append(paged, p[0].ID)
			before = p[0].Seq
		}
		if len(paged) != n {
			t.Fatalf("%s paged %d events, want %d", mode, len(paged), n)
		}
	}
}

func TestRoutineExcludeUsesIndex(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT seq FROM events WHERE seq > 0 AND NOT ` + RoutineWhere + ` ORDER BY seq DESC LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "events_main") {
		t.Fatalf("plan:\n%s", strings.Join(plan, "\n"))
	}
}
