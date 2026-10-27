package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// incidentEvent builds an incident event with a level and a re-alert
// interval in seconds (0 for none).
func incidentEvent(t *testing.T, id string, at time.Time, key, action, level string, realert int) NewEvent {
	t.Helper()
	ev := typed(t, id, at, api.KindIncident, key, action)
	if level != "" || realert > 0 {
		f := ev.Fields
		if level != "" {
			f["level"] = level
		}
		if realert > 0 {
			f["details"] = map[string]any{"heraldo": map[string]any{"realert_s": realert}}
		}
		raw, _ := json.Marshal(f)
		var in api.EventInput
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatal(err)
		}
		ev.Typed = in
	}
	return ev
}

type snap struct {
	State      string    `json:"state"`
	OpenedAt   time.Time `json:"opened_at"`
	ResolvedAt time.Time `json:"resolved_at"`
}

func insertSnap(t *testing.T, s *Store, ev NewEvent) snap {
	t.Helper()
	rec, _, err := s.Insert(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	var e struct {
		Incident *snap `json:"incident"`
	}
	if err := json.Unmarshal(rec.Event, &e); err != nil || e.Incident == nil {
		t.Fatalf("stored %s: no incident (%v)", rec.Event, err)
	}
	return *e.Incident
}

func TestIncidentSnapshot(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	min := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	for _, c := range []struct {
		id, key, action string
		at              time.Time
		state           string
		opened          time.Time
		resolved        time.Time
	}{
		{"01K3ZX0000AAAAAAAAAAAAAA01", "web", "open", min(0), "open", min(0), time.Time{}},
		{"01K3ZX0000AAAAAAAAAAAAAA02", "web", "update", min(3), "open", min(0), time.Time{}},
		{"01K3ZX0000AAAAAAAAAAAAAA03", "web", "ack", min(5), "acked", min(0), time.Time{}},
		// An update after an ack stays acked.
		{"01K3ZX0000AAAAAAAAAAAAAA04", "web", "update", min(8), "acked", min(0), time.Time{}},
		{"01K3ZX0000AAAAAAAAAAAAAA05", "web", "resolve", min(12), "resolved", min(0), min(12)},
		// A second resolve keeps the first.
		{"01K3ZX0000AAAAAAAAAAAAAA06", "web", "resolve", min(14), "resolved", min(0), min(12)},
		// An ack of a resolved incident changes nothing.
		{"01K3ZX0000AAAAAAAAAAAAAA07", "web", "ack", min(15), "resolved", min(0), min(12)},
		// A new open reopens it from scratch.
		{"01K3ZX0000AAAAAAAAAAAAAA08", "web", "open", min(20), "open", min(20), time.Time{}},
		// A resolve with no open before it opens and resolves at once.
		{"01K3ZX0000AAAAAAAAAAAAAA09", "db", "resolve", min(21), "resolved", min(21), min(21)},
	} {
		got := insertSnap(t, s, typed(t, c.id, c.at, api.KindIncident, c.key, c.action))
		if got.State != c.state || !got.OpenedAt.Equal(c.opened) || !got.ResolvedAt.Equal(c.resolved) {
			t.Errorf("%s %s: %+v; want %s %v %v", c.key, c.action, got, c.state, c.opened, c.resolved)
		}
	}
	row, err := s.Row(context.Background(), "incidents", "web")
	if err != nil || row["state"] != "open" || row["last_event"] != "01K3ZX0000AAAAAAAAAAAAAA08" || row["resolved_at"] != nil {
		t.Fatalf("row %v %v", row, err)
	}
}

func TestRealertSchedule(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	ins := func(ev NewEvent) {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	due := func(at time.Time) []DueAlert {
		d, err := s.DueAlerts(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// Only a time-sensitive or critical incident with an interval repeats.
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB01", t0, "ts", "open", "time-sensitive", 600))
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB02", t0, "active", "open", "active", 600))
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB03", t0, "none", "open", "critical", 0))
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB04", t0, "acked", "open", "critical", 600))
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB05", t0.Add(time.Minute), "acked", "ack", "", 0))
	if d := due(t0.Add(9 * time.Minute)); len(d) != 0 {
		t.Fatalf("due early: %+v", d)
	}
	d := due(t0.Add(10 * time.Minute))
	if len(d) != 1 || d[0].Key != "ts" || d[0].EventID != "01K3ZX0000BBBBBBBBBBBBBB01" || d[0].Every != 10*time.Minute ||
		!d[0].Due.Equal(t0.Add(10*time.Minute)) || !d[0].OpenedAt.Equal(t0) {
		t.Fatalf("due %+v", d)
	}
	// Moving the schedule works once; a second move from the same due time
	// finds it moved.
	next := d[0].Due.Add(d[0].Every)
	if ok, err := s.Realerted(ctx, "ts", d[0].Due, next); !ok || err != nil {
		t.Fatalf("realerted %v %v", ok, err)
	}
	if ok, _ := s.Realerted(ctx, "ts", d[0].Due, next); ok {
		t.Fatal("moved twice")
	}
	if d := due(t0.Add(15 * time.Minute)); len(d) != 0 {
		t.Fatalf("due after move: %+v", d)
	}
	// An update without an interval keeps the schedule; the resolve ends it.
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB06", t0.Add(16*time.Minute), "ts", "update", "active", 0))
	if d := due(next); len(d) != 1 || d[0].EventID != "01K3ZX0000BBBBBBBBBBBBBB01" {
		t.Fatalf("due after update: %+v", d)
	}
	ins(incidentEvent(t, "01K3ZX0000BBBBBBBBBBBBBB07", t0.Add(17*time.Minute), "ts", "resolve", "", 0))
	if d := due(t0.Add(time.Hour)); len(d) != 0 {
		t.Fatalf("due after resolve: %+v", d)
	}
	keys, err := s.OpenIncidentKeys(ctx)
	if err != nil || len(keys) != 3 || keys[0] != "acked" || keys[1] != "active" || keys[2] != "none" {
		t.Fatalf("open keys %v %v", keys, err)
	}
}

func TestMutes(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, m := range []Mute{
		{ID: "mute_1", Source: "backup", Until: now.Add(time.Hour)},
		{ID: "mute_2", Key: "unit:a:nginx", Until: now.Add(2 * time.Hour)},
		{ID: "mute_3", Source: "uptime", Key: "web", Until: now.Add(30 * time.Minute)},
		{ID: "mute_4", Source: "old", Until: now.Add(-time.Minute)},
	} {
		m.CreatedAt, m.CreatedBy = now, "phone-write"
		if err := s.AddMute(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		source, key, want string
	}{
		{"backup", "", "mute_1"},
		{"backup", "x", "mute_1"},
		{"other", "unit:a:nginx", "mute_2"},
		{"uptime", "web", "mute_3"},
		{"uptime", "db", ""},
		{"other", "web", ""},
		{"old", "", ""},
	} {
		m, ok, err := s.MatchMute(ctx, c.source, c.key, now)
		if err != nil || (ok && m.ID != c.want) || (!ok && c.want != "") {
			t.Errorf("%s/%s: %q %v %v; want %q", c.source, c.key, m.ID, ok, err, c.want)
		}
	}
	ms, err := s.Mutes(ctx, now)
	if err != nil || len(ms) != 3 || ms[0].ID != "mute_3" {
		t.Fatalf("mutes %+v %v", ms, err)
	}
	m, err := s.EndMute(ctx, "mute_1", "phone-write", now)
	if err != nil || m.EndedAt.IsZero() || m.Active(now) {
		t.Fatalf("ended %+v %v", m, err)
	}
	if _, ok, _ := s.MatchMute(ctx, "backup", "", now); ok {
		t.Fatal("ended mute still matches")
	}
	if _, err := s.GetMute(ctx, "mute_x"); err != ErrNotFound {
		t.Fatalf("unknown mute: %v", err)
	}
}

func TestIcons(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	ic := &Icon{Name: "backup", ContentType: "image/png", Data: []byte("\x89PNG one"), UpdatedBy: "admin"}
	if created, err := s.PutIcon(ctx, ic); !created || err != nil || ic.Size != 8 || len(ic.SHA256) != 64 {
		t.Fatalf("put %v %v %+v", created, err, ic)
	}
	ic.Data = []byte("\x89PNG two!")
	if created, err := s.PutIcon(ctx, ic); created || err != nil {
		t.Fatalf("replace %v %v", created, err)
	}
	got, err := s.GetIcon(ctx, "backup")
	if err != nil || string(got.Data) != "\x89PNG two!" || got.Size != 9 {
		t.Fatalf("get %+v %v", got, err)
	}
	list, err := s.Icons(ctx)
	if err != nil || len(list) != 1 || list[0].Data != nil || list[0].Size != 9 {
		t.Fatalf("list %+v %v", list, err)
	}
	if ok, _ := s.HasIcon(ctx, "backup"); !ok {
		t.Fatal("HasIcon false")
	}
	if err := s.DeleteIcon(ctx, "backup"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteIcon(ctx, "backup"); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}
