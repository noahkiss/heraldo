package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/noahkiss/heraldo/internal/checkin"
)

// A database at version 3 keeps its check-ins: each becomes an unscheduled
// check, running becomes started and unknown becomes new.
func TestMigrateCheckinsFrom3(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_cursors.sql", "0003_devices.sql"} {
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 3;
		INSERT INTO checkins (key, last_ping, state, last_event) VALUES
		  ('a', '2026-10-27T10:00:00.000000000Z', 'up', 'e1'),
		  ('b', '2026-10-27T11:00:00.000000000Z', 'running', 'e2'),
		  ('c', '2026-10-27T12:00:00.000000000Z', 'down', 'e3'),
		  ('d', '2026-10-27T13:00:00.000000000Z', 'unknown', 'e4')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s := open(t, path)
	defer s.Close()
	want := map[string]string{"a": checkin.StateUp, "b": checkin.StateStarted, "c": checkin.StateDown, "d": checkin.StateNew}
	for key, state := range want {
		c, found, err := s.LoadCheck(ctx, key)
		if err != nil || !found {
			t.Fatalf("%s: %v %v", key, found, err)
		}
		if c.State != state || c.Schedule.Scheduled() || c.LastPing.IsZero() {
			t.Errorf("%s: %+v", key, c)
		}
	}
	a, _, _ := s.LoadCheck(ctx, "a")
	c, _, _ := s.LoadCheck(ctx, "c")
	if a.LastSuccess.IsZero() || c.DownReason != checkin.ReasonFailed || a.Schedule.Grace != checkin.DefaultGrace {
		t.Fatalf("a %+v, c %+v", a, c)
	}
}
