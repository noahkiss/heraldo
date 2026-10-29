package push

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInQuiet(t *testing.T) {
	at := func(clock string) time.Time {
		tm, _ := time.Parse("2006-01-02 15:04", "2026-10-29 "+clock)
		return tm
	}
	for _, c := range []struct {
		now, start, end, tz string
		want                bool
	}{
		{"23:00", "22:00", "07:00", "UTC", true},
		{"03:00", "22:00", "07:00", "UTC", true},
		{"07:00", "22:00", "07:00", "UTC", false},
		{"12:00", "22:00", "07:00", "UTC", false},
		{"22:00", "22:00", "07:00", "UTC", true},
		{"13:00", "12:00", "14:00", "UTC", true},
		{"14:00", "12:00", "14:00", "UTC", false},
		{"12:00", "12:00", "12:00", "UTC", false},
		// 23:00 UTC is 01:00 in Athens (UTC+2 on this date).
		{"23:00", "00:30", "02:00", "Europe/Athens", true},
		{"23:00", "22:00", "23:30", "Europe/Athens", false},
	} {
		got, err := inQuiet(at(c.now), c.start, c.end, c.tz)
		if err != nil || got != c.want {
			t.Errorf("inQuiet(%s, %s-%s %s) = %v, %v; want %v", c.now, c.start, c.end, c.tz, got, err, c.want)
		}
	}
	if _, err := inQuiet(time.Now(), "22:00", "07:00", "Nowhere/Else"); err == nil {
		t.Fatal("bad zone: no error")
	}
}

// quietNow turns quiet hours on, in a window around the rig's clock.
func (r *rig) quietNow(t *testing.T, floor string) {
	t.Helper()
	now := r.clock.now().UTC()
	r.set(t, "quiet.enabled", "true")
	r.set(t, "quiet.start", fmt.Sprintf("%q", now.Add(-time.Hour).Format("15:04")))
	r.set(t, "quiet.end", fmt.Sprintf("%q", now.Add(time.Hour).Format("15:04")))
	r.set(t, "quiet.tz", `"UTC"`)
	if floor != "" {
		r.set(t, "quiet.floor", fmt.Sprintf("%q", floor))
	}
}

func TestQuietHours(t *testing.T) {
	r := newRig(t, 0)
	dev := r.register(t, prodToken, "production")

	// Off by default: active pushes active.
	id := r.post(t, writeTok, `{"source":"s","title":"one"}`)
	if p := payloadOf(t, r.prod.requests()[0]); p.APS.InterruptionLevel != "active" {
		t.Fatalf("off: level %s", p.APS.InterruptionLevel)
	}
	if d := decision(t, r.why(t, id), "quiet"); d.Reason != "quiet hours off" {
		t.Fatalf("off: quiet row %+v", d)
	}

	r.quietNow(t, "")
	// Below the floor (time-sensitive): active goes out passive, silent.
	id = r.post(t, writeTok, `{"source":"s","title":"two"}`)
	q := r.prod.requests()
	if p := payloadOf(t, q[1]); p.APS.InterruptionLevel != "passive" || p.APS.Sound != "" || q[1].Priority != "5" {
		t.Fatalf("quiet: payload %s priority %s", q[1].Body, q[1].Priority)
	}
	tr := r.why(t, id)
	if d := decision(t, tr, "quiet"); d.Outcome != "downgrade" || d.Reason != "quiet hours: active sent as passive" {
		t.Fatalf("quiet row %+v", d)
	}
	if ds := r.delivery(t, id); len(ds) != 1 || ds[0].Device != dev || ds[0].Level == nil || *ds[0].Level != "passive" {
		t.Fatalf("delivery %+v", ds)
	}

	// At the floor: time-sensitive goes out as sent.
	r.post(t, writeTok, `{"source":"s","title":"three","level":"time-sensitive"}`)
	if p := payloadOf(t, r.prod.requests()[2]); p.APS.InterruptionLevel != "time-sensitive" {
		t.Fatalf("at floor: level %s", p.APS.InterruptionLevel)
	}

	// Floor critical: time-sensitive is lowered, critical never is.
	r.set(t, "quiet.floor", `"critical"`)
	r.post(t, writeTok, `{"source":"s","title":"four","level":"time-sensitive"}`)
	if p := payloadOf(t, r.prod.requests()[3]); p.APS.InterruptionLevel != "passive" {
		t.Fatalf("floor critical: level %s", p.APS.InterruptionLevel)
	}
	id = r.post(t, writeTok, `{"source":"s","title":"five","level":"critical"}`)
	if p := payloadOf(t, r.prod.requests()[4]); p.APS.InterruptionLevel != "time-sensitive" {
		t.Fatalf("critical: level %s", p.APS.InterruptionLevel)
	}
	if d := decision(t, r.why(t, id), "quiet"); d.Outcome != "pass" || !strings.Contains(d.Reason, "critical is never lowered") {
		t.Fatalf("critical quiet row %+v", d)
	}

	// Outside the window: nothing changes.
	r.clock.add(3 * time.Hour)
	r.post(t, writeTok, `{"source":"s","title":"six"}`)
	if p := payloadOf(t, r.prod.requests()[5]); p.APS.InterruptionLevel != "active" {
		t.Fatalf("outside: level %s", p.APS.InterruptionLevel)
	}
}
