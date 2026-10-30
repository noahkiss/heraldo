package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	writeTok  = "write-token-for-tests"
	readTok   = "read-token-for-tests"
	bridgeTok = "bridge-token-for-tests"
	critTok   = "critical-token-for-tests"
	topic     = "org.example.heraldo"
	// Hand-made device tokens: 64 hex characters each.
	prodToken    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sandboxToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeReq is one request the fake APNs got.
type fakeReq struct {
	Path, Topic, PushType, Priority, CollapseID, Expiration, Auth string
	Method, Channel                                               string
	Proto                                                         int
	Body                                                          []byte
}

// fakeAPNs is an in-process APNs: HTTP/2 over TLS. reply picks each answer;
// nil answers 200.
type fakeAPNs struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []fakeReq
	reply func(n int, r fakeReq) (int, string)
}

func newFake(t *testing.T) *fakeAPNs {
	t.Helper()
	f := &fakeAPNs{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := fakeReq{
			Path: r.URL.Path, Topic: r.Header.Get("apns-topic"), PushType: r.Header.Get("apns-push-type"),
			Priority: r.Header.Get("apns-priority"), CollapseID: r.Header.Get("apns-collapse-id"),
			Expiration: r.Header.Get("apns-expiration"), Auth: r.Header.Get("authorization"),
			Method: r.Method, Channel: r.Header.Get("apns-channel-id"),
			Proto: r.ProtoMajor, Body: body,
		}
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, req)
		reply := f.reply
		f.mu.Unlock()
		status, reason := http.StatusOK, ""
		if reply != nil {
			status, reason = reply(n, req)
		}
		w.Header().Set("apns-id", fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/channels") {
			w.Header().Set("apns-channel-id", fmt.Sprintf("Y2hhbm5lbC0%d==", n))
		}
		w.WriteHeader(status)
		if reason != "" {
			fmt.Fprintf(w, `{"reason":%q}`, reason)
		}
	}))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPNs) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

func (f *fakeAPNs) setReply(r func(n int, req fakeReq) (int, string)) {
	f.mu.Lock()
	f.reply = r
	f.mu.Unlock()
}

type rig struct {
	h          http.Handler
	st         *store.Store
	p          *Pusher
	key        *ecdsa.PrivateKey
	prod, sand *fakeAPNs
	clock      *clock
	// prefs is the settings store the pusher and the server share.
	prefs *settings.Manager
}

// set writes the setting name, as JSON, as PUT /v1/settings/{key} does.
func (r *rig) set(t *testing.T, name, raw string) {
	t.Helper()
	if _, err := r.prefs.Set(context.Background(), name, json.RawMessage(raw), "test"); err != nil {
		t.Fatalf("set %s: %v", name, err)
	}
}

// clock is a test clock that only moves when told.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newRig(t *testing.T, queue int) *rig {
	t.Helper()
	return newRigRules(t, queue, "")
}

// newRigRules is newRig with a rules file; "" means no rules.
func newRigRules(t *testing.T, queue int, rulesYAML string) *rig {
	t.Helper()
	var rw *rules.Watcher
	if rulesYAML != "" {
		path := filepath.Join(t.TempDir(), "rules.yaml")
		if err := os.WriteFile(path, []byte(rulesYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if rw, err = rules.NewWatcher(path, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, prod: newFake(t), sand: newFake(t), clock: &clock{t: time.Now().UTC().Truncate(time.Second)}}
	st.SetClock(r.clock.now)
	// As heraldo-server does when push goes on.
	if _, err := st.StartCursor(ctx, BadgeCursor, "test"); err != nil {
		t.Fatal(err)
	}
	r.key, _ = newKey(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if r.prefs, err = settings.New(ctx, settings.Options{Backend: st, Now: r.clock.now, Logger: log}); err != nil {
		t.Fatal(err)
	}
	r.p = New(Config{
		Settings: r.prefs,
		Key:      r.key, KeyID: "KEYID00001", TeamID: "TEAMID0001", Store: st, Logger: log,
		ProductionURL: r.prod.srv.URL, SandboxURL: r.sand.srv.URL, HTTPClient: r.prod.srv.Client(),
		Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Queue: queue,
		Now: r.clock.now, Rules: rw.Rules, RealertTick: time.Hour,
	})
	done := make(chan struct{})
	go func() { r.p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf("sender write %s,phone read %s,bridge write %s trusted-ingest,pager write %s critical",
		auth.Hash(writeTok), auth.Hash(readTok), auth.Hash(bridgeTok), auth.Hash(critTok))), ',')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	r.h = server.New(server.Options{Store: st, Tokens: tokens, Validator: v, Logger: log, Pusher: r.p,
		PushState: api.HealthPushOn, Now: r.clock.now, Rules: rw, Settings: r.prefs})
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

func (r *rig) register(t *testing.T, token, env string) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/devices", readTok,
		fmt.Sprintf(`{"token":%q,"environment":%q,"topic":%q,"name":"Test phone"}`, token, env, topic))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	var d api.Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	return d.Id
}

// post stores an event and waits until its push is done.
func (r *rig) post(t *testing.T, token, body string) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/events", token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	var ev struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	r.p.Wait()
	return ev.ID
}

func (r *rig) delivery(t *testing.T, id string) []api.Delivery {
	t.Helper()
	rec := r.do(t, http.MethodGet, "/v1/events/"+id, readTok, "")
	var ev api.Event
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Delivery == nil {
		return nil
	}
	return *ev.Delivery
}

func TestPushEndToEnd(t *testing.T) {
	r := newRig(t, 0)
	prodID := r.register(t, prodToken, "production")
	sandID := r.register(t, sandboxToken, "sandbox")
	id := r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"unit:web-1:nginx","action":"open",
		"title":"Web is down","body":"nginx failed","level":"critical"}`)

	pr, sr := r.prod.requests(), r.sand.requests()
	if len(pr) != 1 || len(sr) != 1 {
		t.Fatalf("production got %d, sandbox got %d; want 1 each", len(pr), len(sr))
	}
	if pr[0].Path != "/3/device/"+prodToken || sr[0].Path != "/3/device/"+sandboxToken {
		t.Fatalf("paths %q %q", pr[0].Path, sr[0].Path)
	}
	q := pr[0]
	if q.Proto != 2 || q.Topic != topic || q.PushType != "alert" || q.Priority != "10" ||
		q.CollapseID != CollapseID("unit:web-1:nginx") || q.Expiration != "" {
		t.Fatalf("request %+v", q)
	}
	jwt, ok := strings.CutPrefix(q.Auth, "bearer ")
	if !ok {
		t.Fatalf("authorization %q", q.Auth)
	}
	if _, c, err := verifyES256(&r.key.PublicKey, jwt); err != nil || c["iss"] != "TEAMID0001" {
		t.Fatalf("provider token: %v %v", c, err)
	}
	if sr[0].Auth != q.Auth {
		t.Fatal("provider token not cached across pushes")
	}
	var p Payload
	if err := json.Unmarshal(q.Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Heraldo.ID != id || p.APS.InterruptionLevel != "time-sensitive" || p.APS.Badge == nil || *p.APS.Badge != 1 {
		t.Fatalf("payload %s", q.Body)
	}

	ds := r.delivery(t, id)
	if len(ds) != 2 {
		t.Fatalf("delivery %+v", ds)
	}
	for _, d := range ds {
		if (d.Device != prodID && d.Device != sandID) || d.Outcome != api.DeliveryOutcomeSent ||
			d.Level == nil || *d.Level != api.LevelTimeSensitive || d.Error == nil {
			t.Errorf("delivery %+v", d)
		}
	}

	// The badge follows the shared inbox cursor.
	rec := r.do(t, http.MethodGet, "/v1/events?limit=1", readTok, "")
	var page api.EventPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec := r.do(t, http.MethodPut, "/v1/cursors/inbox", readTok, `{"cursor":"`+*page.NextCursor+`"}`); !strings.Contains(rec.Body.String(), `"unread":0`) {
		t.Fatalf("cursor answer %s", rec.Body)
	}
	r.post(t, writeTok, `{"source":"s","title":"next"}`)
	var p2 Payload
	_ = json.Unmarshal(r.prod.requests()[1].Body, &p2)
	if *p2.APS.Badge != 1 {
		t.Fatalf("badge after read = %d, want 1", *p2.APS.Badge)
	}
}

func TestPushSkips(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	for _, body := range []string{
		`{"source":"s","kind":"incident","key":"k","action":"ack"}`,
		`{"source":"s","kind":"progress","key":"p","action":"start"}`,
		`{"source":"s","kind":"checkin","key":"c","action":"success"}`,
		`{"source":"s","kind":"message","expires_at":"2000-01-01T00:00:00Z"}`,
	} {
		r.post(t, writeTok, body)
	}
	// A bridged event the tee held is stored only.
	r.post(t, bridgeTok, `{"source":"s","title":"held","details":{"tee":{"hold":{"state":"held","id":"h1"}}}}`)
	if n := len(r.prod.requests()); n != 0 {
		t.Fatalf("%d pushes, want 0", n)
	}
}

// TestBridgedHoldStates sends each hold shape the apprise tee sends and
// checks which push. Only an alert the hold kept back is stored only.
func TestBridgedHoldStates(t *testing.T) {
	for _, c := range []struct {
		name, hold string
		push       bool
	}{
		{"held on", `{"id":"h-1","state":"held","forwarded":false,"store_only":true}`, false},
		{"held shadow", `{"id":"h-2","state":"held","forwarded":true,"shadow":true}`, true},
		{"held dead-man", `{"id":"h-3","state":"held","forwarded":true,"shadow":false}`, true},
		{"bypass", `{"state":"bypass"}`, true},
		{"release forward", `{"ids":["h-4","h-5"],"forward":"release"}`, true},
		{"deadline forward", `{"ids":["h-6"],"forward":"deadline"}`, true},
		{"dead-man forward", `{"ids":["h-7"],"forward":"deadman"}`, true},
		{"dead-man notice", `{"ids":[],"forward":"deadman-notice"}`, true},
		// A sender's store_only on a forward does not stop it.
		{"forward marked store_only", `{"ids":["h-8"],"forward":"release","store_only":true}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, 0)
			r.register(t, prodToken, "production")
			id := r.post(t, bridgeTok, `{"source":"s","title":"disk full","tags":["alert"],"level":"time-sensitive",
				"details":{"tee":{"id":"n-1","hold":`+c.hold+`}}}`)
			if got := len(r.prod.requests()) == 1; got != c.push {
				t.Fatalf("pushed %v, want %v", got, c.push)
			}
			ds := r.delivery(t, id)
			if c.push && (len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSent || *ds[0].Level != api.LevelTimeSensitive) {
				t.Fatalf("delivery %+v", ds)
			}
			rec, err := r.st.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			var ev api.Event
			_ = json.Unmarshal(rec.Event, &ev)
			if ev.Hold == nil {
				t.Fatalf("no hold: %s", rec.Event)
			}
			if so, _ := (*ev.Hold)["store_only"].(bool); so == c.push {
				t.Fatalf("store_only %v: %s", so, rec.Event)
			}
			if got, err := r.st.Pushes(context.Background(), rec.Seq); err != nil || got != c.push {
				t.Fatalf("badge rule %v %v, want %v", got, err, c.push)
			}
		})
	}
}

func TestPushRetiresDeadTokens(t *testing.T) {
	for _, c := range []struct {
		status int
		reason string
	}{
		{http.StatusGone, "Unregistered"},
		{http.StatusBadRequest, "BadDeviceToken"},
		{http.StatusBadRequest, "DeviceTokenNotForTopic"},
	} {
		t.Run(c.reason, func(t *testing.T) {
			r := newRig(t, 0)
			dev := r.register(t, prodToken, "production")
			r.prod.setReply(func(int, fakeReq) (int, string) { return c.status, c.reason })
			id := r.post(t, writeTok, `{"source":"s","title":"one"}`)
			ds := r.delivery(t, id)
			if len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeFailed || *ds[0].Error != c.reason {
				t.Fatalf("delivery %+v", ds)
			}
			d, err := r.st.GetDevice(context.Background(), dev)
			if err != nil || d.Active() || d.RetiredReason != "apns: "+c.reason {
				t.Fatalf("device %+v %v", d, err)
			}
			r.post(t, writeTok, `{"source":"s","title":"two"}`)
			if n := len(r.prod.requests()); n != 1 {
				t.Fatalf("%d requests; a retired device got a push", n)
			}
			// The app registers the same token again: it is back.
			r.register200(t, prodToken)
			r.prod.setReply(nil)
			r.post(t, writeTok, `{"source":"s","title":"three"}`)
			if n := len(r.prod.requests()); n != 2 {
				t.Fatalf("%d requests after re-registering", n)
			}
		})
	}
}

func (r *rig) register200(t *testing.T, token string) {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/devices", readTok,
		fmt.Sprintf(`{"token":%q,"environment":"production","topic":%q}`, token, topic))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":true`) {
		t.Fatalf("re-register: %d %s", rec.Code, rec.Body)
	}
}

func TestPushRetries(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	// 429, then 503, then accepted.
	r.prod.setReply(func(n int, _ fakeReq) (int, string) {
		switch n {
		case 0:
			return http.StatusTooManyRequests, "TooManyRequests"
		case 1:
			return http.StatusServiceUnavailable, "ServiceUnavailable"
		}
		return http.StatusOK, ""
	})
	id := r.post(t, writeTok, `{"source":"s","title":"one"}`)
	ds := r.delivery(t, id)
	if len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSent {
		t.Fatalf("delivery %+v", ds)
	}
	if n := len(r.prod.requests()); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}

	// Always failing: give up after the last backoff step.
	r.prod.setReply(func(int, fakeReq) (int, string) { return http.StatusInternalServerError, "InternalServerError" })
	id = r.post(t, writeTok, `{"source":"s","title":"two"}`)
	ds = r.delivery(t, id)
	if len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeFailed || !strings.Contains(*ds[0].Error, "InternalServerError") {
		t.Fatalf("delivery %+v", ds)
	}
	if n := len(r.prod.requests()); n != 3+3 {
		t.Fatalf("%d requests, want 6", n)
	}
	var att int
	if err := rowAttempts(r.st, id, &att); err != nil || att != 3 {
		t.Fatalf("attempts %d %v", att, err)
	}

	// Another 4xx is final, not retried.
	r.prod.setReply(func(int, fakeReq) (int, string) { return http.StatusBadRequest, "PayloadTooLarge" })
	r.post(t, writeTok, `{"source":"s","title":"three"}`)
	if n := len(r.prod.requests()); n != 7 {
		t.Fatalf("%d requests, want 7", n)
	}
}

func TestPushRefreshesProviderToken(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.prod.setReply(func(n int, _ fakeReq) (int, string) {
		if n == 0 {
			return http.StatusForbidden, "ExpiredProviderToken"
		}
		return http.StatusOK, ""
	})
	// The first token is signed now; Apple calls it stale a second later.
	r.p.sign.now = func() time.Time { return time.Now().Add(-time.Second) }
	if _, _, err := r.p.sign.get(time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.p.sign.now = time.Now
	id := r.post(t, writeTok, `{"source":"s","title":"one"}`)
	reqs := r.prod.requests()
	if len(reqs) != 2 || reqs[0].Auth == reqs[1].Auth {
		t.Fatalf("%d requests; second must carry a new token", len(reqs))
	}
	if ds := r.delivery(t, id); len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSent {
		t.Fatalf("delivery %+v", ds)
	}
}

// TestIngestDoesNotWait posts while APNs hangs: every post answers at once,
// and a full queue drops pushes, not events.
func TestIngestDoesNotWait(t *testing.T) {
	r := newRig(t, 2)
	r.register(t, prodToken, "production")
	release := make(chan struct{})
	r.prod.setReply(func(int, fakeReq) (int, string) { <-release; return http.StatusOK, "" })
	start := time.Now()
	for i := range 20 {
		rec := r.do(t, http.MethodPost, "/v1/events", writeTok, fmt.Sprintf(`{"source":"s","title":"n%d"}`, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("post %d: %d", i, rec.Code)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("20 posts took %v with APNs hanging", d)
	}
	close(release)
	r.p.Wait()
	if n, _ := r.st.Count(context.Background()); n != 20 {
		t.Fatalf("%d events stored", n)
	}
	if n := len(r.prod.requests()); n >= 20 || n == 0 {
		t.Fatalf("%d pushes; the queue of 2 behind 4 workers should drop some", n)
	}
}

func rowAttempts(st *store.Store, eventID string, n *int) error {
	ds, err := st.DeliveryAttempts(context.Background(), eventID)
	if err != nil {
		return err
	}
	if len(ds) != 1 {
		return fmt.Errorf("%d deliveries", len(ds))
	}
	*n = ds[0]
	return nil
}

// TestBadgeRuleMatchesDecide stores every push-table case and checks the
// store's SQL rule against Decide.
func TestBadgeRuleMatchesDecide(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range decideCases {
		var fields map[string]any
		dec := json.NewDecoder(bytes.NewReader([]byte(c.ev)))
		dec.UseNumber()
		if err := dec.Decode(&fields); err != nil {
			t.Fatal(err)
		}
		var typed api.EventInput
		_ = json.Unmarshal([]byte(c.ev), &typed)
		id := ids.New(testNow)
		fields["id"], fields["received_at"], fields["ts"] = id, testNow.Format(time.RFC3339), testNow.Format(time.RFC3339)
		rec, _, err := st.Insert(ctx, store.NewEvent{ID: id, ReceivedAt: testNow, Fields: fields, Typed: typed})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := st.Pushes(ctx, rec.Seq)
		if err != nil {
			t.Fatal(err)
		}
		if want := c.push && !c.replace; got != want {
			t.Errorf("%s: SQL says badge %v, Decide says %v", c.name, got, want)
		}
	}
}

func payloadOf(t *testing.T, q fakeReq) Payload {
	t.Helper()
	var p Payload
	if err := json.Unmarshal(q.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestResolveReplacesOpen is the lock-screen acceptance case: a failure and
// its recovery go out under one collapse id, and the recovery says how long
// the incident was open.
func TestResolveReplacesOpen(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"unit:web-1:nginx","action":"open",
		"title":"Web is down","summary":"nginx failed","level":"time-sensitive"}`)
	r.clock.add(12*time.Minute + 10*time.Second)
	r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"unit:web-1:nginx","action":"resolve","title":"Web is up"}`)
	q := r.prod.requests()
	if len(q) != 2 || q[0].CollapseID == "" || q[0].CollapseID != q[1].CollapseID || q[1].Priority != "5" {
		t.Fatalf("requests %+v", q)
	}
	if p := payloadOf(t, q[1]); p.APS.Alert.Body != "Resolved after 12 min" || p.APS.InterruptionLevel != "passive" || p.APS.Sound != "" {
		t.Fatalf("resolve payload %s", q[1].Body)
	}
}

// TestPassiveIsInboxOnly: a passive event pushes nothing and does not count
// in the badge, unless it replaces a visible push of its key.
func TestPassiveIsInboxOnly(t *testing.T) {
	r := newRig(t, 0)
	r.set(t, "badge.scope", `"pushed"`)
	r.register(t, prodToken, "production")
	r.post(t, writeTok, `{"source":"s","title":"quiet","level":"passive"}`)
	r.post(t, writeTok, `{"source":"s","kind":"digest","title":"nightly digest"}`)
	// An incident that opened passive never showed, so its resolve has
	// nothing to replace.
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"quiet","action":"open","level":"passive"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"quiet","action":"resolve"}`)
	if n := len(r.prod.requests()); n != 0 {
		t.Fatalf("%d pushes, want 0", n)
	}
	// A visible open, then its resolve: both push, and the resolve does not
	// raise the badge.
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"loud","action":"open","title":"down"}`)
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"loud","action":"resolve"}`)
	q := r.prod.requests()
	if len(q) != 2 || q[0].CollapseID != q[1].CollapseID {
		t.Fatalf("requests %+v", q)
	}
	if p := payloadOf(t, q[1]); p.APS.Badge == nil || *p.APS.Badge != 1 {
		t.Fatalf("badge %v, want 1", p.APS.Badge)
	}
	// A second resolve replaces nothing: the last one already did.
	r.post(t, writeTok, `{"source":"s","kind":"incident","key":"loud","action":"resolve"}`)
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("%d pushes after a second resolve, want 2", n)
	}
}

func TestMuteHoldsBackPush(t *testing.T) {
	r := newRig(t, 0)
	r.set(t, "badge.scope", `"pushed"`)
	dev := r.register(t, prodToken, "production")
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"source":"backup","for":"1h"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body)
	}
	muted := r.post(t, writeTok, `{"source":"backup","title":"nightly ran"}`)
	r.post(t, writeTok, `{"source":"other","title":"not muted"}`)
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes, want 1", n)
	}
	ds := r.delivery(t, muted)
	if len(ds) != 1 || ds[0].Device != dev || ds[0].Outcome != api.DeliveryOutcomeSkipped || ds[0].Error == nil ||
		!strings.HasPrefix(*ds[0].Error, "muted by mute_") {
		t.Fatalf("delivery %+v", ds)
	}
	// The muted event does not count in the badge; the other one does.
	if p := payloadOf(t, r.prod.requests()[0]); p.APS.Badge == nil || *p.APS.Badge != 1 {
		t.Fatalf("badge %v", p.APS.Badge)
	}
	// After the hour, the source pushes again.
	r.clock.add(time.Hour)
	r.post(t, writeTok, `{"source":"backup","title":"nightly ran again"}`)
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("%d pushes after expiry, want 2", n)
	}
}

// One mute on checkin:<slug> holds back the check's incident and its own
// failed runs, and no other check.
func TestMuteCoversCheckinRuns(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"key":"checkin:nightly","for":"1h"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body)
	}
	muted := r.post(t, writeTok, `{"source":"checkin","kind":"checkin","key":"nightly","action":"fail","title":"nightly failed"}`)
	r.post(t, writeTok, `{"source":"checkin","kind":"checkin","key":"weekly","action":"fail","title":"weekly failed"}`)
	r.post(t, writeTok, `{"source":"heraldo","kind":"incident","key":"checkin:nightly","action":"open","title":"nightly is down"}`)
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes, want 1 (weekly)", n)
	}
	if ds := r.delivery(t, muted); len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSkipped {
		t.Fatalf("delivery %+v", ds)
	}
}

func TestInhibitHoldsBackChildren(t *testing.T) {
	r := newRigRules(t, 0, `inhibit:
  - name: host-down
    parent: '^host:(?P<host>[\w.-]+)$'
    children: '^(unit|container):{host}:'
`)
	r.register(t, prodToken, "production")
	r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"open","title":"web-1 is down"}`)
	child := r.post(t, writeTok, `{"source":"units","kind":"incident","key":"unit:web-1:nginx","action":"open","title":"nginx failed"}`)
	r.post(t, writeTok, `{"source":"units","kind":"incident","key":"unit:web-2:nginx","action":"open","title":"other host"}`)
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("%d pushes, want the parent and the other host", n)
	}
	if ds := r.delivery(t, child); len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSkipped ||
		!strings.Contains(*ds[0].Error, "host-down") {
		t.Fatalf("child delivery %+v", ds)
	}
	r.post(t, writeTok, `{"source":"uptime","kind":"incident","key":"host:web-1","action":"resolve"}`)
	r.post(t, writeTok, `{"source":"units","kind":"incident","key":"unit:web-1:nginx","action":"update","title":"nginx still failed"}`)
	if n := len(r.prod.requests()); n != 4 {
		t.Fatalf("%d pushes after the parent resolved, want 4", n)
	}
}

func TestRealert(t *testing.T) {
	r := newRigRules(t, 0, `rules:
  - name: down
    match: {title: '^(?P<h>[\w-]+) is down'}
    set: {kind: incident, action: open, key: 'host:{h}', level: time-sensitive, realert: 10m}
`)
	r.register(t, prodToken, "production")
	ctx := context.Background()
	r.post(t, writeTok, `{"source":"uptime","title":"web-1 is down","body":"no answer"}`)
	realert := func() {
		r.p.Realert(ctx)
		r.p.Wait()
	}
	realert()
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes before the interval", n)
	}
	r.clock.add(10 * time.Minute)
	realert()
	q := r.prod.requests()
	if len(q) != 2 || q[1].CollapseID != q[0].CollapseID {
		t.Fatalf("requests %+v", q)
	}
	if p := payloadOf(t, q[1]); p.APS.Alert.Body != "Still open after 10 min. no answer" || p.APS.InterruptionLevel != "time-sensitive" {
		t.Fatalf("re-alert payload %s", q[1].Body)
	}
	// A server that slept through three intervals sends one re-alert.
	r.clock.add(35 * time.Minute)
	realert()
	realert()
	if n := len(r.prod.requests()); n != 3 {
		t.Fatalf("%d pushes after a long gap, want 3", n)
	}
	// A mute holds re-alerts back; an ack ends them.
	if rec := r.do(t, http.MethodPost, "/v1/mutes", writeTok, `{"key":"host:web-1","for":"15m"}`); rec.Code != http.StatusCreated {
		t.Fatalf("mute: %d", rec.Code)
	}
	r.clock.add(10 * time.Minute)
	realert()
	if n := len(r.prod.requests()); n != 3 {
		t.Fatalf("%d pushes while muted, want 3", n)
	}
	r.post(t, writeTok, `{"source":"heraldo-app","kind":"incident","key":"host:web-1","action":"ack"}`)
	r.clock.add(time.Hour)
	realert()
	if n := len(r.prod.requests()); n != 3 {
		t.Fatalf("%d pushes after the ack, want 3", n)
	}
}

func TestIconInPayload(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)
	if err := func() error {
		_, err := r.st.PutIcon(context.Background(), &store.Icon{Name: "backup", ContentType: "image/png", Data: png, UpdatedBy: "t"})
		return err
	}(); err != nil {
		t.Fatal(err)
	}
	r.post(t, writeTok, `{"source":"backup","title":"by source"}`)
	r.post(t, writeTok, `{"source":"other","icon":"backup","title":"by name"}`)
	r.post(t, writeTok, `{"source":"other","icon":"gear","title":"no such icon"}`)
	q := r.prod.requests()
	for i, want := range []string{"backup", "backup", ""} {
		if p := payloadOf(t, q[i]); p.Heraldo.Icon != want {
			t.Errorf("push %d: icon %q, want %q", i, p.Heraldo.Icon, want)
		}
	}
}

// TestPingRuleHoldsFail: a ping rule that holds a check's failures stores
// the failed run held and pushes nothing; a ping no rule matches pushes.
func TestPingRuleHoldsFail(t *testing.T) {
	r := newRigRules(t, 0, `rules:
  - name: backup-triage
    match: {kind: '^checkin$', key: '^backup$', action: '^fail$'}
    set: {hold: true, thread: triage}
`)
	r.register(t, prodToken, "production")
	ping := func(path string) {
		t.Helper()
		if rec := r.do(t, http.MethodGet, path, writeTok, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		r.p.Wait()
	}
	ping("/ping/backup/1?create=1")
	ping("/ping/other/1?create=1")
	if n := len(r.prod.requests()); n != 1 {
		t.Fatalf("%d pushes, want 1 (other)", n)
	}
	rec := r.do(t, http.MethodGet, "/v1/events?kind=checkin&key=backup", readTok, "")
	var p struct{ Events []api.Event }
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 1 {
		t.Fatalf("events %s", rec.Body)
	}
	ev := p.Events[0]
	if ev.Hold == nil || (*ev.Hold)["state"] != "held" || (*ev.Hold)["store_only"] != true ||
		ev.Thread == nil || *ev.Thread != "triage" || ev.Kind != api.KindCheckin || string(*ev.Action) != "fail" {
		t.Fatalf("held run %s", rec.Body)
	}
	// The hold stops the push only: the check still went down.
	rec = r.do(t, http.MethodGet, "/v1/checkins/backup", readTok, "")
	var c api.Check
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if c.State != api.CheckStateDown {
		t.Fatalf("check %s", rec.Body)
	}
}

// insertQuiet stores an event the way the server does, but queues no push:
// an event the server accepted and then stopped before pushing.
func (r *rig) insertQuiet(t *testing.T, js string, imported bool) string {
	t.Helper()
	var fields map[string]any
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["kind"]; !ok {
		fields["kind"] = "message"
	}
	var typed api.EventInput
	_ = json.Unmarshal([]byte(js), &typed)
	if typed.Kind == nil {
		k := api.KindMessage
		typed.Kind = &k
	}
	now := r.clock.now()
	id := ids.New(now)
	fields["id"], fields["received_at"], fields["ts"] = id, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
	ev := store.NewEvent{ID: id, ReceivedAt: now, Fields: fields, Typed: typed}
	if imported {
		if _, err := r.st.Import(context.Background(), "tee", id, ev); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, _, err := r.st.Insert(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSweepPushesUnpushed(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	old := r.insertQuiet(t, `{"source":"s","title":"before the window"}`, false)
	r.clock.add(2 * time.Hour)
	since := r.clock.now().Add(-time.Hour)
	pushed := r.post(t, writeTok, `{"source":"s","title":"pushed already"}`)
	lost := r.insertQuiet(t, `{"source":"s","title":"lost at shutdown"}`, false)
	r.insertQuiet(t, `{"source":"s","title":"imported","dedup":"n-1"}`, true)
	r.insertQuiet(t, `{"source":"s","title":"held","hold":{"state":"held","store_only":true}}`, false)
	r.insertQuiet(t, `{"source":"s","kind":"checkin","key":"k","action":"success"}`, false)
	n, err := r.p.Sweep(ctx, since)
	r.p.Wait()
	if err != nil || n != 2 {
		// lost, and the check-in success, which the push table then skips.
		t.Fatalf("swept %d, %v; want 2", n, err)
	}
	if got := len(r.prod.requests()); got != 2 {
		t.Fatalf("%d pushes, want 2 (pushed, lost)", got)
	}
	for _, id := range []string{pushed, lost} {
		if ds := r.delivery(t, id); len(ds) != 1 || ds[0].Outcome != api.DeliveryOutcomeSent {
			t.Fatalf("%s: delivery %+v", id, ds)
		}
	}
	if ds := r.delivery(t, old); len(ds) != 0 {
		t.Fatalf("old event pushed: %+v", ds)
	}
	// A second sweep finds nothing new to push.
	if _, err := r.p.Sweep(ctx, since); err != nil {
		t.Fatal(err)
	}
	r.p.Wait()
	if got := len(r.prod.requests()); got != 2 {
		t.Fatalf("%d pushes after a second sweep", got)
	}
}

func TestDrain(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	release := make(chan struct{})
	r.prod.setReply(func(int, fakeReq) (int, string) { <-release; return http.StatusOK, "" })
	r.do(t, http.MethodPost, "/v1/events", writeTok, `{"source":"s","title":"queued"}`)
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if r.p.Drain(short) {
		t.Fatal("drained with a push in flight")
	}
	close(release)
	if !r.p.Drain(context.Background()) || len(r.prod.requests()) != 1 {
		t.Fatalf("drain: %d pushes", len(r.prod.requests()))
	}
}

func TestHealthUnsent(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	unsent := func() int {
		t.Helper()
		var h api.Health
		_ = json.Unmarshal(r.do(t, http.MethodGet, "/healthz", "", "").Body.Bytes(), &h)
		if h.Unsent == nil {
			t.Fatal("no unsent with push on")
		}
		return *h.Unsent
	}
	r.post(t, writeTok, `{"source":"s","title":"sent"}`)
	r.prod.setReply(func(int, fakeReq) (int, string) { return http.StatusInternalServerError, "InternalServerError" })
	r.post(t, writeTok, `{"source":"s","title":"refused"}`)
	r.post(t, writeTok, `{"source":"s","title":"quiet","level":"passive"}`)
	r.insertQuiet(t, `{"source":"s","title":"never pushed"}`, false)
	r.insertQuiet(t, `{"source":"s","title":"imported","dedup":"n-1"}`, true)
	if n := unsent(); n != 0 {
		t.Fatalf("unsent %d inside the first 15 minutes", n)
	}
	r.clock.add(server.UnsentAfter)
	if n := unsent(); n != 2 {
		t.Fatalf("unsent %d, want 2 (refused, never pushed)", n)
	}
	r.clock.add(24 * time.Hour)
	if n := unsent(); n != 0 {
		t.Fatalf("unsent %d after the window", n)
	}
}
