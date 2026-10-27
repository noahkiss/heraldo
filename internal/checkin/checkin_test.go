package checkin

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func TestNextPeriod(t *testing.T) {
	s := Schedule{Period: time.Hour}
	if n, ok := s.Next(t0); !ok || !n.Equal(t0.Add(time.Hour)) {
		t.Fatalf("next = %v, %v", n, ok)
	}
	if _, ok := (Schedule{}).Next(t0); ok {
		t.Fatal("an unscheduled check has a next ping")
	}
}

func TestNextCronTZ(t *testing.T) {
	// 03:00 every day in Madrid, which is UTC+1 on 2026-10-27 (after the
	// clocks went back on the 25th).
	s := Schedule{Cron: "0 3 * * *", TZ: "Europe/Madrid"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	n, ok := s.Next(t0)
	want := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	if !ok || !n.Equal(want) {
		t.Fatalf("next = %v, want %v", n, want)
	}
	// Without tz, cron is UTC.
	n, _ = Schedule{Cron: "30 12 * * *"}.Next(t0)
	if !n.Equal(t0.Add(30 * time.Minute)) {
		t.Fatalf("utc next = %v", n)
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Schedule{
		{Period: time.Hour, Cron: "* * * * *"},
		{TZ: "UTC"},
		{Cron: "not cron"},
		{Cron: "* * * * *", TZ: "Nowhere/Town"},
		{Grace: -time.Second},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v: valid", s)
		}
	}
}

func check() Check {
	return New("job", Schedule{Period: time.Hour, Grace: 10 * time.Minute, MaxRuntime: 30 * time.Minute}, t0)
}

func TestApplyRun(t *testing.T) {
	c := check()
	c.Apply(Ping{Action: "start", At: t0, RID: "r1", Duration: -1, EventID: "e1"})
	if c.State != StateStarted || !c.StartedAt.Equal(t0) {
		t.Fatalf("start: %+v", c)
	}
	end := t0.Add(90 * time.Second)
	c.Apply(Ping{Action: "success", At: end, RID: "r1", Duration: -1, EventID: "e2", HasLog: true})
	if c.State != StateUp || c.LastDuration != 90*time.Second || !c.StartedAt.IsZero() {
		t.Fatalf("success: %+v", c)
	}
	if !c.DueAt.Equal(end.Add(time.Hour)) || c.LastLogEvent != "e2" || c.LastEvent != "e2" {
		t.Fatalf("success: %+v", c)
	}
	// An end with another run id measures nothing and leaves the run alone.
	c.Apply(Ping{Action: "start", At: end, RID: "r2", Duration: -1})
	c.Apply(Ping{Action: "success", At: end.Add(time.Minute), RID: "other", Duration: -1})
	if c.StartRID != "r2" || c.LastDuration != 90*time.Second {
		t.Fatalf("other rid: %+v", c)
	}
	// The sender's own measurement wins.
	c.Apply(Ping{Action: "success", At: end.Add(2 * time.Minute), Duration: 5 * time.Second})
	if c.LastDuration != 5*time.Second {
		t.Fatalf("sender duration: %+v", c)
	}
}

func TestApplyExitCode(t *testing.T) {
	c := check()
	two := 2
	c.Apply(Ping{At: t0, ExitCode: &two, Duration: -1})
	if c.State != StateDown || c.DownReason != ReasonFailed {
		t.Fatalf("exit 2: %+v", c)
	}
	// A log changes no state, whatever its exit code.
	c.Apply(Ping{Action: "log", At: t0.Add(time.Minute), Duration: -1, HasLog: true, EventID: "l"})
	if c.State != StateDown || c.LastLogEvent != "l" {
		t.Fatalf("log: %+v", c)
	}
	zero := 0
	c.Apply(Ping{Action: "fail", At: t0.Add(2 * time.Minute), ExitCode: &zero, Duration: -1})
	if c.State != StateUp || c.DownReason != "" {
		t.Fatalf("exit 0: %+v", c)
	}
}

func TestApplyPaused(t *testing.T) {
	c := check()
	c.State = StatePaused
	c.Apply(Ping{Action: "fail", At: t0, Duration: -1, EventID: "e"})
	if c.State != StatePaused || !c.LastPing.Equal(t0) || c.LastEvent != "e" {
		t.Fatalf("paused fail: %+v", c)
	}
}

func TestTick(t *testing.T) {
	c := check()
	if c.Tick(t0.Add(48*time.Hour), t0) {
		t.Fatal("a new check changed with time")
	}
	c.Apply(Ping{At: t0, Duration: -1})
	if c.Tick(t0.Add(59*time.Minute), t0) {
		t.Fatal("changed before due")
	}
	if !c.Tick(t0.Add(time.Hour), t0) || c.State != StateLate {
		t.Fatalf("due: %+v", c)
	}
	if c.Tick(t0.Add(69*time.Minute), t0) {
		t.Fatal("changed in the grace")
	}
	if !c.Tick(t0.Add(70*time.Minute), t0) || c.State != StateDown || c.DownReason != ReasonMissed {
		t.Fatalf("grace out: %+v", c)
	}
	if c.Tick(t0.Add(100*time.Hour), t0) {
		t.Fatal("down changed with time")
	}
}

func TestTickOverran(t *testing.T) {
	c := check()
	c.Apply(Ping{At: t0, Duration: -1})
	c.Apply(Ping{Action: "start", At: t0.Add(5 * time.Minute), Duration: -1})
	if c.Tick(t0.Add(34*time.Minute), t0) {
		t.Fatal("changed inside max runtime")
	}
	if !c.Tick(t0.Add(35*time.Minute), t0) || c.State != StateDown || c.DownReason != ReasonOverran {
		t.Fatalf("overran: %+v", c)
	}
}

// A server that was itself down gives each check its grace from its own
// start before a missed ping counts.
func TestTickNotBefore(t *testing.T) {
	c := check()
	c.Apply(Ping{At: t0, Duration: -1})
	start := t0.Add(5 * time.Hour)
	c.Tick(start, start)
	if c.State != StateLate {
		t.Fatalf("at start: %+v", c)
	}
	if c.Tick(start.Add(9*time.Minute), start) {
		t.Fatal("down inside the start-up grace")
	}
	if !c.Tick(start.Add(10*time.Minute), start) || c.State != StateDown {
		t.Fatalf("after start-up grace: %+v", c)
	}
}
