package push

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

func (r *rig) remind() {
	r.p.Remind(context.Background())
	r.p.Wait()
}

func (r *rig) snooze(t *testing.T, key, dur string) api.Mute {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"key":"`+key+`","for":"`+dur+`","remind":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("snooze: %d %s", rec.Code, rec.Body)
	}
	var m api.Mute
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Remind == nil || !*m.Remind {
		t.Fatalf("snooze %s: remind not set", rec.Body)
	}
	return m
}

// A snooze holds the key back like a mute; when it ends and the key is
// still open, the key's last event pushes again at its level, once, and the
// trace says why.
func TestSnoozeReminds(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	id := r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"open","level":"time-sensitive","title":"down","body":"no answer"}`)
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes, want 1", n)
	}
	m := r.snooze(t, "host:web-1", "1h")
	upd := r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"update","level":"time-sensitive","title":"still down"}`)
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes while snoozed, want 1", n)
	}
	r.remind()
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes before the snooze ends, want 1", n)
	}
	r.clock.add(61 * time.Minute)
	r.remind()
	r.remind()
	q := r.prod.requests()
	if len(q) != 2 {
		t.Fatalf("%d pushes after the snooze, want 2", len(q))
	}
	p := payloadOf(t, q[1])
	if p.Heraldo.ID != upd || p.APS.InterruptionLevel != "time-sensitive" || !strings.HasPrefix(p.APS.Alert.Body, "Still open after 1 h 1 min") {
		t.Fatalf("reminder payload %s", q[1].Body)
	}
	tr := r.why(t, upd)
	var snooze *api.PushDecision
	for i, d := range tr.Decisions {
		if d.Stage == "snooze" {
			snooze = &tr.Decisions[i]
		}
	}
	if snooze == nil || !strings.Contains(snooze.Reason, m.Id) || !strings.HasPrefix(summary(tr), "re-alerted") {
		t.Fatalf("trace %s, %+v", stagesOf(tr), tr.Decisions)
	}
	if got := stagesOf(r.why(t, id)); !strings.Contains(got, "snooze:pass") {
		t.Fatalf("open trace %s", got)
	}
	// The snooze shows reminded_at.
	rec := r.do(t, http.MethodGet, "/v1/mutes", writeTok, "")
	if strings.Contains(rec.Body.String(), m.Id) {
		t.Fatalf("an ended snooze is still listed: %s", rec.Body)
	}
}

// A snooze whose key resolved, or was acked, reminds of nothing; ending a
// snooze early reminds at once.
func TestSnoozeQuietWhenClosed(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"a","action":"open","title":"a"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"b","action":"open","title":"b"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"c","action":"open","title":"c"}`)
	r.snooze(t, "a", "30m")
	r.snooze(t, "b", "30m")
	c := r.snooze(t, "c", "30m")
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"a","action":"resolve"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"b","action":"ack"}`)
	base := len(r.prod.requests())
	if rec := r.do(t, http.MethodDelete, "/v1/mutes/"+c.Id, writeTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("end: %d", rec.Code)
	}
	r.remind()
	q := r.prod.requests()
	if len(q) != base+1 || payloadOf(t, q[len(q)-1]).Heraldo.Key != "c" {
		t.Fatalf("%d pushes after ending c early, want %d", len(q), base+1)
	}
	r.clock.add(time.Hour)
	r.remind()
	if n := len(r.prod.requests()); n != base+1 {
		t.Fatalf("%d pushes, want %d: a resolved and b acked", n, base+1)
	}
}

func TestSnoozeNeedsKey(t *testing.T) {
	r := newRig(t, 0)
	rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"source":"s","for":"1h","remind":true}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "remind needs key") {
		t.Fatalf("snooze without key: %d %s", rec.Code, rec.Body)
	}
}

func flapOn(r *rig) {
	r.st.SetFlapPolicy(func() store.FlapPolicy {
		return store.FlapPolicy{Enabled: true, Opens: 3, Window: time.Hour, Settle: 30 * time.Minute}
	})
}

// A key that opens three times in an hour flaps: the third open pages and
// says so, later opens and resolves go out passive under the same collapse
// id, re-alerts stop, and after 30 quiet minutes the next open pages again.
func TestFlapGrouping(t *testing.T) {
	r := newRig(t, 0)
	flapOn(r)
	r.register(t, prodToken, "production")
	open := `{"source":"uptime","kind":"incident","key":"host:web-1","action":"open","level":"time-sensitive","title":"down"}`
	resolve := `{"source":"uptime","kind":"incident","key":"host:web-1","action":"resolve"}`
	for i := 0; i < 2; i++ {
		r.post(t, writeTok, open)
		r.clock.add(2 * time.Minute)
		r.post(t, writeTok, resolve)
		r.clock.add(3 * time.Minute)
	}
	q := r.prod.requests()
	if len(q) != 4 {
		t.Fatalf("%d pushes before flapping, want 4", len(q))
	}
	third := r.post(t, writeTok, open)
	q = r.prod.requests()
	p := payloadOf(t, q[4])
	if len(q) != 5 || p.APS.InterruptionLevel != "time-sensitive" || !strings.HasPrefix(p.APS.Alert.Body, "Flapping: 3 opens in 10 min") {
		t.Fatalf("third open: %d pushes, %s", len(q), q[len(q)-1].Body)
	}
	if got := stagesOf(r.why(t, third)); !strings.Contains(got, "flap:pass") {
		t.Fatalf("third trace %s", got)
	}
	r.clock.add(time.Minute)
	r.post(t, writeTok, resolve)
	r.clock.add(time.Minute)
	fourth := r.post(t, writeTok, open)
	q = r.prod.requests()
	if len(q) != 7 {
		t.Fatalf("%d pushes, want 7", len(q))
	}
	for _, x := range q[5:] {
		if pl := payloadOf(t, x); pl.APS.InterruptionLevel != "passive" || pl.APS.Sound != "" || x.CollapseID != q[0].CollapseID {
			t.Fatalf("a flapping push pages: %s", x.Body)
		}
	}
	if pl := payloadOf(t, q[6]); !strings.HasPrefix(pl.APS.Alert.Body, "Flapping: 4 opens") {
		t.Fatalf("fourth open text %q", pl.APS.Alert.Body)
	}
	tr := r.why(t, fourth)
	found := false
	for _, d := range tr.Decisions {
		if d.Stage == "flap" && d.Outcome == api.PushDecisionOutcomeDowngrade {
			found = true
		}
	}
	if !found {
		t.Fatalf("fourth trace %+v", tr.Decisions)
	}
	// /v1/state shows it flapping.
	if st := r.state(t); len(st.Incidents) != 1 || st.Incidents[0].Incident.Flapping == nil || !*st.Incidents[0].Incident.Flapping {
		t.Fatalf("state: %+v", st.Incidents)
	}
	// Quiet for the settle time: state stops showing it, and the next open pages.
	r.clock.add(31 * time.Minute)
	if st := r.state(t); st.Incidents[0].Incident.Flapping != nil {
		t.Fatalf("still flapping after settle: %+v", st.Incidents[0].Incident)
	}
	r.post(t, writeTok, resolve)
	r.clock.add(time.Minute)
	r.post(t, writeTok, open)
	q = r.prod.requests()
	if pl := payloadOf(t, q[len(q)-1]); pl.APS.InterruptionLevel != "time-sensitive" || strings.HasPrefix(pl.APS.Alert.Body, "Flapping") {
		t.Fatalf("open after settle: %s", q[len(q)-1].Body)
	}
}

// Off by default: the same opens page every time.
func TestFlapOffByDefault(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	for i := 0; i < 5; i++ {
		r.post(t, writeTok, `{"source":"s","kind":"incident","key":"k","action":"open","title":"down"}`)
		r.post(t, writeTok, `{"source":"s","kind":"incident","key":"k","action":"resolve"}`)
	}
	for _, x := range r.prod.requests() {
		var p Payload
		_ = json.Unmarshal(x.Body, &p)
		if strings.HasPrefix(p.APS.Alert.Body, "Flapping") {
			t.Fatalf("flapping with flap off: %s", x.Body)
		}
	}
}

func (r *rig) state(t *testing.T) api.State {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/v1/state", readTok, "")
	var st api.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}
