package routine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/routine"
)

// A routine event never pushes, and a check-in that is not routine does:
// the routine rule is the push table's, by kind and action.
func TestRoutineMatchesPushTable(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ev      string
		routine bool
	}{
		{`{"kind":"message"}`, false},
		{`{"kind":"digest"}`, false},
		{`{"kind":"incident","key":"k","action":"open"}`, false},
		{`{"kind":"checkin","key":"k","action":"start"}`, true},
		{`{"kind":"checkin","key":"k","action":"success"}`, true},
		{`{"kind":"checkin","key":"k","action":"success","checkin":{"exit_code":0}}`, true},
		{`{"kind":"checkin","key":"k","action":"success","checkin":{"exit_code":3}}`, false},
		{`{"kind":"checkin","key":"k","action":"log","checkin":{"exit_code":3}}`, true},
		{`{"kind":"checkin","key":"k","action":"fail"}`, false},
		{`{"kind":"progress","key":"k","action":"update"}`, true},
		{`{"kind":"status","key":"k","action":"set"}`, true},
	} {
		var ev api.Event
		if err := json.Unmarshal([]byte(`{"id":"x","source":"s",`+c.ev[1:]), &ev); err != nil {
			t.Fatal(err)
		}
		if got := routine.Is(&ev); got != c.routine {
			t.Errorf("%s: routine = %v, want %v", c.ev, got, c.routine)
		}
		d := push.Decide(&ev, now)
		if c.routine && d.Push {
			t.Errorf("%s: routine but pushes", c.ev)
		}
		if ev.Kind == api.KindCheckin && !c.routine && !d.Push {
			t.Errorf("%s: a check-in that is not routine does not push", c.ev)
		}
	}
}
