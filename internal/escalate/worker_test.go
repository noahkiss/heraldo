package escalate_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/escalate"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	writeTok = "write-token-for-tests"
	readTok  = "read-token-for-tests"
	critTok  = "critical-token-for-tests"
	appTok   = "app-token-for-tests"
	userKey  = "user-key-for-tests"
)

// fakePushover is an in-process Pushover API. It takes appTok only.
type fakePushover struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []pushoverReq
}

type pushoverReq struct {
	Path string
	Form url.Values
}

func newFakePushover(t *testing.T) *fakePushover {
	t.Helper()
	f := &fakePushover{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, pushoverReq{Path: r.URL.Path, Form: r.PostForm})
		f.mu.Unlock()
		if r.PostForm.Get("token") != appTok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":0,"errors":["application token is invalid"]}`)
			return
		}
		if r.URL.Path == "/1/messages.json" {
			fmt.Fprintf(w, `{"status":1,"request":"req%d","receipt":"rcpt%d"}`, n, n)
			return
		}
		fmt.Fprint(w, `{"status":1,"request":"cancel"}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePushover) requests() []pushoverReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushoverReq(nil), f.reqs...)
}

// on is the settings that turn escalation on with the fake's token.
var on = map[string]string{
	"HERALDO_ESCALATION_ENABLED":        "true",
	"HERALDO_ESCALATION_PUSHOVER_TOKEN": appTok,
	"HERALDO_ESCALATION_PUSHOVER_USER":  userKey,
	"HERALDO_ESCALATION_RETRY":          "10s", // below Pushover's floor: sent as 30
	"HERALDO_ESCALATION_EXPIRE":         "2h",
}

type rig struct {
	h   http.Handler
	st  *store.Store
	esc *escalate.Pushover
	p   *push.Pusher
	f   *fakePushover
}

type opts struct {
	env   map[string]string
	push  bool // a pusher with no devices
	rules string
	// detached leaves the escalator out of the server, as if the server
	// stopped before the queue reached each event.
	detached bool
}

func newRig(t *testing.T, o opts) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var rw *rules.Watcher
	if o.rules != "" {
		path := filepath.Join(t.TempDir(), "rules.yaml")
		if err := os.WriteFile(path, []byte(o.rules), 0o600); err != nil {
			t.Fatal(err)
		}
		if rw, err = rules.NewWatcher(path, nil); err != nil {
			t.Fatal(err)
		}
	}
	m, err := settings.New(ctx, settings.Options{Getenv: func(k string) string { return o.env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, f: newFakePushover(t)}
	r.esc = escalate.New(escalate.Config{Store: st, Settings: m, BaseURL: r.f.srv.URL, Logger: log, Rules: rw.Rules})
	done := make(chan struct{}, 2)
	go func() { r.esc.Run(ctx); done <- struct{}{} }()
	so := server.Options{Store: st, Logger: log, Rules: rw}
	if !o.detached {
		so.Escalator = r.esc
	}
	if o.push {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		// No device is registered, so it never calls APNs.
		r.p = push.New(push.Config{Key: key, KeyID: "KEYID00001", TeamID: "TEAMID0001", Store: st, Logger: log,
			ProductionURL: "http://127.0.0.1:1", SandboxURL: "http://127.0.0.1:1", Rules: rw.Rules})
		go func() { r.p.Run(ctx); done <- struct{}{} }()
		so.Pusher, so.PushState = r.p, api.HealthPushOn
	} else {
		done <- struct{}{}
	}
	t.Cleanup(func() { cancel(); <-done; <-done; st.Close() })
	so.Tokens, err = auth.Parse(strings.NewReader(fmt.Sprintf("sender write %s,phone read %s,pager write %s critical",
		auth.Hash(writeTok), auth.Hash(readTok), auth.Hash(critTok))), ',')
	if err != nil {
		t.Fatal(err)
	}
	if so.Validator, err = validate.New(); err != nil {
		t.Fatal(err)
	}
	r.h = server.New(so)
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

// post stores an event and waits until the escalator and the pusher are
// done with it.
func (r *rig) post(t *testing.T, token, body string) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/events", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	var ev struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	r.esc.Wait()
	if r.p != nil {
		r.p.Wait()
	}
	return ev.ID
}

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
	return tr
}

// escRow returns the trace's escalate row; ok is false when there is none.
func escRow(tr api.PushTrace) (api.PushDecision, bool) {
	for _, d := range tr.Decisions {
		if d.Stage == "escalate" {
			return d, true
		}
	}
	return api.PushDecision{}, false
}

func summary(tr api.PushTrace) string {
	if tr.Why == nil {
		return ""
	}
	return *tr.Why
}

const open = `{"source":"s","kind":"incident","key":"web","action":"open","title":"down","summary":"web is down","level":"critical"}`

// Push off and no device: the emergency escalates all the same, and an
// ack cancels it.
func TestEscalatesWithPushOff(t *testing.T) {
	r := newRig(t, opts{env: on})
	id := r.post(t, critTok, open)
	q := r.f.requests()
	if len(q) != 1 || q[0].Path != "/1/messages.json" {
		t.Fatalf("pushover requests %+v", q)
	}
	form := q[0].Form
	if form.Get("priority") != "2" || form.Get("retry") != "30" || form.Get("expire") != "7200" ||
		form.Get("title") != "down" || form.Get("message") != "web is down" || form.Get("user") != userKey {
		t.Fatalf("form %v", form)
	}
	tr := r.why(t, id)
	d, ok := escRow(tr)
	if !ok || d.Reason != "escalated to Pushover" || (*d.Detail)["receipt"] != "rcpt0" {
		t.Fatalf("trace %+v", tr)
	}
	if summary(tr) != "escalated to Pushover" {
		t.Fatalf("why %q", summary(tr))
	}
	if s := fmt.Sprint(tr); strings.Contains(s, appTok) || strings.Contains(s, userKey) {
		t.Fatal("a secret reached the trace")
	}

	// An update of the same incident does not page twice.
	up := r.post(t, critTok, `{"source":"s","kind":"incident","key":"web","action":"update","title":"still down","level":"critical"}`)
	if d, _ := escRow(r.why(t, up)); !strings.HasPrefix(d.Reason, "already escalated to Pushover by "+id) || len(r.f.requests()) != 1 {
		t.Fatalf("update: %+v", d)
	}

	ack := r.post(t, writeTok, `{"source":"phone","kind":"incident","key":"web","action":"ack"}`)
	q = r.f.requests()
	if len(q) != 2 || q[1].Path != "/1/receipts/rcpt0/cancel.json" || q[1].Form.Get("token") != appTok {
		t.Fatalf("pushover requests %+v", q)
	}
	if d, ok := escRow(r.why(t, ack)); !ok || !strings.HasPrefix(d.Reason, "cancelled the Pushover escalation of "+id+": acked by phone") {
		t.Fatalf("ack row %+v", d)
	}
	e, err := r.st.Escalation(context.Background(), id)
	if err != nil || e.CancelledAt.IsZero() || !strings.HasPrefix(e.CancelReason, "acked by phone") {
		t.Fatalf("escalation %+v %v", e, err)
	}

	// A resolve after the ack has nothing left to cancel.
	res := r.post(t, writeTok, `{"source":"phone","kind":"incident","key":"web","action":"resolve"}`)
	if _, ok := escRow(r.why(t, res)); ok || len(r.f.requests()) != 2 {
		t.Fatal("resolve cancelled twice")
	}

	// A new critical of the key pages again; its resolve cancels it.
	again := r.post(t, critTok, `{"source":"s","kind":"incident","key":"web","action":"open","title":"down again","level":"critical"}`)
	if d, _ := escRow(r.why(t, again)); d.Reason != "escalated to Pushover" {
		t.Fatalf("again: %+v", d)
	}
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"web","action":"resolve"}`)
	if q := r.f.requests(); len(q) != 4 || q[3].Path != "/1/receipts/rcpt2/cancel.json" {
		t.Fatalf("pushover requests %+v", q)
	}
}

// Push on with zero devices: the pipeline drops the push, the emergency
// still escalates, and the pipeline's trace does not erase the escalate row.
func TestEscalatesWithNoDevices(t *testing.T) {
	r := newRig(t, opts{env: on, push: true})
	id := r.post(t, critTok, open)
	if len(r.f.requests()) != 1 {
		t.Fatalf("pushover requests %+v", r.f.requests())
	}
	tr := r.why(t, id)
	d, ok := escRow(tr)
	if !ok || d.Reason != "escalated to Pushover" {
		t.Fatalf("trace %+v", tr)
	}
	if summary(tr) != "no active devices" || tr.Decisions[len(tr.Decisions)-1].Stage != "escalate" {
		t.Fatalf("why %q, trace %+v", summary(tr), tr.Decisions)
	}
}

func TestNotEscalated(t *testing.T) {
	r := newRig(t, opts{env: on, rules: `rules:
  - name: ops-emergency
    match: {tag: '^ops$'}
    set: {kind: incident, action: open, key: 'ops:site', level: critical}
`})
	cases := []struct{ name, token, body string }{
		{"time-sensitive", critTok, `{"source":"s","kind":"incident","key":"a","action":"open","title":"slow","level":"time-sensitive"}`},
		{"critical from a plain token", writeTok, `{"source":"s","kind":"incident","key":"b","action":"open","title":"down","level":"critical"}`},
		{"held", critTok, `{"source":"s","title":"down","level":"critical","expires_at":"2001-01-01T00:00:00Z"}`},
	}
	for _, c := range cases {
		id := r.post(t, c.token, c.body)
		if _, ok := escRow(r.why(t, id)); ok || len(r.f.requests()) != 0 {
			t.Fatalf("%s escalated", c.name)
		}
	}

	// A mute holds back the escalation as it holds back the push.
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"key":"c","for":"1h"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body)
	}
	id := r.post(t, critTok, `{"source":"s","kind":"incident","key":"c","action":"open","title":"down","level":"critical"}`)
	if d, _ := escRow(r.why(t, id)); !strings.HasPrefix(d.Reason, "not escalated: muted by ") || d.Outcome != "drop" ||
		len(r.f.requests()) != 0 {
		t.Fatalf("muted: %+v", d)
	}

	// A rule's critical escalates from a plain token.
	r.post(t, writeTok, `{"source":"s","title":"fire","tags":["ops"]}`)
	if len(r.f.requests()) != 1 {
		t.Fatalf("rule critical: %+v", r.f.requests())
	}
}

func TestEscalationOffOrUnset(t *testing.T) {
	r := newRig(t, opts{})
	id := r.post(t, critTok, open)
	if d, _ := escRow(r.why(t, id)); d.Reason != "escalation off" || len(r.f.requests()) != 0 {
		t.Fatalf("off: %+v", d)
	}
	r = newRig(t, opts{env: map[string]string{"HERALDO_ESCALATION_ENABLED": "true"}})
	id = r.post(t, critTok, open)
	if d, _ := escRow(r.why(t, id)); !strings.Contains(d.Reason, "is not set") || len(r.f.requests()) != 0 {
		t.Fatalf("no keys: %+v", d)
	}
}

// A Pushover that refuses the token: the trace says so without the token.
func TestEscalationFails(t *testing.T) {
	env := map[string]string{}
	for k, v := range on {
		env[k] = v
	}
	env["HERALDO_ESCALATION_PUSHOVER_TOKEN"] = "wrong-token-for-tests"
	r := newRig(t, opts{env: env})
	id := r.post(t, critTok, open)
	d, _ := escRow(r.why(t, id))
	if d.Reason != "escalation to Pushover failed: application token is invalid" || strings.Contains(fmt.Sprint(d), "wrong-token") {
		t.Fatalf("failed: %+v", d)
	}
}

// The start-up sweep escalates the critical events no escalation saw, once,
// and skips those acked or resolved since, those nobody allowed, and those
// outside the window.
func TestSweep(t *testing.T) {
	r := newRig(t, opts{env: on, detached: true})
	a := r.post(t, critTok, `{"source":"s","kind":"incident","key":"a","action":"open","title":"a down","level":"critical"}`)
	r.post(t, critTok, `{"source":"s","kind":"incident","key":"b","action":"open","title":"b down","level":"critical"}`)
	r.post(t, writeTok, `{"source":"phone","kind":"incident","key":"b","action":"ack"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"c","action":"open","title":"c down","level":"critical"}`)
	nokey := r.post(t, critTok, `{"source":"s","title":"no key","level":"critical"}`)
	if len(r.f.requests()) != 0 {
		t.Fatal("escalated before the sweep")
	}
	ctx := context.Background()
	if n, err := r.esc.Sweep(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("outside the window: %d %v", n, err)
	}
	n, err := r.esc.Sweep(ctx, time.Now().Add(-escalate.SweepWindow))
	if err != nil || n != 2 {
		t.Fatalf("sweep queued %d, %v; want a and the event without a key", n, err)
	}
	r.esc.Wait()
	q := r.f.requests()
	if len(q) != 2 || q[0].Form.Get("title") != "a down" || q[1].Form.Get("title") != "no key" {
		t.Fatalf("pushover requests %+v", q)
	}
	for _, id := range []string{a, nokey} {
		if d, ok := escRow(r.why(t, id)); !ok || !strings.HasPrefix(d.Reason, "escalated to Pushover") {
			t.Fatalf("%s: %+v", id, d)
		}
	}
	// Once: a second start finds nothing left.
	if n, err := r.esc.Sweep(ctx, time.Now().Add(-escalate.SweepWindow)); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
}

// An event the escalator already decided, here with escalation off, is
// not swept again when escalation is turned on later.
func TestSweepSkipsDecided(t *testing.T) {
	r := newRig(t, opts{})
	r.post(t, critTok, open)
	if n, err := r.esc.Sweep(context.Background(), time.Now().Add(-escalate.SweepWindow)); err != nil || n != 0 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}
