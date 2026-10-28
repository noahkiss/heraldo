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
	if u := New("k", Schedule{}, t0); u.Tick(t0.Add(48*time.Hour), t0) {
		t.Fatal("an unscheduled new check changed with time")
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

// A scheduled check that never pings goes late at its first slot after
// creation and down when the grace runs out.
func TestTickNeverPinged(t *testing.T) {
	c := check()
	if c.Tick(t0.Add(59*time.Minute), t0) {
		t.Fatal("changed before the first slot")
	}
	if !c.DownAfter().Equal(t0.Add(70 * time.Minute)) {
		t.Fatalf("down after %v", c.DownAfter())
	}
	if !c.Tick(t0.Add(time.Hour), t0) || c.State != StateLate || !c.DueAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("first slot: %+v", c)
	}
	if c.Tick(t0.Add(69*time.Minute), t0) {
		t.Fatal("changed in the grace")
	}
	if !c.Tick(t0.Add(70*time.Minute), t0) || c.State != StateDown || c.DownReason != ReasonMissed {
		t.Fatalf("grace out: %+v", c)
	}

	// One tick long after the slot goes straight to down.
	c = check()
	if !c.Tick(t0.Add(5*time.Hour), t0) || c.State != StateDown {
		t.Fatalf("late tick: %+v", c)
	}

	// A cron check is due at its first fire after creation.
	c = New("nightly", Schedule{Cron: "0 2 * * *", Grace: time.Hour}, t0)
	if c.Tick(t0.Add(13*time.Hour+59*time.Minute), t0) {
		t.Fatal("cron: changed before 02:00 next day")
	}
	if !c.Tick(t0.Add(14*time.Hour+59*time.Minute), t0) || c.State != StateLate {
		t.Fatalf("cron at 02:59: %+v", c)
	}
	if !c.Tick(t0.Add(15*time.Hour), t0) || c.State != StateDown {
		t.Fatalf("cron: %+v", c)
	}

	// Server start-up grace still applies: a server that starts after the
	// first slot gives the grace from its own start.
	c = check()
	start := t0.Add(3 * time.Hour)
	if !c.Tick(start, start) || c.State != StateLate {
		t.Fatalf("at start: %+v", c)
	}
	if c.Tick(start.Add(9*time.Minute), start) {
		t.Fatal("down inside the start-up grace")
	}
	if !c.Tick(start.Add(10*time.Minute), start) || c.State != StateDown {
		t.Fatalf("after start-up grace: %+v", c)
	}

	// A resumed check counts from its resume.
	c = check()
	c.StateSince = t0.Add(10 * time.Hour)
	if c.Tick(t0.Add(10*time.Hour+59*time.Minute), t0) {
		t.Fatal("resumed: judged from creation")
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

// A run that started is judged on its own start, not on the due time of the
// run before: a 2 h nightly job is not missed while it runs.
func TestStartedRunNotMissed(t *testing.T) {
	s := Schedule{Cron: "0 2 * * *", Grace: time.Hour}
	d1 := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	c := New("backup", s, d1.Add(-time.Hour))
	c.Apply(Ping{Action: "start", At: d1, Duration: -1})
	c.Apply(Ping{Action: "success", At: d1.Add(2 * time.Hour), Duration: -1})
	d2 := d1.Add(24 * time.Hour)
	c.Apply(Ping{Action: "start", At: d2, Duration: -1})
	if c.Tick(d2.Add(time.Hour+time.Second), time.Time{}) || c.State != StateStarted {
		t.Fatalf("while running: %+v", c)
	}
	// Without max_runtime, it is missed only when the next slot after its
	// start passes, plus the grace.
	if want := d2.Add(25 * time.Hour); !c.DownAfter().Equal(want) {
		t.Fatalf("down after %v, want %v", c.DownAfter(), want)
	}
	if c.Tick(d2.Add(25*time.Hour-time.Second), time.Time{}) {
		t.Fatal("down before the next slot plus grace")
	}
	if !c.Tick(d2.Add(25*time.Hour), time.Time{}) || c.State != StateDown || c.DownReason != ReasonMissed {
		t.Fatalf("next slot passed: %+v", c)
	}
}

// With max_runtime, a started run goes down only as overran, even past its
// due time.
func TestStartedOnlyOverran(t *testing.T) {
	c := New("job", Schedule{Period: time.Hour, Grace: time.Minute, MaxRuntime: 3 * time.Hour}, t0)
	c.Apply(Ping{At: t0, Duration: -1})
	c.Apply(Ping{Action: "start", At: t0.Add(55 * time.Minute), Duration: -1})
	if c.Tick(t0.Add(3*time.Hour), t0) || c.State != StateStarted {
		t.Fatalf("past due while running: %+v", c)
	}
	if !c.DownAfter().Equal(t0.Add(235 * time.Minute)) {
		t.Fatalf("down after %v", c.DownAfter())
	}
	if !c.Tick(t0.Add(235*time.Minute), t0) || c.DownReason != ReasonOverran {
		t.Fatalf("overran: %+v", c)
	}
}

// A /start on a down check leaves it down with its state_since; the next
// success brings it up.
func TestStartOnDownKeepsDown(t *testing.T) {
	c := check()
	c.Apply(Ping{Action: "fail", At: t0, Duration: -1})
	c.Apply(Ping{Action: "start", At: t0.Add(time.Hour), Duration: -1})
	if c.State != StateDown || !c.StateSince.Equal(t0) || !c.StartedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("start on down: %+v", c)
	}
	if c.Tick(t0.Add(10*time.Hour), t0) {
		t.Fatal("tick changed a down check")
	}
	c.Apply(Ping{Action: "success", At: t0.Add(time.Hour + time.Minute), Duration: -1})
	if c.State != StateUp || c.LastDuration != time.Minute {
		t.Fatalf("success: %+v", c)
	}
}

// In the fall-back hour, a ping at the first 01:30 is due at 01:30 the
// next day, not at the second 01:30 an hour later.
func TestCronFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	s := Schedule{Cron: "30 1 * * *", TZ: "America/New_York"}
	first := time.Date(2026, 11, 1, 1, 31, 0, 0, ny) // EDT, the first pass
	n, _ := s.Next(first)
	if want := time.Date(2026, 11, 2, 1, 30, 0, 0, ny); !n.Equal(want) {
		t.Fatalf("next %v, want %v", n, want)
	}
	// A ping before the slot is due at its first reading.
	n, _ = s.Next(time.Date(2026, 11, 1, 0, 50, 0, 0, ny))
	if want := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC); !n.Equal(want) {
		t.Fatalf("before the slot: %v, want %v (01:30 EDT)", n, want)
	}
	// A slot outside the repeated hour is untouched on that day.
	n, _ = Schedule{Cron: "30 3 * * *", TZ: "America/New_York"}.Next(first)
	if want := time.Date(2026, 11, 1, 3, 30, 0, 0, ny); !n.Equal(want) {
		t.Fatalf("03:30: %v, want %v", n, want)
	}
	// Spring forward: the 02:30 slot does not exist; the next one is the
	// day after.
	n, _ = Schedule{Cron: "30 2 * * *", TZ: "America/New_York"}.Next(time.Date(2026, 3, 8, 1, 0, 0, 0, ny))
	if n.Before(time.Date(2026, 3, 8, 3, 0, 0, 0, ny)) {
		t.Fatalf("spring: %v", n)
	}
}
