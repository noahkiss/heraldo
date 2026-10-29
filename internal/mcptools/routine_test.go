package mcptools

import "testing"

func TestSearchRoutine(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"s","kind":"message","title":"hello"}`)
	f.post(`{"source":"cron","kind":"checkin","key":"job","action":"start"}`)
	f.post(`{"source":"cron","kind":"checkin","key":"job","action":"success"}`)
	f.post(`{"source":"cron","kind":"checkin","key":"job","action":"fail"}`)
	s := f.connect(false)
	for _, c := range []struct {
		args map[string]any
		n    int
	}{
		{map[string]any{}, 2},
		{map[string]any{"routine": "include"}, 4},
		{map[string]any{"routine": "only"}, 2},
		{map[string]any{"key": "job"}, 3},
		{map[string]any{"key": "job", "routine": "exclude"}, 1},
		{map[string]any{"kind": "checkin"}, 3},
	} {
		out, _ := s.ok("events_search", c.args)
		if n := len(out["events"].([]any)); n != c.n {
			t.Errorf("%v: %d events, want %d", c.args, n, c.n)
		}
	}
	// The cursor keeps the routine filter.
	out, _ := s.ok("events_search", map[string]any{"limit": 1})
	out, _ = s.ok("events_search", map[string]any{"cursor": out["next_cursor"]})
	if ev := out["events"].([]any); len(ev) != 1 || ev[0].(map[string]any)["title"] != "hello" {
		t.Fatalf("page 2: %v", out)
	}
}
