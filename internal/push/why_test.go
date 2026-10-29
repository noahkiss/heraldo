package push

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// why reads an event's trace from GET /v1/events/{id}/why and checks that
// the event's why matches its summary.
func (r *rig) why(t *testing.T, id string) api.PushTrace {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/v1/events/"+id+"/why", readTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("why: %d %s", rec.Code, rec.Body)
	}
	var tr api.PushTrace
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	var ev api.Event
	_ = json.Unmarshal(r.do(t, http.MethodGet, "/v1/events/"+id, readTok, "").Body.Bytes(), &ev)
	if (ev.Why == nil) != (tr.Why == nil) || (ev.Why != nil && *ev.Why != *tr.Why) {
		t.Fatalf("event why %v, trace why %v", ev.Why, tr.Why)
	}
	return tr
}

// stagesOf lists the trace as stage:outcome.
func stagesOf(tr api.PushTrace) string {
	var s []string
	for _, d := range tr.Decisions {
		s = append(s, d.Stage+":"+string(d.Outcome))
	}
	return strings.Join(s, " ")
}

func summary(tr api.PushTrace) string {
	if tr.Why == nil {
		return ""
	}
	return *tr.Why
}

func TestWhyPushed(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.register(t, sandboxToken, "sandbox")
	id := r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"open","title":"down"}`)
	tr := r.why(t, id)
	if got := stagesOf(tr); got != "hold:pass expiry:pass table:pass snooze:pass flap:pass replace:pass devices:pass mute:pass inhibit:pass bundle:pass quiet:pass focus:pass deliver:pass" {
		t.Fatalf("stages %s", got)
	}
	if summary(tr) != "pushed active to 2 devices" || tr.EventId != id {
		t.Fatalf("trace %+v", tr)
	}
	last := tr.Decisions[len(tr.Decisions)-1]
	if last.Detail == nil || len((*last.Detail)["devices"].([]any)) != 2 {
		t.Fatalf("deliver detail %+v", last.Detail)
	}
}

// When the push table says no, the trace says why; before stages, nothing
// was recorded.
func TestWhyNotPushed(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	cases := []struct{ body, stages, why string }{
		{`{"source":"s","title":"quiet","level":"passive"}`, "hold:pass expiry:pass table:drop", "passive: inbox only"},
		{`{"source":"s","kind":"incident","key":"k","action":"ack"}`, "hold:pass expiry:pass table:drop", "no push: incident ack"},
		{`{"source":"s","kind":"checkin","key":"backup","action":"success"}`, "hold:pass expiry:pass table:drop", "no push: checkin success"},
		{`{"source":"s","title":"late","expires_at":"2001-01-01T00:00:00Z"}`, "hold:pass expiry:drop", "expired before push"},
		{`{"source":"s","kind":"incident","key":"q","action":"resolve","title":"fine"}`,
			"hold:pass expiry:pass table:pass snooze:pass flap:pass replace:drop", "passive: inbox only (no visible push to replace)"},
	}
	for _, c := range cases {
		tr := r.why(t, r.post(t, writeTok, c.body))
		if got := stagesOf(tr); got != c.stages || summary(tr) != c.why {
			t.Errorf("%s: %s, %q; want %s, %q", c.body, got, summary(tr), c.stages, c.why)
		}
	}
	if n := len(r.prod.requests()); n != 0 {
		t.Fatalf("%d pushes, want none", n)
	}
}

func TestWhyStoreOnly(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	held := r.post(t, bridgeTok, `{"source":"s","title":"disk full",
		"details":{"tee":{"id":"n-1","hold":{"id":"h-1","state":"held","forwarded":false}}}}`)
	if got := summary(r.why(t, held)); got != "store-only: inbox only" {
		t.Fatalf("held: %q", got)
	}
	if got := stagesOf(r.why(t, held)); got != "hold:drop" {
		t.Fatalf("held stages %s", got)
	}
}

func TestWhyNoDevices(t *testing.T) {
	r := newRig(t, 0)
	tr := r.why(t, r.post(t, writeTok, `{"source":"s","title":"hi"}`))
	if summary(tr) != "no active devices" {
		t.Fatalf("why %q", summary(tr))
	}
}

func TestWhyMutedAndInhibited(t *testing.T) {
	r := newRigRules(t, 0, `inhibit:
  - name: host-down
    parent: '^host:(?P<host>[\w.-]+)$'
    children: '^(unit|container):{host}:'
`)
	r.register(t, prodToken, "production")
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"project":"web","for":"1h"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d", rec.Code)
	}
	tr := r.why(t, r.post(t, writeTok, `{"source":"deploy","project":"web","title":"deployed"}`))
	if got := summary(tr); !strings.HasPrefix(got, "muted by mute_") || !strings.HasSuffix(got, " (project)") ||
		!strings.HasSuffix(stagesOf(tr), "mute:drop") {
		t.Fatalf("muted: %q %s", got, stagesOf(tr))
	}
	r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"open","title":"web-1 is down"}`)
	tr = r.why(t, r.post(t, writeTok, `{"source":"units","kind":"incident","key":"unit:web-1:nginx","action":"open","title":"nginx"}`))
	d := tr.Decisions[len(tr.Decisions)-1]
	if summary(tr) != "inhibited by host-down: host:web-1 is open" || d.Stage != "inhibit" ||
		(*d.Detail)["parent"] != "host:web-1" {
		t.Fatalf("inhibited: %+v", tr)
	}
}

func TestWhyCriticalDowngrade(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	tr := r.why(t, r.post(t, writeTok, `{"source":"s","title":"fire","level":"critical"}`))
	if summary(tr) != "pushed time-sensitive to 1 device; critical sent as time-sensitive: no rule set it and the token lacks the critical capability" ||
		tr.Decisions[2].Outcome != api.PushDecisionOutcomeDowngrade {
		t.Fatalf("trace %+v", tr)
	}
}

func TestWhyFailedDelivery(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.register(t, sandboxToken, "sandbox")
	r.sand.setReply(func(int, fakeReq) (int, string) { return http.StatusBadRequest, "BadTopic" })
	tr := r.why(t, r.post(t, writeTok, `{"source":"s","title":"hi"}`))
	if summary(tr) != "pushed active to 1 of 2 devices" {
		t.Fatalf("why %q", summary(tr))
	}
	r.prod.setReply(func(int, fakeReq) (int, string) { return http.StatusBadRequest, "BadTopic" })
	tr = r.why(t, r.post(t, writeTok, `{"source":"s","title":"hi again"}`))
	if summary(tr) != "push failed on 2 devices" || !strings.HasSuffix(stagesOf(tr), "deliver:drop") {
		t.Fatalf("why %q %s", summary(tr), stagesOf(tr))
	}
}

// A re-alert that pushes replaces the trace; one a mute holds back keeps
// the trace of the push in force, as it keeps the delivery.
func TestWhyRealert(t *testing.T) {
	r := newRigRules(t, 0, `rules:
  - name: down
    match: {title: '^(?P<h>[\w-]+) is down'}
    set: {kind: incident, action: open, key: 'host:{h}', level: time-sensitive, realert: 10m}
`)
	r.register(t, prodToken, "production")
	ctx := context.Background()
	id := r.post(t, writeTok, `{"source":"uptime","title":"web-1 is down"}`)
	r.clock.add(10 * time.Minute)
	r.p.Realert(ctx)
	r.p.Wait()
	tr := r.why(t, id)
	if summary(tr) != "re-alerted time-sensitive to 1 device" {
		t.Fatalf("after re-alert: %q", summary(tr))
	}
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"key":"host:web-1","for":"15m"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d", rec.Code)
	}
	r.clock.add(10 * time.Minute)
	r.p.Realert(ctx)
	r.p.Wait()
	if got := summary(r.why(t, id)); got != "re-alerted time-sensitive to 1 device" {
		t.Fatalf("after a held-back re-alert: %q", got)
	}
}

// An event the pusher never saw has an empty trace and no why.
func TestWhyUnknownAndEmpty(t *testing.T) {
	r := newRig(t, 0)
	if rec := r.do(t, http.MethodGet, "/v1/events/nope/why", readTok, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	id := r.insertQuiet(t, `{"source":"s","kind":"message","title":"imported"}`, true)
	tr := r.why(t, id)
	if tr.Why != nil || len(tr.Decisions) != 0 {
		t.Fatalf("trace %+v", tr)
	}
}

// A downgrade never raises the level: the pipeline refuses it.
func TestDowngradeNeverRaises(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	saved := stages
	t.Cleanup(func() { stages = saved })
	loud := stage{"loud", func(_ *Pusher, _ context.Context, _ *Run) Verdict {
		return Downgrade(api.LevelTimeSensitive, "try to raise")
	}}
	quiet := stage{"quiet", func(_ *Pusher, _ context.Context, _ *Run) Verdict {
		return Downgrade(api.LevelPassive, "lower it")
	}}
	stages = append(append(append([]stage{}, saved[:3]...), loud, quiet), saved[3:]...)
	tr := r.why(t, r.post(t, writeTok, `{"source":"s","title":"hi"}`))
	if tr.Decisions[3].Outcome != api.PushDecisionOutcomePass || !strings.HasPrefix(tr.Decisions[3].Reason, "refused time-sensitive") {
		t.Fatalf("loud: %+v", tr.Decisions[3])
	}
	if p := payloadOf(t, r.prod.requests()[0]); p.APS.InterruptionLevel != "passive" {
		t.Fatalf("level %s", p.APS.InterruptionLevel)
	}
	if summary(tr) != "pushed passive to 1 device; lower it" {
		t.Fatalf("why %q", summary(tr))
	}
}
