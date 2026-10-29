package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// revs reads GET /v1/revisions as a map by domain.
func (e *env) revs(t *testing.T) (string, map[string]int64) {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/v1/revisions", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Boot string
		Rev  map[string]int64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Boot, out.Rev
}

func TestRevisionsEndpoint(t *testing.T) {
	e := newEnv(t)
	boot, rev := e.revs(t)
	if boot == "" || len(rev) != len(store.Domains) {
		t.Fatalf("boot %q rev %v", boot, rev)
	}
	for _, d := range store.Domains {
		if _, ok := rev[string(d)]; !ok {
			t.Errorf("no %s", d)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/revisions", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	// A new server, as after a restart, has a new boot.
	other := newEnv(t)
	if b, _ := other.revs(t); b == boot {
		t.Fatal("two servers share a boot id")
	}
}

// Every write route bumps the domains whose read answers it changes.
func TestWriteRoutesBumpDomains(t *testing.T) {
	const holdBody = `{"ids":["h-1"],"verdict":"REAL","title":"t","body":"b","tag":"general"}`
	type call struct{ method, target, token, body string }
	cases := []struct {
		name  string
		setup []call
		// prep runs after setup; run gets what it returns.
		prep func(t *testing.T, e *env) string
		// act is the route under test; when nil, run does the work.
		act  *call
		run  func(t *testing.T, e *env, id string)
		want []store.Domain
	}{
		{name: "post event", act: &call{"POST", "/v1/events", writeTok, `{"source":"s","title":"x"}`},
			want: []store.Domain{store.DomainEvents, store.DomainCursor, store.DomainState, store.DomainProjects}},
		{name: "send event", act: &call{"GET", "/v1/send?source=s&title=x", writeTok, ""},
			want: []store.Domain{store.DomainEvents, store.DomainState}},
		{name: "apprise notify", act: &call{"POST", "/v1/notify", writeTok, `{"title":"t","body":"b"}`},
			want: []store.Domain{store.DomainEvents, store.DomainState}},
		{name: "progress event", act: &call{"POST", "/v1/events", writeTok, `{"source":"s","kind":"progress","key":"job:1","action":"start","title":"x"}`},
			want: []store.Domain{store.DomainEvents, store.DomainActivities, store.DomainState}},
		{name: "cursor put",
			setup: []call{{"POST", "/v1/events", writeTok, `{"source":"s","title":"x"}`}},
			act:   &call{"PUT", "/v1/cursors/inbox", readTok, `{"cursor":"s1"}`},
			want:  []store.Domain{store.DomainCursor, store.DomainState}},
		{name: "mute create", act: &call{"POST", "/v1/mutes", writeTok, `{"source":"s","for":"1h"}`},
			want: []store.Domain{store.DomainMutes, store.DomainState}},
		{name: "mute end",
			prep: func(t *testing.T, e *env) string {
				return decode[api.Mute](t, e.do(t, "POST", "/v1/mutes", writeTok, []byte(`{"source":"s","for":"1h"}`))).Id
			},
			run: func(t *testing.T, e *env, id string) {
				if rec := e.do(t, "DELETE", "/v1/mutes/"+id, writeTok, nil); rec.Code != http.StatusOK {
					t.Fatalf("end mute: %d %s", rec.Code, rec.Body)
				}
			},
			want: []store.Domain{store.DomainMutes, store.DomainState}},
		{name: "view create", act: &call{"POST", "/v1/views", writeTok, `{"name":"V"}`},
			want: []store.Domain{store.DomainViews}},
		{name: "view put",
			prep: newView,
			run: func(t *testing.T, e *env, id string) {
				if rec := e.do(t, "PUT", "/v1/views/"+id, writeTok, []byte(`{"name":"W"}`)); rec.Code != http.StatusOK {
					t.Fatalf("put view: %d %s", rec.Code, rec.Body)
				}
			},
			want: []store.Domain{store.DomainViews}},
		{name: "view delete",
			prep: newView,
			run: func(t *testing.T, e *env, id string) {
				if rec := e.do(t, "DELETE", "/v1/views/"+id, writeTok, nil); rec.Code/100 != 2 {
					t.Fatalf("delete view: %d %s", rec.Code, rec.Body)
				}
			},
			want: []store.Domain{store.DomainViews}},
		{name: "setting put", act: &call{"PUT", "/v1/settings/quiet.enabled", adminTok, `{"value":true}`},
			want: []store.Domain{store.DomainSettings, store.DomainState, store.DomainCursor, store.DomainLLM}},
		{name: "setting delete",
			setup: []call{{"PUT", "/v1/settings/quiet.enabled", adminTok, `{"value":true}`}},
			act:   &call{"DELETE", "/v1/settings/quiet.enabled", adminTok, ""},
			want:  []store.Domain{store.DomainSettings}},
		{name: "check create", act: &call{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`},
			want: []store.Domain{store.DomainChecks, store.DomainState, store.DomainCursor}},
		{name: "check update",
			setup: []call{{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`}},
			act:   &call{"PUT", "/v1/checkins/job", writeTok, `{"period_s":600}`},
			want:  []store.Domain{store.DomainChecks}},
		{name: "check pause",
			setup: []call{{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`}},
			act:   &call{"POST", "/v1/checkins/job/pause", writeTok, ""},
			want:  []store.Domain{store.DomainChecks, store.DomainState}},
		{name: "check resume",
			setup: []call{{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`}, {"POST", "/v1/checkins/job/pause", writeTok, ""}},
			act:   &call{"POST", "/v1/checkins/job/resume", writeTok, ""},
			want:  []store.Domain{store.DomainChecks}},
		{name: "check ping key",
			setup: []call{{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`}},
			act:   &call{"POST", "/v1/checkins/job/key", writeTok, ""},
			want:  []store.Domain{store.DomainChecks}},
		{name: "check ping key removed",
			setup: []call{{"PUT", "/v1/checkins/job", writeTok, `{"period_s":3600}`}, {"POST", "/v1/checkins/job/key", writeTok, ""}},
			act:   &call{"DELETE", "/v1/checkins/job/key", writeTok, ""},
			want:  []store.Domain{store.DomainChecks}},
		{name: "ping", act: &call{"GET", "/ping/job?create=1&period=1h", writeTok, ""},
			want: []store.Domain{store.DomainChecks, store.DomainEvents, store.DomainState}},
		{name: "dead-man sweep",
			setup: []call{{"GET", "/ping/job?create=1&period=1h&grace=0", writeTok, ""}},
			run: func(t *testing.T, e *env, _ string) {
				e.clock.add(2 * time.Hour)
				e.tick(t)
			},
			want: []store.Domain{store.DomainChecks, store.DomainState, store.DomainEvents}},
		{name: "held release",
			prep: addHold,
			act:  &call{"POST", "/held/release", writeTok, holdBody},
			want: []store.Domain{store.DomainHeld}},
		{name: "held suppress",
			prep: addHold,
			act:  &call{"POST", "/v1/hold/suppress", writeTok, `{"ids":["h-1"],"reason":"blip"}`},
			want: []store.Domain{store.DomainHeld}},
		{name: "activity instance (Live Activity report)",
			run: func(t *testing.T, e *env, _ string) {
				// The route needs push on; the Live manager writes through
				// the store, which is what bumps.
				if err := e.st.SaveInstance(context.Background(), store.Instance{ID: "lai_1", ActivityID: "a", Key: "k",
					DeviceID: "dev_1", State: "starting", RequestedAt: e.clock.now()}); err != nil {
					t.Fatal(err)
				}
			},
			want: []store.Domain{store.DomainActivities}},
		{name: "llm call log",
			run: func(t *testing.T, e *env, _ string) {
				if err := e.st.AddLLMCall(context.Background(), store.LLMCall{ID: "llm_1", Role: "test", At: e.clock.now(), Status: "ok"}); err != nil {
					t.Fatal(err)
				}
			},
			want: []store.Domain{store.DomainLLM}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			for _, s := range c.setup {
				if rec := e.do(t, s.method, s.target, s.token, body(s.body)); rec.Code/100 != 2 {
					t.Fatalf("setup %s %s: %d %s", s.method, s.target, rec.Code, rec.Body)
				}
			}
			id := ""
			if c.prep != nil {
				id = c.prep(t, e)
			}
			_, before := e.revs(t)
			if c.act != nil {
				if rec := e.do(t, c.act.method, c.act.target, c.act.token, body(c.act.body)); rec.Code/100 != 2 {
					t.Fatalf("%s %s: %d %s", c.act.method, c.act.target, rec.Code, rec.Body)
				}
			}
			if c.run != nil {
				c.run(t, e, id)
			}
			_, after := e.revs(t)
			for _, d := range c.want {
				if after[string(d)] == before[string(d)] {
					t.Errorf("%s did not move: %v -> %v", d, before, after)
				}
			}
		})
	}
}

func body(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func newView(t *testing.T, e *env) string {
	t.Helper()
	return decode[api.View](t, e.do(t, "POST", "/v1/views", writeTok, []byte(`{"name":"V"}`))).Id
}

func addHold(t *testing.T, e *env) string {
	t.Helper()
	now := e.clock.now()
	if err := e.st.AddHold(context.Background(), store.HoldEntry{ID: "h-1", TS: now, Deadline: now.Add(time.Hour),
		Title: "t", Body: "b", Tag: "alert", Source: "s", Status: store.HoldHeld}); err != nil {
		t.Fatal(err)
	}
	return "h-1"
}

// A standby serves the vector from its own state.
func TestStandbyRevisions(t *testing.T) {
	e := newEnvWith(t, func(o *Options) { o.Standby = true })
	_, before := e.revs(t)
	if rec := e.post(t, `{"source":"s","title":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	if _, after := e.revs(t); after["events"] == before["events"] {
		t.Fatalf("standby: %v -> %v", before, after)
	}
}

// Reads change nothing: a client that syncs on a poke must not cause
// another poke, and the hold's triage poll is a read too.
func TestReadsBumpNothing(t *testing.T) {
	e := newEnv(t)
	e.post(t, `{"source":"s","title":"x"}`)
	_, before := e.revs(t)
	for _, target := range []string{"/v1/events", "/v1/state", "/v1/checkins", "/v1/projects", "/v1/views",
		"/v1/activities", "/v1/llm/status", "/v1/settings", "/v1/cursors/inbox", "/held", "/v1/revisions"} {
		e.do(t, http.MethodGet, target, readTok, nil)
	}
	e.do(t, http.MethodGet, "/v1/mutes", writeTok, nil)
	if _, after := e.revs(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("reads moved revisions: %v -> %v", before, after)
	}
}

// Time alone can change a read answer: a mute runs out. The clock bumps
// the domain once, and not again while nothing changes.
func TestTimeChangesBump(t *testing.T) {
	e := newEnv(t)
	s := e.h.(*Server).s
	if rec := e.do(t, "POST", "/v1/mutes", writeTok, []byte(`{"source":"s","for":"10m"}`)); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body)
	}
	ctx := context.Background()
	s.tickTime(ctx)
	_, before := e.revs(t)
	s.tickTime(ctx)
	if _, same := e.revs(t); fmt.Sprint(same) != fmt.Sprint(before) {
		t.Fatalf("an idle tick moved revisions: %v -> %v", before, same)
	}
	e.clock.add(11 * time.Minute)
	s.tickTime(ctx)
	_, after := e.revs(t)
	if after["mutes"] == before["mutes"] || after["state"] == before["state"] {
		t.Fatalf("expiry did not bump: %v -> %v", before, after)
	}
	s.tickTime(ctx)
	if _, again := e.revs(t); fmt.Sprint(again) != fmt.Sprint(after) {
		t.Fatalf("a second tick bumped again: %v -> %v", after, again)
	}
	// A write between ticks is not bumped twice by the clock.
	if rec := e.do(t, "POST", "/v1/mutes", writeTok, []byte(`{"source":"t","for":"10m"}`)); rec.Code != http.StatusCreated {
		t.Fatal(rec.Code)
	}
	_, w := e.revs(t)
	s.tickTime(ctx)
	if _, x := e.revs(t); fmt.Sprint(x) != fmt.Sprint(w) {
		t.Fatalf("the clock bumped after a write: %v -> %v", w, x)
	}
}
