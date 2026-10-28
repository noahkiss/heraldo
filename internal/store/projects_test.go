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
)

// withSource sets ev's source, and its project unless project is "".
func withSource(ev NewEvent, source, project string) NewEvent {
	ev.Fields["source"], ev.Typed.Source = source, source
	if project != "" {
		ev.Fields["project"], ev.Typed.Project = project, &project
	}
	return ev
}

func eventProject(t *testing.T, s *Store, id string) any {
	t.Helper()
	r, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(r.Event, &f); err != nil {
		t.Fatal(err)
	}
	return f["project"]
}

func TestProjectFilterAndList(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i, p := range []string{"web", "fleet", "", "web"} {
		ev := withSource(event(fmt.Sprintf("01K3ZX0000AAAAAAAAAAAAAA%02d", i), at.Add(time.Duration(i)*time.Second)), "s", p)
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		projects []string
		want     int
	}{{nil, 4}, {[]string{"web"}, 2}, {[]string{NoProject}, 1}, {[]string{"fleet", NoProject}, 2}, {[]string{"x"}, 0}} {
		recs, err := s.List(ctx, Filter{Projects: c.projects})
		if err != nil || len(recs) != c.want {
			t.Errorf("%v: %d events, %v; want %d", c.projects, len(recs), err, c.want)
		}
	}
	ps, none, err := s.Projects(ctx)
	if err != nil || none != 1 || len(ps) != 2 || ps[1].Name != "web" || ps[1].Events != 2 || !ps[1].LastAt.Equal(at.Add(3*time.Second)) {
		t.Fatalf("Projects = %+v, %d, %v", ps, none, err)
	}
}

func TestBackfillProjects(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	evs := []NewEvent{
		withSource(typed(t, "01K3ZX0000AAAAAAAAAAAAAA01", at, api.KindIncident, "unit:a", "open"), "watchdog", ""),
		// A different sender's ack: only the key ties it to the open.
		withSource(typed(t, "01K3ZX0000AAAAAAAAAAAAAA02", at, api.KindIncident, "unit:a", "ack"), "cli", ""),
		withSource(event("01K3ZX0000AAAAAAAAAAAAAA03", at), "test", ""),
		withSource(event("01K3ZX0000AAAAAAAAAAAAAA04", at), "watchdog", "kept"),
		withSource(typed(t, "01K3ZX0000AAAAAAAAAAAAAA05", at, api.KindCheckin, "job", "success"), "cron", ""),
		withSource(typed(t, "01K3ZX0000AAAAAAAAAAAAAA06", at, api.KindIncident, "checkin:job", "open"), "heraldo", ""),
	}
	for _, ev := range evs {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	decide := func(f map[string]any) string {
		switch f["source"] {
		case "watchdog":
			return "fleet"
		case "cron":
			return "batch"
		}
		return ""
	}
	dry, err := s.BackfillProjects(ctx, decide, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Scanned != 5 || dry.Total() != 4 || dry.Changed["fleet"] != 2 || dry.Changed["batch"] != 2 || dry.Derived != 3 {
		t.Fatalf("dry run = %+v", dry)
	}
	if p := eventProject(t, s, evs[1].ID); p != nil {
		t.Fatalf("the dry run changed an event: %v", p)
	}
	if row, _ := s.Row(ctx, "incidents", "unit:a"); row["project"] != nil {
		t.Fatalf("the dry run changed a row: %v", row)
	}

	res, err := s.BackfillProjects(ctx, decide, false)
	if err != nil || res.Total() != dry.Total() || res.Derived != dry.Derived {
		t.Fatalf("run = %+v, %v; want %+v", res, err, dry)
	}
	for id, want := range map[string]any{evs[0].ID: "fleet", evs[1].ID: "fleet", evs[2].ID: nil, evs[3].ID: "kept", evs[4].ID: "batch", evs[5].ID: "batch"} {
		if p := eventProject(t, s, id); p != want {
			t.Errorf("%s: project %v, want %v", id, p, want)
		}
	}
	if recs, _ := s.List(ctx, Filter{Projects: []string{"fleet"}}); len(recs) != 2 {
		t.Errorf("column: %d fleet events", len(recs))
	}
	for table, key := range map[string]string{"incidents": "checkin:job", "checkins": "job"} {
		if row, _ := s.Row(ctx, table, key); row["project"] != "batch" {
			t.Errorf("%s %s: %v", table, key, row["project"])
		}
	}
	// A live ack after the backfill inherits the filled row.
	ack := withSource(typed(t, "01K3ZX0000AAAAAAAAAAAAAA07", at, api.KindIncident, "unit:a", "resolve"), "cli", "")
	if _, _, err := s.Insert(ctx, ack); err != nil {
		t.Fatal(err)
	}
	if p := eventProject(t, s, ack.ID); p != "fleet" {
		t.Fatalf("resolve after backfill: %v", p)
	}

	again, err := s.BackfillProjects(ctx, decide, false)
	if err != nil || again.Total() != 0 || again.Derived != 0 || again.Scanned != 1 {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
}

func TestProjectFilterUsesIndex(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT seq FROM events WHERE seq > 0 AND (project IN (?, ?)) ORDER BY seq LIMIT 10`, "a", "b")
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
	if !strings.Contains(strings.Join(plan, "\n"), "events_project") {
		t.Fatalf("plan:\n%s", strings.Join(plan, "\n"))
	}
}
