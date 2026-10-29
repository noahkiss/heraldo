package push

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

// decision is the trace row of stage.
func decision(t *testing.T, tr api.PushTrace, stage string) api.PushDecision {
	t.Helper()
	for _, d := range tr.Decisions {
		if d.Stage == stage {
			return d
		}
	}
	t.Fatalf("no %s row in %s", stage, stagesOf(tr))
	return api.PushDecision{}
}

// focus sets the Focus filter of the device id with token on env, as the
// app's SetFocusFilterIntent does; filter is the focus_filter JSON.
func (r *rig) focus(t *testing.T, id, token, env, filter string) api.Device {
	t.Helper()
	rec := r.do(t, http.MethodPut, "/v1/devices/"+id, readTok,
		fmt.Sprintf(`{"token":%q,"environment":%q,"topic":%q,"name":"Test phone","focus_filter":%s}`, token, env, topic, filter))
	if rec.Code != http.StatusOK {
		t.Fatalf("focus: %d %s", rec.Code, rec.Body)
	}
	var d api.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	return d
}

func levelOf(t *testing.T, q fakeReq) string {
	t.Helper()
	return payloadOf(t, q).APS.InterruptionLevel
}

func TestFocusFilter(t *testing.T) {
	r := newRig(t, 0)
	phone := r.register(t, prodToken, "production")
	mac := r.register(t, sandboxToken, "sandbox")

	// Off by default: nothing is lowered.
	id := r.post(t, writeTok, `{"source":"s","title":"one","level":"time-sensitive"}`)
	if d := decision(t, r.why(t, id), "focus"); d.Reason != "no Focus filter" {
		t.Fatalf("no filter: %+v", d)
	}

	// Sleep with critical only: on the phone, time-sensitive goes out active.
	d := r.focus(t, phone, prodToken, "production", `{"active":true,"name":"Sleep","break_through":"critical"}`)
	if d.FocusFilter == nil || !d.FocusFilter.Active || d.FocusFilter.BreakThrough == nil ||
		*d.FocusFilter.BreakThrough != "critical" || d.FocusFilter.UpdatedAt == nil {
		t.Fatalf("device %+v", d.FocusFilter)
	}
	id = r.post(t, writeTok, `{"source":"s","title":"two","level":"time-sensitive"}`)
	if got := levelOf(t, r.prod.requests()[1]); got != "active" {
		t.Fatalf("phone: %s, want active", got)
	}
	if got := levelOf(t, r.sand.requests()[1]); got != "time-sensitive" {
		t.Fatalf("mac: %s, want time-sensitive", got)
	}
	tr := r.why(t, id)
	if row := decision(t, tr, "focus"); row.Reason != "focus: time-sensitive sent as active on 1 device" {
		t.Fatalf("focus row %+v", row)
	}
	for _, del := range r.delivery(t, id) {
		switch del.Device {
		case phone:
			if *del.Level != "active" || del.Error == nil || *del.Error != "focus: time-sensitive sent as active" {
				t.Fatalf("phone delivery %+v", del)
			}
		case mac:
			if *del.Level != "time-sensitive" || del.Error != nil {
				t.Fatalf("mac delivery %+v", del)
			}
		}
	}

	// Critical is never lowered.
	id = r.post(t, writeTok, `{"source":"s","title":"three","level":"critical"}`)
	if got := levelOf(t, r.prod.requests()[2]); got != "time-sensitive" {
		t.Fatalf("critical on phone: %s", got)
	}
	if row := decision(t, r.why(t, id), "focus"); !strings.HasPrefix(row.Reason, "critical: never lowered") {
		t.Fatalf("critical focus row %+v", row)
	}

	// A registration without focus_filter keeps it.
	if rec := r.do(t, http.MethodPost, "/v1/devices", readTok,
		fmt.Sprintf(`{"token":%q,"environment":"production","topic":%q}`, prodToken, topic)); rec.Code != http.StatusOK {
		t.Fatalf("re-register: %d %s", rec.Code, rec.Body)
	}
	r.post(t, writeTok, `{"source":"s","title":"four","level":"time-sensitive"}`)
	if got := levelOf(t, r.prod.requests()[3]); got != "active" {
		t.Fatalf("after re-register: %s, want active", got)
	}

	// Projects: only listed projects break through.
	r.focus(t, phone, prodToken, "production", `{"active":true,"projects":["infra"]}`)
	r.post(t, writeTok, `{"source":"s","title":"five","level":"time-sensitive","project":"infra"}`)
	r.post(t, writeTok, `{"source":"s","title":"six","level":"time-sensitive","project":"home"}`)
	q := r.prod.requests()
	if a, b := levelOf(t, q[4]), levelOf(t, q[5]); a != "time-sensitive" || b != "active" {
		t.Fatalf("projects: infra %s, home %s", a, b)
	}

	// Inactive clears it.
	d = r.focus(t, phone, prodToken, "production", `{"active":false}`)
	if d.FocusFilter != nil {
		t.Fatalf("cleared: %+v", d.FocusFilter)
	}
	r.post(t, writeTok, `{"source":"s","title":"seven","level":"time-sensitive","project":"home"}`)
	if got := levelOf(t, r.prod.requests()[6]); got != "time-sensitive" {
		t.Fatalf("cleared: %s", got)
	}

	// A bad filter is refused.
	rec := r.do(t, http.MethodPut, "/v1/devices/"+phone, readTok,
		fmt.Sprintf(`{"token":%q,"environment":"production","topic":%q,"focus_filter":{"active":true,"break_through":"loud"}}`, prodToken, topic))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: %d %s", rec.Code, rec.Body)
	}
}

// TestFocusAndQuiet: quiet hours lower first, for every device; the Focus
// filter then works from that level.
func TestFocusAndQuiet(t *testing.T) {
	r := newRig(t, 0)
	phone := r.register(t, prodToken, "production")
	r.focus(t, phone, prodToken, "production", `{"active":true,"break_through":"critical"}`)
	r.quietNow(t, "")
	id := r.post(t, writeTok, `{"source":"s","title":"one"}`)
	if got := levelOf(t, r.prod.requests()[0]); got != "passive" {
		t.Fatalf("level %s, want passive", got)
	}
	if got := stagesOf(r.why(t, id)); !strings.Contains(got, "quiet:downgrade focus:pass deliver:pass") {
		t.Fatalf("stages %s", got)
	}
}

func TestBadgeScope(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	badge := func(i int) int {
		t.Helper()
		p := payloadOf(t, r.prod.requests()[i])
		if p.APS.Badge == nil {
			t.Fatalf("push %d has no badge", i)
		}
		return *p.APS.Badge
	}
	// attention (the default): an active message does not count; an open
	// incident counts once, not again for its unread open event.
	r.post(t, writeTok, `{"source":"s","title":"fyi"}`)
	if n := badge(0); n != 0 {
		t.Fatalf("active message: badge %d, want 0", n)
	}
	id := r.post(t, writeTok, `{"source":"s","kind":"incident","key":"web","action":"open","level":"time-sensitive","title":"down"}`)
	if n := badge(1); n != 1 {
		t.Fatalf("incident: badge %d, want 1", n)
	}
	r.post(t, writeTok, `{"source":"s","title":"now","level":"time-sensitive","project":"home"}`)
	if n := badge(2); n != 2 {
		t.Fatalf("time-sensitive message: badge %d, want 2", n)
	}
	row := decision(t, r.why(t, id), "deliver")
	if row.Detail == nil || (*row.Detail)["badge_scope"] != "attention" || (*row.Detail)["badge"] != float64(1) {
		t.Fatalf("deliver detail %+v", row.Detail)
	}

	// badge.projects narrows it.
	r.set(t, "badge.projects", `["home"]`)
	r.post(t, writeTok, `{"source":"s","title":"again","level":"time-sensitive","project":"home"}`)
	if n := badge(3); n != 2 {
		t.Fatalf("projects home: badge %d, want 2", n)
	}
	r.set(t, "badge.projects", `[]`)

	// pushed: every unread event that pushed.
	r.set(t, "badge.scope", `"pushed"`)
	r.post(t, writeTok, `{"source":"s","title":"five"}`)
	if n := badge(4); n != 5 {
		t.Fatalf("pushed: badge %d, want 5", n)
	}
	// none: 0.
	r.set(t, "badge.scope", `"none"`)
	r.post(t, writeTok, `{"source":"s","title":"six"}`)
	if n := badge(5); n != 0 {
		t.Fatalf("none: badge %d, want 0", n)
	}

	// The cursor and /v1/state answer the same number as the push.
	r.set(t, "badge.scope", `"attention"`)
	var c struct{ Unread, Badge *int }
	_ = json.Unmarshal(r.do(t, http.MethodGet, "/v1/cursors/inbox", readTok, "").Body.Bytes(), &c)
	if c.Badge == nil || *c.Badge != 3 || c.Unread == nil || *c.Unread != 6 {
		t.Fatalf("cursor badge %v unread %v, want 3 and 6", c.Badge, c.Unread)
	}
	var st api.State
	_ = json.Unmarshal(r.do(t, http.MethodGet, "/v1/state", readTok, "").Body.Bytes(), &st)
	if st.Badge == nil || *st.Badge != 3 {
		t.Fatalf("state badge %v, want 3", st.Badge)
	}
	// Acking the incident takes it off the badge; its open event was
	// time-sensitive and is still unread, so it counts as that instead.
	if rec := r.do(t, http.MethodPost, "/v1/events", writeTok, `{"source":"s","kind":"incident","key":"web","action":"ack"}`); rec.Code != http.StatusCreated {
		t.Fatalf("ack: %d", rec.Code)
	}
	r.p.Wait()
	_ = json.Unmarshal(r.do(t, http.MethodGet, "/v1/cursors/inbox", readTok, "").Body.Bytes(), &c)
	if *c.Badge != 3 {
		t.Fatalf("after ack: badge %d, want 3", *c.Badge)
	}
}
