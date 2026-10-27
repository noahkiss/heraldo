package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// typed builds an event from its fields. Typed comes from the same JSON, so
// the helper does not depend on how the generated types spell optional fields.
func typed(t *testing.T, id string, at time.Time, kind api.Kind, key, action string) NewEvent {
	t.Helper()
	f := map[string]any{"id": id, "source": "s", "kind": string(kind), "key": key}
	if action != "" {
		f["action"] = action
	}
	raw, _ := json.Marshal(f)
	var in api.EventInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return NewEvent{ID: id, ReceivedAt: at, Fields: f, Typed: in}
}

func ids(t *testing.T, raws []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, r := range raws {
		var e struct{ ID string }
		if err := json.Unmarshal(r, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e.ID)
	}
	return out
}

func TestState(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, ev := range []NewEvent{
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA1", at, api.KindIncident, "disk:a", "open"),
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA2", at, api.KindIncident, "disk:b", "open"),
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA3", at, api.KindIncident, "disk:b", "resolve"),
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA4", at, api.KindIncident, "disk:a", "ack"),
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA5", at, api.KindCheckin, "nightly", "success"),
		typed(t, "01K3ZX0000AAAAAAAAAAAAAAA6", at, api.KindProgress, "copy", "start"),
	} {
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, st.Incidents); len(got) != 1 || got[0] != "01K3ZX0000AAAAAAAAAAAAAAA4" {
		t.Fatalf("incidents = %v", got)
	}
	if got := ids(t, st.Checkins); len(got) != 1 || got[0] != "01K3ZX0000AAAAAAAAAAAAAAA5" {
		t.Fatalf("checkins = %v", got)
	}
	if got := ids(t, st.Progress); len(got) != 1 {
		t.Fatalf("progress = %v", got)
	}
	if st.Statuses == nil || len(st.Statuses) != 0 {
		t.Fatalf("statuses = %v; want an empty list", st.Statuses)
	}
}

func TestCursorsMoveForward(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	if _, found, err := s.Cursor(ctx, "inbox"); err != nil || found {
		t.Fatalf("found = %v, %v before any set", found, err)
	}
	// Two clients share the cursor; neither moves it back.
	for _, step := range []struct {
		by        string
		set, want int64
	}{{"cli", 5, 5}, {"phone", 3, 5}, {"phone", 9, 9}, {"cli", 7, 9}} {
		got, err := s.SetCursor(ctx, "inbox", step.set, step.by)
		if err != nil || got != step.want {
			t.Fatalf("%s set %d = %d, %v; want %d", step.by, step.set, got, err, step.want)
		}
	}
	if row := s.db.QueryRowContext(ctx, `SELECT updated_by FROM cursors WHERE name = 'inbox'`); row != nil {
		var by string
		if err := row.Scan(&by); err != nil || by != "phone" {
			t.Fatalf("updated_by = %q, %v; want the token that last moved it", by, err)
		}
	}
	if _, found, _ := s.Cursor(ctx, "scratch"); found {
		t.Fatal("another name shares the cursor")
	}
}

func TestListDescBeforeHold(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"01K3ZX0000AAAAAAAAAAAAAAA1", "01K3ZX0000AAAAAAAAAAAAAAA2", "01K3ZX0000AAAAAAAAAAAAAAA3"} {
		ev := event(id, at)
		if i == 1 {
			ev.Fields["hold"] = map[string]any{"state": "held", "id": "h-1"}
		}
		if _, _, err := s.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.List(ctx, Filter{Desc: true, Limit: 2})
	if err != nil || len(recs) != 2 || recs[0].Seq != 3 || recs[1].Seq != 2 {
		t.Fatalf("desc = %+v, %v", recs, err)
	}
	recs, err = s.List(ctx, Filter{Desc: true, BeforeSeq: 2})
	if err != nil || len(recs) != 1 || recs[0].Seq != 1 {
		t.Fatalf("before = %+v, %v", recs, err)
	}
	recs, err = s.List(ctx, Filter{Hold: "held"})
	if err != nil || len(recs) != 1 || recs[0].Seq != 2 {
		t.Fatalf("hold = %+v, %v", recs, err)
	}
	if head, err := s.Head(ctx); err != nil || head != 3 {
		t.Fatalf("head = %d, %v", head, err)
	}
}
