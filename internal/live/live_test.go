package live_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/live"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	writeTok = "write-token-for-tests"
	readTok  = "read-token-for-tests"
	topic    = "org.example.heraldo"
	// Hand-made tokens, hex.
	phoneToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	macToken   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	startToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// updateToken is a made-up activity token, different per n.
func updateToken(n int) string { return strings.Repeat(fmt.Sprintf("%x", n%16), 80) }

// clock is the fake clock every part of the rig reads.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// sent is one push the fake APNs got, decoded.
type sent struct {
	push.LivePush
	APS map[string]any
}

func (s sent) event() string { e, _ := s.APS["event"].(string); return e }

func (s sent) state() map[string]any { m, _ := s.APS["content-state"].(map[string]any); return m }

func (s sent) num(k string) int64 { f, _ := s.APS[k].(float64); return int64(f) }

// fakeAPNs records each push. reply picks the answer by token; nil is 200.
type fakeAPNs struct {
	mu    sync.Mutex
	got   []sent
	reply func(token string) push.LiveResult
}

func (f *fakeAPNs) SendLive(_ context.Context, lp push.LivePush) push.LiveResult {
	var body struct {
		APS map[string]any `json:"aps"`
	}
	_ = json.Unmarshal(lp.Body, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, sent{LivePush: lp, APS: body.APS})
	if f.reply != nil {
		return f.reply(lp.Token)
	}
	return push.LiveResult{Status: http.StatusOK}
}

// take returns the pushes since the last take.
func (f *fakeAPNs) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.got
	f.got = nil
	return out
}

// syncLive runs the manager's queued work at once, so a test sees its
// effect when the request returns.
type syncLive struct{ *live.Manager }

func (s syncLive) Stored(id, key, action string) { s.Handle(context.Background(), id, key, action) }

func (s syncLive) Enqueue(job func(context.Context)) { job(context.Background()) }

type rig struct {
	h    http.Handler
	st   *store.Store
	m    *live.Manager
	apns *fakeAPNs
	clk  *clock
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{t: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	st.SetClock(clk.now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{st: st, apns: &fakeAPNs{}, clk: clk}
	r.m = live.New(live.Config{Store: st, Sender: r.apns, Logger: log, Now: clk.now})
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf("sender write %s,phone read %s",
		auth.Hash(writeTok), auth.Hash(readTok))), ',')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	r.h = server.New(server.Options{Store: st, Tokens: tokens, Validator: v, Logger: log, Now: clk.now,
		Live: syncLive{r.m}, PushState: api.HealthPushOn})
	return r
}

func (r *rig) do(t *testing.T, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// phone registers an iPhone with a push-to-start token and a Mac without
// one, and returns the phone's id.
func (r *rig) phone(t *testing.T) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/devices", readTok, fmt.Sprintf(
		`{"token":%q,"environment":"production","topic":%q,"platform":"iOS","live_activity_token":%q}`,
		phoneToken, topic, startToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register phone: %d %s", rec.Code, rec.Body)
	}
	var d api.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.LiveActivityTokenSuffix == nil || *d.LiveActivityTokenSuffix != startToken[len(startToken)-8:] {
		t.Fatalf("live_activity_token_suffix = %v", d.LiveActivityTokenSuffix)
	}
	rec = r.do(t, http.MethodPost, "/v1/devices", readTok, fmt.Sprintf(
		`{"token":%q,"environment":"production","topic":%q,"platform":"macOS"}`, macToken, topic))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register mac: %d %s", rec.Code, rec.Body)
	}
	return d.Id
}

func (r *rig) post(t *testing.T, body string) {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/events", writeTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
}

func (r *rig) report(t *testing.T, id, body string) api.ActivityInstance {
	t.Helper()
	rec := r.do(t, http.MethodPut, "/v1/activity-instances/"+id, readTok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	var in api.ActivityInstance
	_ = json.Unmarshal(rec.Body.Bytes(), &in)
	return in
}

func (r *rig) activity(t *testing.T, key string) api.Activity {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/v1/activities/"+key, readTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: %d %s", rec.Code, rec.Body)
	}
	var a api.Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func (r *rig) tick() { r.m.Tick(context.Background()) }

const start = `{"source":"printer","kind":"progress","key":"print/42","action":"start","level":"active",
	"content":{"title":"bracket.3mf","symbol":"printer.fill","tint":"blue","progress":0,"status":"Printing",
	"ends_at":"2026-10-27T19:00:00Z","metrics":[{"label":"Nozzle","value":"215","unit":"°C"}],
	"button":{"id":"pause","title":"Pause","type":"http","url":"http://192.0.2.20/pause"}}}`

func update(progress float64) string {
	return fmt.Sprintf(`{"source":"printer","kind":"progress","key":"print/42","action":"update","content":{"progress":%v}}`, progress)
}

// begin starts print/42, confirms it on the phone with update token n, and
// returns the instance id.
func (r *rig) begin(t *testing.T, n int) string {
	t.Helper()
	r.post(t, start)
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("start pushes = %+v", got)
	}
	attrs, _ := got[0].APS["attributes"].(map[string]any)
	id, _ := attrs["instance"].(string)
	r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(n)))
	r.apns.take()
	return id
}

func TestStartPushesToStartToken(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.post(t, start)
	got := r.apns.take()
	if len(got) != 1 {
		t.Fatalf("pushes = %d, want 1 (the Mac has no push-to-start token)", len(got))
	}
	p := got[0]
	if p.Token != startToken || p.Topic != topic || p.Priority != 10 || p.Environment != "production" {
		t.Errorf("push = token %s topic %s priority %d env %s", p.Token, p.Topic, p.Priority, p.Environment)
	}
	if p.event() != "start" || p.APS["attributes-type"] != live.AttributesType || p.num("input-push-token") != 1 {
		t.Errorf("aps = %v", p.APS)
	}
	attrs, _ := p.APS["attributes"].(map[string]any)
	if attrs["key"] != "print/42" || attrs["source"] != "printer" || !strings.HasPrefix(attrs["instance"].(string), "lai_") {
		t.Errorf("attributes = %v", attrs)
	}
	st := p.state()
	if st["title"] != "bracket.3mf" || st["status"] != "Printing" || st["tint"] != "blue" {
		t.Errorf("content-state = %v", st)
	}
	if ends, _ := st["ends_at"].(float64); int64(ends) != time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("ends_at = %v, want Unix seconds", st["ends_at"])
	}
	alert, _ := p.APS["alert"].(map[string]any)
	if alert["title"] != "bracket.3mf" || alert["sound"] != "default" {
		t.Errorf("alert = %v", alert)
	}
	// No update interval yet: stale after twice the 10 minute default.
	if p.num("stale-date") != r.clk.now().Add(20*time.Minute).Unix() {
		t.Errorf("stale-date = %d", p.num("stale-date"))
	}
	if p.Expiration != r.clk.now().Add(live.DefaultStartTimeout).Unix() {
		t.Errorf("expiration = %d", p.Expiration)
	}
	a := r.activity(t, "print/42")
	if a.State != "active" || len(a.Instances) != 1 || a.Instances[0].State != "starting" || a.Timeline == nil || len(*a.Timeline) != 1 {
		t.Fatalf("activity = %+v", a)
	}
}

func TestConfirmPushesCurrentContent(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.post(t, start)
	attrs, _ := r.apns.take()[0].APS["attributes"].(map[string]any)
	id := attrs["instance"].(string)
	// An update before the token comes cannot reach the activity.
	r.clk.add(time.Minute)
	r.post(t, update(0.1))
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes before the token = %d", len(got))
	}
	in := r.report(t, id, fmt.Sprintf(`{"token":%q}`, strings.ToUpper(updateToken(1))))
	if in.State != "active" || in.ConfirmedAt == nil {
		t.Fatalf("instance = %+v", in)
	}
	got := r.apns.take()
	if len(got) != 1 || got[0].Token != updateToken(1) || got[0].event() != "update" || got[0].Priority != 10 {
		t.Fatalf("catch-up = %+v", got)
	}
	if got[0].state()["progress"] != 0.1 {
		t.Errorf("progress = %v", got[0].state()["progress"])
	}
	// Reporting the same token again pushes nothing.
	r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes after a repeated token = %d", len(got))
	}
}

func TestThrottleAndPriority(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)

	// The first progress update after the catch-up push waits out the 15 s.
	r.clk.add(time.Second)
	r.post(t, update(0.2))
	r.clk.add(time.Second)
	r.post(t, update(0.3))
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("throttled pushes = %d", len(got))
	}
	r.clk.add(10 * time.Second)
	r.tick()
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes at 12 s = %d", len(got))
	}
	r.clk.add(5 * time.Second)
	r.tick()
	got := r.apns.take()
	if len(got) != 1 || got[0].Priority != 5 || got[0].state()["progress"] != 0.3 {
		t.Fatalf("coalesced push = %+v", got)
	}
	// Two updates 1 s apart: stale after twice the 15 s floor.
	if got[0].num("stale-date") != r.clk.now().Add(30*time.Second).Unix() {
		t.Errorf("stale-date = %d, want now + 30 s", got[0].num("stale-date"))
	}

	// A new status is not a state change: it waits out the 15 s, at
	// priority 5, like progress.
	r.clk.add(time.Second)
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"update","content":{"status":"Paused","tint":"orange"}}`)
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("status change pushed at once: %+v", got)
	}
	r.clk.add(15 * time.Second)
	r.tick()
	got = r.apns.take()
	if len(got) != 1 || got[0].Priority != 5 || got[0].state()["status"] != "Paused" || got[0].state()["progress"] != 0.3 {
		t.Fatalf("status change = %+v", got)
	}

	// Sender events 75 s apart (the status, then this): stale after 150 s.
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	got = r.apns.take()
	if len(got) != 1 || got[0].Priority != 5 || got[0].num("stale-date") != r.clk.now().Add(150*time.Second).Unix() {
		t.Fatalf("minute update = %+v", got)
	}
	if got[0].Expiration != got[0].num("stale-date") {
		t.Errorf("expiration %d, want the stale date", got[0].Expiration)
	}
}

func TestStaleAfterWins(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.clk.add(time.Minute)
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"update","content":{"progress":0.5,"stale_after":900}}`)
	got := r.apns.take()
	if len(got) != 1 || got[0].num("stale-date") != r.clk.now().Add(15*time.Minute).Unix() {
		t.Fatalf("push = %+v", got)
	}
	if _, ok := got[0].state()["stale_after"]; ok {
		t.Error("stale_after is in the content state")
	}
}

func TestEndWritesOneHistoryEvent(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.clk.add(3*time.Hour + 20*time.Minute)
	r.post(t, update(0.9))
	r.apns.take()
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"progress":1,"status":"Done","tint":"green","keep_for":600}}`)
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "end" || got[0].Priority != 10 || got[0].Token != updateToken(1) {
		t.Fatalf("end pushes = %+v", got)
	}
	if got[0].num("dismissal-date") != r.clk.now().Add(10*time.Minute).Unix() || got[0].state()["status"] != "Done" {
		t.Errorf("end aps = %v", got[0].APS)
	}
	a := r.activity(t, "print/42")
	if a.State != "ended" || a.EndReason == nil || *a.EndReason != "sender" || a.Instances[0].State != "ended" {
		t.Fatalf("activity = %+v", a)
	}
	if len(*a.Timeline) != 3 {
		t.Errorf("timeline = %d entries, want 3", len(*a.Timeline))
	}
	msgs := r.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("history events = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if *m.Title != "bracket.3mf" || *m.Summary != "Done · ran 3h 20m" || *m.Level != api.LevelPassive || *m.Key != "print/42" {
		t.Errorf("history event = title %q summary %q", *m.Title, *m.Summary)
	}
	// A repeated end changes nothing and writes nothing.
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"end"}`)
	if got := r.apns.take(); len(got) != 0 {
		t.Errorf("pushes after a repeated end = %d", len(got))
	}
	if n := len(r.messages(t)); n != 1 {
		t.Errorf("history events after a repeated end = %d", n)
	}
}

// messages lists the message events: the activities' history events.
func (r *rig) messages(t *testing.T) []api.Event {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/v1/events?kind=message", readTok, "")
	var page struct{ Events []api.Event }
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func TestNeverStartedRetriesThenStops(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.post(t, start)
	r.apns.take()
	for i := 1; i <= 3; i++ {
		r.clk.add(live.DefaultStartTimeout)
		r.tick()
		r.post(t, update(float64(i)/10))
		got := r.apns.take()
		want := 1
		if i == 3 {
			want = 0 // three push-to-starts never produced an activity
		}
		if len(got) != want || (want == 1 && got[0].event() != "start") {
			t.Fatalf("round %d: pushes = %+v", i, got)
		}
	}
	a := r.activity(t, "print/42")
	for _, in := range a.Instances {
		if in.State != "never-started" {
			t.Errorf("instance %s = %s", in.Id, in.State)
		}
	}
}

func TestLateTokenRevivesNeverStarted(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.post(t, start)
	attrs, _ := r.apns.take()[0].APS["attributes"].(map[string]any)
	id := attrs["instance"].(string)
	r.clk.add(live.DefaultStartTimeout)
	r.tick()
	in := r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	if in.State != "active" {
		t.Fatalf("late instance = %s", in.State)
	}
	if got := r.apns.take(); len(got) != 1 || got[0].event() != "update" {
		t.Fatalf("pushes = %+v", got)
	}
}

func TestDismissedIsNeverRespawned(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	id := r.begin(t, 1)
	if in := r.report(t, id, `{"state":"dismissed"}`); in.State != "dismissed" {
		t.Fatalf("state = %s", in.State)
	}
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	r.clk.add(live.DefaultChainAfter)
	r.post(t, update(0.9))
	r.tick()
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes after dismissal = %+v", got)
	}
}

func TestDeadTokenIsNeverRespawned(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.apns.reply = func(token string) push.LiveResult {
		if token == updateToken(1) {
			return push.LiveResult{Status: http.StatusGone, Reason: "Unregistered"}
		}
		return push.LiveResult{Status: http.StatusOK}
	}
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	if got := r.apns.take(); len(got) != 1 {
		t.Fatalf("pushes = %d", len(got))
	}
	r.clk.add(time.Minute)
	r.post(t, update(0.6))
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes after the dead token = %+v", got)
	}
	if a := r.activity(t, "print/42"); a.Instances[0].State != "dead" {
		t.Errorf("instance = %s", a.Instances[0].State)
	}
}

func TestDeadPushToStartTokenIsCleared(t *testing.T) {
	r := newRig(t)
	dev := r.phone(t)
	r.apns.reply = func(string) push.LiveResult {
		return push.LiveResult{Status: http.StatusBadRequest, Reason: "BadDeviceToken"}
	}
	r.post(t, start)
	r.apns.take()
	d, err := r.st.GetDevice(context.Background(), dev)
	if err != nil || d.LiveActivityToken != "" {
		t.Fatalf("push-to-start token = %q, %v", d.LiveActivityToken, err)
	}
	// A new token from the app starts the running activity.
	r.apns.reply = nil
	rec := r.do(t, http.MethodPut, "/v1/devices/"+dev, readTok, fmt.Sprintf(
		`{"token":%q,"environment":"production","topic":%q,"platform":"iOS","live_activity_token":%q}`,
		phoneToken, topic, strings.Repeat("d", 80)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update device: %d %s", rec.Code, rec.Body)
	}
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" || got[0].Token != strings.Repeat("d", 80) {
		t.Fatalf("pushes = %+v", got)
	}
}

func TestChainHandOff(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	first := r.begin(t, 1)
	// A 10 hour print, updates every 10 minutes.
	var second string
	for elapsed := time.Duration(0); elapsed < 10*time.Hour; elapsed += 10 * time.Minute {
		r.clk.add(10 * time.Minute)
		r.post(t, update(float64(elapsed)/float64(10*time.Hour)))
		r.tick()
		for _, p := range r.apns.take() {
			switch {
			case p.event() == "start":
				if second != "" {
					t.Fatalf("a second hand-off at %v", elapsed)
				}
				if elapsed+10*time.Minute < live.DefaultChainAfter {
					t.Fatalf("hand-off at %v, before %v", elapsed, live.DefaultChainAfter)
				}
				attrs, _ := p.APS["attributes"].(map[string]any)
				second = attrs["instance"].(string)
				if p.state()["title"] != "bracket.3mf" {
					t.Errorf("the new link lost the content: %v", p.state())
				}
				r.report(t, second, fmt.Sprintf(`{"token":%q}`, updateToken(2)))
			case p.event() == "end":
				if p.Token != updateToken(1) || p.num("dismissal-date") > r.clk.now().Unix() {
					t.Errorf("old link end = token %s dismissal %d", p.Token, p.num("dismissal-date"))
				}
			case second != "" && p.Token == updateToken(1):
				t.Errorf("update to the old link after the hand-off")
			}
		}
	}
	if second == "" {
		t.Fatal("no hand-off in 10 hours")
	}
	a := r.activity(t, "print/42")
	states := map[string]string{}
	for _, in := range a.Instances {
		states[in.Id] = string(in.State)
	}
	if states[first] != "ended" || states[second] != "active" || len(states) != 2 {
		t.Fatalf("instances = %v", states)
	}
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"status":"Done"}}`)
	got := r.apns.take()
	if len(got) != 1 || got[0].Token != updateToken(2) || got[0].event() != "end" {
		t.Fatalf("end = %+v", got)
	}
	m := r.messages(t)
	if len(m) != 1 || !strings.Contains(*m[0].Summary, "ran 10h") {
		t.Fatalf("history = %+v", m)
	}
}

func TestAbandonedActivityEnds(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.clk.add(live.DefaultAbandonAfter - time.Second)
	r.tick()
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes before abandon = %+v", got)
	}
	r.clk.add(time.Second)
	r.tick()
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "end" {
		t.Fatalf("pushes = %+v", got)
	}
	a := r.activity(t, "print/42")
	if a.State != "ended" || *a.EndReason != "abandoned" {
		t.Fatalf("activity = %+v", a)
	}
	if m := r.messages(t); len(m) != 1 || !strings.Contains(*m[0].Summary, "Printing") {
		t.Fatalf("history = %+v", m)
	}
	// A later update starts a new activity on the key.
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	if got := r.apns.take(); len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("pushes = %+v", got)
	}
}

func TestStartReplacesRunningActivity(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	first := r.begin(t, 1)
	r.clk.add(time.Minute)
	r.post(t, start)
	got := r.apns.take()
	var events []string
	for _, p := range got {
		events = append(events, p.event())
	}
	if strings.Join(events, ",") != "end,start" {
		t.Fatalf("pushes = %v", events)
	}
	in, err := r.st.GetInstance(context.Background(), first)
	if err != nil || in.State != "ended" || in.EndReason != store.EndRestarted {
		t.Fatalf("old instance = %+v %v", in, err)
	}
}

func TestReportChecks(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	id := r.begin(t, 1)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"state":"paused"}`, http.StatusBadRequest},
		{`{"token":"xyz"}`, http.StatusBadRequest},
		{`{"device":"dev_other","state":"dismissed"}`, http.StatusBadRequest},
		{`{"extra":1}`, http.StatusBadRequest},
	} {
		if rec := r.do(t, http.MethodPut, "/v1/activity-instances/"+id, readTok, c.body); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.body, rec.Code, c.want)
		}
	}
	if rec := r.do(t, http.MethodPut, "/v1/activity-instances/lai_none", readTok, `{"state":"ended"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance: %d", rec.Code)
	}
	if rec := r.do(t, http.MethodGet, "/v1/activities/none", readTok, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown activity: %d", rec.Code)
	}
	rec := r.do(t, http.MethodGet, "/v1/activities", readTok, "")
	var list api.ActivityList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Activities) != 1 || list.Activities[0].Timeline != nil {
		t.Errorf("list = %+v", list)
	}
}

func TestSystemEndStartsNextLink(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	id := r.begin(t, 1)
	r.report(t, id, `{"state":"ended"}`)
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("pushes = %+v", got)
	}
}

// compat calls the Notify!-compatible API with the token in the URL, as
// notify-bambuddy does.
func (r *rig) compat(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req := httptest.NewRequest(method, path+sep+"token="+writeTok, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestNotifyCompat(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	if code, out := r.compat(t, http.MethodGet, "/link?id=printer-farm", ""); code != 200 || out["type"] != "device" {
		t.Fatalf("link: %d %v", code, out)
	}
	code, out := r.compat(t, http.MethodPost, "/live-activity/printer-farm?new=1", `{"title":"X2D","symbol":"printer.fill",
		"tint":"#FF00A76F","body":"bracket","progress":12,"steps":10,"step":3,"endsIn":3600,"status":"Printing",
		"button":{"title":"Open","url":"http://192.0.2.30/","open":true}}`)
	if code != 200 || out["success"] != true {
		t.Fatalf("start: %d %v", code, out)
	}
	id := out["activityId"].(string)
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("pushes = %+v", got)
	}
	st := got[0].state()
	steps, _ := st["steps"].(map[string]any)
	btn, _ := st["button"].(map[string]any)
	if st["progress"] != 0.12 || steps["current"] != 3.0 || st["tint"] != "#00A76FFF" || btn["type"] != "open" {
		t.Errorf("content-state = %v", st)
	}
	if ends, _ := st["ends_at"].(float64); int64(ends) != r.clk.now().Add(time.Hour).Unix() {
		t.Errorf("ends_at = %v", st["ends_at"])
	}
	if code, out := r.compat(t, http.MethodGet, "/live-activity/"+id, ""); code != 200 || out["state"] != "starting" {
		t.Fatalf("status: %d %v", code, out)
	}
	attrs, _ := got[0].APS["attributes"].(map[string]any)
	inst := attrs["instance"].(string)
	r.report(t, inst, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	r.apns.take()
	if _, out := r.compat(t, http.MethodGet, "/live-activity/printer-farm", ""); len(out["activities"].([]any)) != 1 {
		t.Fatalf("list = %v", out)
	}
	r.clk.add(time.Minute)
	if code, _ := r.compat(t, http.MethodPost, "/live-activity/"+id, `{"progress":50,"endsIn":null}`); code != 200 {
		t.Fatalf("update: %d", code)
	}
	if got := r.apns.take(); len(got) != 1 || got[0].state()["progress"] != 0.5 || got[0].state()["ends_at"] != nil {
		t.Fatalf("update pushes = %+v", got)
	}

	// The person swipes it away: updates get 410, and the status says why.
	r.report(t, inst, `{"state":"dismissed"}`)
	if code, _ := r.compat(t, http.MethodPost, "/live-activity/"+id, `{"progress":60}`); code != http.StatusGone {
		t.Fatalf("update after dismissal: %d", code)
	}
	if _, out := r.compat(t, http.MethodGet, "/live-activity/"+id, ""); out["state"] != "dismissed" || out["endReason"] != "dismissed" {
		t.Fatalf("status after dismissal = %v", out)
	}
	if code, _ := r.compat(t, http.MethodDelete, "/live-activity/"+id, `{"keepFor":0,"status":"Done"}`); code != 200 {
		t.Fatalf("end: %d", code)
	}
	if _, out := r.compat(t, http.MethodGet, "/live-activity/"+id, ""); out["state"] != "ended" || out["endReason"] != "script" {
		t.Fatalf("status after end = %v", out)
	}
	if code, _ := r.compat(t, http.MethodDelete, "/live-activity/"+id, ""); code != 200 {
		t.Fatalf("second end: %d", code)
	}
	if code, _ := r.compat(t, http.MethodDelete, "/live-activity/notify-la-unknown", ""); code != http.StatusForbidden {
		t.Fatalf("end unknown: %d", code)
	}

	if code, _ := r.compat(t, http.MethodPost, "/notify-json/printer-farm", `{"text":"Print finished","title":"X2D",
		"groupType":"printer-x2d","iconUrl":"https://example.com/icon.png","timeSensitive":true}`); code != 200 {
		t.Fatalf("notify-json: %d", code)
	}
	msgs := r.messages(t)
	last := msgs[len(msgs)-1]
	if *last.Body != "Print finished" || *last.Thread != "printer-x2d" || *last.Level != api.LevelTimeSensitive || last.Source != "sender" {
		t.Errorf("message = %+v", last)
	}
	// A token in the URL only works with write scope.
	req := httptest.NewRequest(http.MethodGet, "/link?id=x&token="+readTok, nil)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("read token on the compat API: %d", rec.Code)
	}
}

// sound returns the alert sound of a start push, "" when it has none.
func sound(p sent) string {
	alert, _ := p.APS["alert"].(map[string]any)
	s, _ := alert["sound"].(string)
	return s
}

func TestOnlyTheFirstStartSounds(t *testing.T) {
	// A retry after never-started comes up silently.
	r := newRig(t)
	r.phone(t)
	r.post(t, start)
	if got := r.apns.take(); len(got) != 1 || sound(got[0]) != "default" {
		t.Fatalf("first start = %+v", got)
	}
	r.clk.add(live.DefaultStartTimeout)
	r.tick()
	r.post(t, update(0.1))
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" || sound(got[0]) != "" {
		t.Fatalf("retry = %+v", got)
	}
	if alert, _ := got[0].APS["alert"].(map[string]any); alert["title"] != "bracket.3mf" {
		t.Errorf("retry alert = %v, want a title and no sound", alert)
	}

	// A chain link comes up silently.
	r = newRig(t)
	r.phone(t)
	r.begin(t, 1)
	for i := 0; i < 46; i++ { // 7 h 40 min in 10 minute steps
		r.clk.add(10 * time.Minute)
		r.post(t, update(float64(i)/100))
		r.tick()
	}
	var link *sent
	for _, p := range r.apns.take() {
		if p.event() == "start" {
			link = &p
		}
	}
	if link == nil || sound(*link) != "" {
		t.Fatalf("chain link = %+v", link)
	}

	// A next link after a system end comes up silently.
	r = newRig(t)
	r.phone(t)
	id := r.begin(t, 1)
	r.report(t, id, `{"state":"ended"}`)
	r.clk.add(time.Minute)
	r.post(t, update(0.5))
	if got := r.apns.take(); len(got) != 1 || got[0].event() != "start" || sound(got[0]) != "" {
		t.Fatalf("next link = %+v", got)
	}
}

// TestFocusFilterStartIsSilent: on a device in a Focus filter, the activity
// starts with its alert text and no sound.
func TestFocusFilterStartIsSilent(t *testing.T) {
	r := newRig(t)
	id := r.phone(t)
	rec := r.do(t, http.MethodPut, "/v1/devices/"+id, readTok, fmt.Sprintf(
		`{"token":%q,"environment":"production","topic":%q,"platform":"iOS","live_activity_token":%q,"focus_filter":{"active":true,"name":"Sleep"}}`,
		phoneToken, topic, startToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("focus: %d %s", rec.Code, rec.Body)
	}
	r.apns.take()
	r.post(t, start)
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" || sound(got[0]) != "" {
		t.Fatalf("start in focus = %+v", got)
	}
	if alert, _ := got[0].APS["alert"].(map[string]any); alert["title"] != "bracket.3mf" {
		t.Errorf("alert = %v, want a title and no sound", alert)
	}
}

// instanceOf returns the instance id in a start push.
func instanceOf(p sent) string {
	attrs, _ := p.APS["attributes"].(map[string]any)
	id, _ := attrs["instance"].(string)
	return id
}

func TestTokenAfterEndGetsTheEnd(t *testing.T) {
	for _, c := range []struct {
		name, ender string
	}{
		{"end", `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"status":"Done"}}`},
		{"restart", start},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.phone(t)
			r.post(t, start)
			id := instanceOf(r.apns.take()[0])
			r.clk.add(3 * time.Second)
			r.post(t, c.ender)
			r.apns.take()
			r.clk.add(2 * time.Second)
			r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
			got := r.apns.take()
			if len(got) != 1 || got[0].event() != "end" || got[0].Token != updateToken(1) {
				t.Fatalf("pushes after the late token = %+v", got)
			}
			if got[0].num("dismissal-date") > r.clk.now().Unix() {
				t.Errorf("dismissal-date = %d, want now", got[0].num("dismissal-date"))
			}
			// The same token again sends nothing more.
			r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
			if got := r.apns.take(); len(got) != 0 {
				t.Errorf("pushes after a repeated token = %+v", got)
			}
		})
	}
}

func TestChainStartBacksOff(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	for e := time.Duration(0); e < 7*time.Hour; e += 30 * time.Minute {
		r.clk.add(30 * time.Minute)
		r.post(t, update(0.1))
		r.tick()
	}
	r.apns.take()
	r.apns.reply = func(tok string) push.LiveResult {
		if tok == startToken {
			return push.LiveResult{Status: http.StatusTooManyRequests, Reason: "TooManyRequests"}
		}
		return push.LiveResult{Status: http.StatusOK}
	}
	r.clk.add(30 * time.Minute) // past 7.5 h
	r.post(t, update(0.2))
	var at []time.Duration
	begin := r.clk.now()
	for i := 0; i < 12*20; i++ { // 20 minutes of 5 s ticks
		r.clk.add(5 * time.Second)
		r.tick()
		for _, p := range r.apns.take() {
			if p.event() == "start" {
				at = append(at, r.clk.now().Sub(begin))
			}
			if p.event() == "end" {
				t.Fatal("the old link ended although no new one started")
			}
		}
	}
	// 30 s, then 1, 2, 4, 8 minutes apart: 6 tries in 20 minutes, not 240.
	if len(at) < 4 || len(at) > 7 {
		t.Fatalf("push-to-start tries in 20 minutes = %d at %v", len(at), at)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i] - at[i-1]; gap < 30*time.Second || (i > 1 && gap < at[i-1]-at[i-2]) {
			t.Fatalf("tries at %v do not back off", at)
		}
	}
	// When APNs takes it again, the hand-off goes through.
	r.apns.reply = nil
	r.clk.add(10 * time.Minute)
	r.tick()
	var events []string
	for _, p := range r.apns.take() {
		events = append(events, p.event())
	}
	if strings.Join(events, ",") != "start,end" {
		t.Fatalf("hand-off after recovery = %v", events)
	}
}

func TestEndRetriesAfterPassingFailure(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.apns.reply = func(string) push.LiveResult { return push.LiveResult{Status: http.StatusServiceUnavailable} }
	r.clk.add(time.Minute)
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"status":"Done"}}`)
	if got := r.apns.take(); len(got) != 1 || got[0].event() != "end" {
		t.Fatalf("end = %+v", got)
	}
	r.clk.add(10 * time.Second)
	r.tick()
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("retried before the backoff: %+v", got)
	}
	r.apns.reply = nil
	r.clk.add(30 * time.Second)
	r.tick()
	if got := r.apns.take(); len(got) != 1 || got[0].event() != "end" {
		t.Fatalf("retry = %+v", got)
	}
	r.clk.add(time.Hour)
	r.tick()
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes after a sent end = %+v", got)
	}
}

func TestResyncAbandonsStaleAndStartsSilently(t *testing.T) {
	r := newRig(t)
	r.post(t, start) // no devices yet
	r.clk.add(5 * time.Hour)
	r.post(t, `{"source":"backup","kind":"progress","key":"backup/1","action":"start","level":"active","content":{"title":"Backup"}}`)
	r.phone(t) // the new push-to-start token resyncs
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("pushes = %+v", got)
	}
	if attrs, _ := got[0].APS["attributes"].(map[string]any); attrs["key"] != "backup/1" {
		t.Errorf("started %v, want only the fresh activity", attrs["key"])
	}
	if sound(got[0]) != "" {
		t.Errorf("a resync start sounds")
	}
	if a := r.activity(t, "print/42"); a.State != "ended" || *a.EndReason != "abandoned" {
		t.Errorf("stale activity = %s", a.State)
	}
}

func TestStatusCountersAreThrottled(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	n := 0
	for i := 0; i < 20; i++ {
		r.clk.add(time.Second)
		r.post(t, fmt.Sprintf(`{"source":"printer","kind":"progress","key":"print/42","action":"update","content":{"status":"Layer %d"}}`, i))
		r.tick()
		for _, p := range r.apns.take() {
			if p.Priority != 5 {
				t.Fatalf("a status change went at priority %d", p.Priority)
			}
			n++
		}
	}
	if n > 2 {
		t.Fatalf("20 status changes in 20 s gave %d pushes, want at most 2", n)
	}
}

func TestEndTimestampFollowsUpdate(t *testing.T) {
	r := newRig(t)
	r.phone(t)
	r.begin(t, 1)
	r.clk.add(time.Minute)
	r.post(t, update(0.9))
	r.post(t, `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"status":"Done"}}`)
	got := r.apns.take()
	if len(got) != 2 || got[0].event() != "update" || got[1].event() != "end" {
		t.Fatalf("pushes = %+v", got)
	}
	if got[1].num("timestamp") <= got[0].num("timestamp") {
		t.Errorf("end timestamp %d is not after the update's %d", got[1].num("timestamp"), got[0].num("timestamp"))
	}
}
