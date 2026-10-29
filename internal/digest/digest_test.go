package digest

import (
	"testing"
	"time"
)

func TestDue(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	s := Schedule{At: "07:30", Loc: ny}
	// 03:00 UTC on 29 Oct is 23:00 on 28 Oct in New York.
	day, due := s.Due(time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC))
	if day != "2026-10-28" || !due.Equal(time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("day %s due %s", day, due.UTC())
	}
	// A bad clock falls back to 07:30.
	s.At = "nope"
	if _, due := s.Due(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)); due.Hour() != 7 || due.Minute() != 30 {
		t.Fatalf("fallback due %s", due)
	}
}

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		last  time.Time
		after time.Duration
		want  bool
	}{
		{now.Add(-73 * time.Hour), 72 * time.Hour, true},
		{now.Add(-72 * time.Hour), 72 * time.Hour, true},
		{now.Add(-71 * time.Hour), 72 * time.Hour, false},
		{time.Time{}, 72 * time.Hour, false},
		{now.Add(-100 * time.Hour), 0, false},
	} {
		if got := IsStale(c.last, now, c.after); got != c.want {
			t.Errorf("IsStale(%s, %s) = %v", c.last, c.after, got)
		}
	}
}

func TestSummaryLine(t *testing.T) {
	d := &Digest{
		Incidents: Incidents{Open: []Incident{{State: "open"}}, Stale: []Incident{{State: "acked"}}},
		Checkins:  Checks{Total: 3, OK: 3},
		Finished:  []Job{{}},
	}
	if got, want := d.summaryLine(), "2 incidents open, 1 stale, checks 3/3 ok, 1 finished"; got != want {
		t.Fatalf("summary %q, want %q", got, want)
	}
	if !d.attention() {
		t.Fatal("an open incident needs attention")
	}
	d.Incidents.Open = nil
	if d.attention() {
		t.Fatal("an acked stale incident and green checks need no attention")
	}
}
