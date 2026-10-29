package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/noahkiss/heraldo/internal/checkin"
)

// TestBadgeAttention: a down check counts once, with or without its open
// incident; an acked incident does not count; projects narrow the count.
func TestBadgeAttention(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	for _, key := range []string{"backup", "sync"} {
		if _, err := s.PutCheck(ctx, key, "", checkin.Schedule{}); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE checkins SET state = 'down', project = 'home'`)
	exec(`INSERT INTO incidents (key, state, opened_at, last_event, project) VALUES
		('checkin:backup', 'open', '2026-10-29T00:00:00Z', 'e1', 'home'),
		('web', 'open', '2026-10-29T00:00:00Z', 'e2', 'infra'),
		('db', 'acked', '2026-10-29T00:00:00Z', 'e3', 'infra')`)
	for _, c := range []struct {
		scope    string
		projects []string
		want     int
	}{
		{BadgeAttention, nil, 3},              // checkin:backup, sync, web
		{BadgeAttention, []string{"home"}, 2}, // checkin:backup, sync
		{BadgeAttention, []string{"infra"}, 1},
		{BadgePushed, nil, 0},
		{BadgeNone, nil, 0},
	} {
		if n, err := s.Badge(ctx, c.scope, c.projects, 0); err != nil || n != c.want {
			t.Errorf("Badge(%s, %v) = %d, %v; want %d", c.scope, c.projects, n, err, c.want)
		}
	}
	// No cursor: attention still counts incidents and checks.
	if n, err := s.BadgeFor(ctx, "nobody", BadgeAttention, nil); err != nil || n != 3 {
		t.Fatalf("BadgeFor without a cursor = %d, %v; want 3", n, err)
	}
}
