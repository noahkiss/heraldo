package push

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
)

// registerCritical registers a device that reports its critical-alerts
// permission as allow: prodToken in production, any other in sandbox.
func (r *rig) registerCritical(t *testing.T, token string, allow bool) string {
	t.Helper()
	env := "sandbox"
	if token == prodToken {
		env = "production"
	}
	rec := r.do(t, http.MethodPost, "/v1/devices", readTok, fmt.Sprintf(
		`{"token":%q,"environment":%q,"topic":%q,"name":"Test phone","permissions":{"alerts":true,"critical_alerts":%t}}`,
		token, env, topic, allow))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var d api.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Permissions.CriticalAlerts == nil || *d.Permissions.CriticalAlerts != allow {
		t.Fatalf("permissions %+v", d.Permissions)
	}
	return d.Id
}

// lastAPS decodes the newest push titled fire the fake got: the
// permission-health incidents push too.
func lastAPS(t *testing.T, f *fakeAPNs) APS {
	t.Helper()
	q := f.requests()
	for i := len(q) - 1; i >= 0; i-- {
		var p Payload
		if err := json.Unmarshal(q[i].Body, &p); err != nil {
			t.Fatal(err)
		}
		if p.APS.Alert.Title == "fire" {
			return p.APS
		}
	}
	t.Fatal("no push titled fire")
	return APS{}
}

func TestCriticalPerDevice(t *testing.T) {
	r := newRig(t, 0)
	r.registerCritical(t, prodToken, true)
	other := r.registerCritical(t, sandboxToken, false)

	// A plain write token's critical is time-sensitive everywhere.
	id := r.post(t, writeTok, `{"source":"s","title":"fire","level":"critical"}`)
	if a := lastAPS(t, r.prod); a.InterruptionLevel != "time-sensitive" || a.CriticalSound != nil || a.Sound != "default" {
		t.Fatalf("plain token: %+v", a)
	}
	if !strings.Contains(summary(r.why(t, id)), "token lacks the critical capability") {
		t.Fatalf("why %q", summary(r.why(t, id)))
	}

	// The critical token's critical is critical where the device allows it.
	id = r.post(t, critTok, `{"source":"s","title":"fire","level":"critical"}`)
	a := lastAPS(t, r.prod)
	if a.InterruptionLevel != "critical" || a.CriticalSound == nil || *a.CriticalSound != (CriticalSound{Critical: 1, Name: "default", Volume: 1}) {
		t.Fatalf("critical device: %+v", a)
	}
	if b, _ := json.Marshal(a); !strings.Contains(string(b), `"sound":{"critical":1,"name":"default","volume":1}`) {
		t.Fatalf("payload %s", b)
	}
	if a := lastAPS(t, r.sand); a.InterruptionLevel != "time-sensitive" || a.CriticalSound != nil {
		t.Fatalf("device without critical alerts: %+v", a)
	}
	tr := r.why(t, id)
	if summary(tr) != "pushed critical to 2 devices; time-sensitive on 1 device without critical alerts" {
		t.Fatalf("why %q", summary(tr))
	}
	if d := tr.Decisions[2]; d.Reason != "push table: critical, allowed by token:pager" {
		t.Fatalf("table row %+v", d)
	}
	for _, d := range r.delivery(t, id) {
		want := "critical"
		if d.Device == other {
			want = "time-sensitive"
		}
		if d.Level == nil || string(*d.Level) != want {
			t.Fatalf("delivery %+v, want level %s", d, want)
		}
	}
}

// A sender cannot write the server's critical mark itself.
func TestCriticalMarkForged(t *testing.T) {
	r := newRig(t, 0)
	r.registerCritical(t, prodToken, true)
	r.post(t, writeTok, `{"source":"s","title":"fire","level":"critical","details":{"heraldo":{"critical":"token:pager"}}}`)
	if a := lastAPS(t, r.prod); a.InterruptionLevel != "time-sensitive" {
		t.Fatalf("forged mark: %+v", a)
	}
	rec := r.do(t, http.MethodGet, "/v1/events?limit=1", readTok, "")
	if strings.Contains(rec.Body.String(), `"critical":"token:pager"`) {
		t.Fatalf("the forged mark was stored: %s", rec.Body)
	}
}

// A level an ingest rule sets needs no capability.
func TestCriticalByRule(t *testing.T) {
	r := newRigRules(t, 0, `rules:
  - name: ops-rung
    match: {tag: '^ops$'}
    set: {kind: incident, action: open, key: 'ops:site', level: critical}
`)
	r.registerCritical(t, prodToken, true)
	id := r.post(t, writeTok, `{"source":"s","title":"fire","tags":["ops"]}`)
	if a := lastAPS(t, r.prod); a.InterruptionLevel != "critical" {
		t.Fatalf("rule critical: %+v", a)
	}
	if d := r.why(t, id).Decisions[2]; d.Reason != "push table: critical, allowed by rule:ops-rung" {
		t.Fatalf("table row %+v", d)
	}
}

func stageRow(t *testing.T, tr api.PushTrace, stage string) api.PushDecision {
	t.Helper()
	for _, d := range tr.Decisions {
		if d.Stage == stage {
			return d
		}
	}
	t.Fatalf("no %s row in %s", stage, stagesOf(tr))
	return api.PushDecision{}
}
