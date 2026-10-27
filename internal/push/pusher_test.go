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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	writeTok  = "write-token-for-tests"
	readTok   = "read-token-for-tests"
	bridgeTok = "bridge-token-for-tests"
	topic     = "org.example.heraldo"
	// Hand-made device tokens: 64 hex characters each.
	prodToken    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sandboxToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeReq is one request the fake APNs got.
type fakeReq struct {
	Path, Topic, PushType, Priority, CollapseID, Expiration, Auth string
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
}

func newRig(t *testing.T, queue int) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, prod: newFake(t), sand: newFake(t)}
	r.key, _ = newKey(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.p = New(Config{
		Key: r.key, KeyID: "KEYID00001", TeamID: "TEAMID0001", Store: st, Logger: log,
		ProductionURL: r.prod.srv.URL, SandboxURL: r.sand.srv.URL, HTTPClient: r.prod.srv.Client(),
		Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Queue: queue,
	})
	done := make(chan struct{})
	go func() { r.p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf("sender write %s,phone read %s,bridge write %s trusted-ingest",
		auth.Hash(writeTok), auth.Hash(readTok), auth.Hash(bridgeTok))), ',')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	r.h = server.New(server.Options{Store: st, Tokens: tokens, Validator: v, Logger: log, Pusher: r.p, PushState: api.HealthPushOn})
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
		if got != c.push {
			t.Errorf("%s: SQL says push %v, Decide says %v", c.name, got, c.push)
		}
	}
}
