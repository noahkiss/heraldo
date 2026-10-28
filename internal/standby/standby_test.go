package standby_test

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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/standby"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	phoneTok   = "phone-read-token-for-tests"
	senderTok  = "sender-write-token-for-tests"
	standbyTok = "standby-token-for-tests"
	topic      = "org.example.heraldo"
	devToken   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// clock is a test clock that moves only when told.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeAPNs counts the pushes it gets, by device path.
type fakeAPNs struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newFakeAPNs(t *testing.T) *fakeAPNs {
	f := &fakeAPNs{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
	}))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPNs) pushes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// node is one heraldo server: a store, a pusher on its own fake APNs, and
// an HTTP server that can go down.
type node struct {
	st   *store.Store
	h    *server.Server
	p    *push.Pusher
	apns *fakeAPNs
	srv  *httptest.Server
	down atomic.Bool
}

func newNode(t *testing.T, c *clock, tokens string) *node {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetClock(c.now)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := &node{st: st, apns: newFakeAPNs(t)}
	n.p = push.New(push.Config{
		Key: key, KeyID: "KEYID00001", TeamID: "TEAMID0001", Store: st, Logger: log,
		ProductionURL: n.apns.srv.URL, SandboxURL: n.apns.srv.URL, HTTPClient: n.apns.srv.Client(),
		Now: c.now, RealertTick: time.Hour,
	})
	done := make(chan struct{})
	go func() { n.p.Run(ctx); close(done) }()
	set, err := auth.Parse(strings.NewReader(tokens), ',')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	n.h = server.NewServer(server.Options{Store: st, Tokens: set, Validator: v, Logger: log, Pusher: n.p,
		PushState: api.HealthPushOn, Now: c.now})
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.down.Load() {
			// No answer at all: the connection drops.
			panic(http.ErrAbortHandler)
		}
		n.h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { n.srv.Close(); cancel(); <-done; st.Close() })
	return n
}

func (n *node) do(t *testing.T, method, path, token, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, n.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// post stores an event and waits for its pushes.
func (n *node) post(t *testing.T, body string) string {
	t.Helper()
	code, b := n.do(t, http.MethodPost, "/v1/events", senderTok, body)
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	var ev struct{ ID string }
	_ = json.Unmarshal(b, &ev)
	n.p.Wait()
	return ev.ID
}

type rig struct {
	clock            *clock
	primary, standby *node
	sb               *standby.Standby
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	r := &rig{clock: c}
	r.primary = newNode(t, c, fmt.Sprintf("phone read %s,sender write %s,standby read %s standby",
		auth.Hash(phoneTok), auth.Hash(senderTok), auth.Hash(standbyTok)))
	r.standby = newNode(t, c, fmt.Sprintf("sender write %s", auth.Hash(senderTok)))
	r.sb = r.newStandby()
	// The phone registers with the primary only.
	code, b := r.primary.do(t, http.MethodPost, "/v1/devices", phoneTok,
		fmt.Sprintf(`{"token":%q,"environment":"production","topic":%q,"name":"Test phone"}`, devToken, topic))
	if code != http.StatusCreated {
		t.Fatalf("register: %d %s", code, b)
	}
	return r
}

func (r *rig) newStandby() *standby.Standby {
	return standby.New(standby.Config{
		PrimaryURL: r.primary.srv.URL, Token: standbyTok, Store: r.standby.st, Emit: r.standby.h.Emit,
		Now: r.clock.now, DownAfter: 10 * time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTP: &http.Client{Timeout: 5 * time.Second},
	})
}

func (r *rig) tick(t *testing.T) error {
	t.Helper()
	err := r.sb.Tick(context.Background())
	r.standby.p.Wait()
	r.primary.p.Wait()
	return err
}

func TestDeviceExportNeedsStandbyToken(t *testing.T) {
	r := newRig(t)
	if code, _ := r.primary.do(t, http.MethodGet, "/v1/standby/devices", phoneTok, ""); code != http.StatusForbidden {
		t.Fatalf("plain read token: %d, want 403", code)
	}
	if code, _ := r.primary.do(t, http.MethodPost, "/v1/standby/import", phoneTok, `{"events":[]}`); code != http.StatusForbidden {
		t.Fatalf("plain read token import: %d, want 403", code)
	}
	code, b := r.primary.do(t, http.MethodGet, "/v1/standby/devices", standbyTok, "")
	if code != http.StatusOK || !strings.Contains(string(b), devToken) {
		t.Fatalf("standby token: %d %s", code, b)
	}
	// The plain device list still shows only a suffix.
	_, b = r.primary.do(t, http.MethodGet, "/v1/devices", phoneTok, "")
	if strings.Contains(string(b), devToken) {
		t.Fatalf("GET /v1/devices leaks the full token: %s", b)
	}
}

func TestStandbyCopiesDevicesAndDelivers(t *testing.T) {
	r := newRig(t)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	ds, err := r.standby.st.Devices(context.Background(), true)
	if err != nil || len(ds) != 1 || ds[0].Token != devToken {
		t.Fatalf("standby devices: %+v %v", ds, err)
	}
	r.primary.down.Store(true)
	r.standby.post(t, `{"source":"backup","title":"backup failed"}`)
	if got := r.standby.apns.pushes(); len(got) != 1 || got[0] != "/3/device/"+devToken {
		t.Fatalf("standby pushes: %v", got)
	}
	// A device the primary retires stops getting pushes from the standby.
	r.primary.down.Store(false)
	id := ds[0].ID
	if code, b := r.primary.do(t, http.MethodDelete, "/v1/devices/"+id, phoneTok, ""); code != http.StatusOK {
		t.Fatalf("remove: %d %s", code, b)
	}
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	if ds, _ := r.standby.st.Devices(context.Background(), true); len(ds) != 0 {
		t.Fatalf("retired device still active on the standby: %+v", ds)
	}
}

func TestCatchUpImportsOnceAndNeverPushes(t *testing.T) {
	r := newRig(t)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	r.primary.down.Store(true)
	a := r.standby.post(t, `{"source":"backup","title":"backup failed","dedup":"run-1"}`)
	b := r.standby.post(t, `{"source":"ups","kind":"incident","action":"open","key":"ups:on-battery","title":"on battery"}`)
	if err := r.tick(t); err == nil {
		t.Fatal("tick with the primary down: no error")
	}
	r.primary.down.Store(false)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b} {
		code, body := r.primary.do(t, http.MethodGet, "/v1/events/"+id, phoneTok, "")
		if code != http.StatusOK {
			t.Fatalf("primary has no %s: %d %s", id, code, body)
		}
	}
	if row, err := r.primary.st.Row(context.Background(), "incidents", "ups:on-battery"); err != nil || row["state"] != "open" {
		t.Fatalf("primary incident: %v %v", row, err)
	}
	if got := r.primary.apns.pushes(); len(got) != 0 {
		t.Fatalf("the primary pushed imported events: %v", got)
	}
	n, _ := r.primary.st.Count(context.Background())
	// Ticks after the catch-up send nothing new.
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	// The same batch again stores nothing twice.
	recs, _ := r.standby.st.List(context.Background(), store.Filter{})
	evs := make([]json.RawMessage, len(recs))
	for i, rec := range recs {
		evs[i] = rec.Event
	}
	body, _ := json.Marshal(map[string]any{"events": evs})
	code, out := r.primary.do(t, http.MethodPost, "/v1/standby/import", standbyTok, string(body))
	var res api.StandbyImportResult
	_ = json.Unmarshal(out, &res)
	if code != http.StatusOK || res.Imported != 0 || res.Skipped != 2 {
		t.Fatalf("second import: %d %s", code, out)
	}
	if m, _ := r.primary.st.Count(context.Background()); m != n {
		t.Fatalf("events after repeats: %d, want %d", m, n)
	}
	r.primary.p.Wait()
	if got := r.primary.apns.pushes(); len(got) != 0 {
		t.Fatalf("the primary pushed imported events: %v", got)
	}
}

func TestPrimaryDownAlertOnceAndResolve(t *testing.T) {
	r := newRig(t)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	r.primary.down.Store(true)
	for _, step := range []time.Duration{time.Minute, 4 * time.Minute, 4 * time.Minute} {
		r.clock.add(step)
		_ = r.tick(t)
	}
	if got := r.standby.apns.pushes(); len(got) != 0 {
		t.Fatalf("alert before 10 minutes: %v", got)
	}
	r.clock.add(2 * time.Minute)
	_ = r.tick(t)
	if got := r.standby.apns.pushes(); len(got) != 1 {
		t.Fatalf("pushes after 11 minutes: %v, want 1", got)
	}
	// Still down: no second alert, from this standby or from a restarted one.
	r.clock.add(time.Minute)
	_ = r.tick(t)
	r.sb = r.newStandby()
	r.clock.add(30 * time.Minute)
	_ = r.tick(t)
	if got := r.standby.apns.pushes(); len(got) != 1 {
		t.Fatalf("pushes while still down: %v, want 1", got)
	}
	row, err := r.standby.st.Row(context.Background(), "incidents", standby.AlertKey)
	if err != nil || row["state"] != "open" {
		t.Fatalf("standby alert: %v %v", row, err)
	}
	r.primary.down.Store(false)
	if err := r.tick(t); err != nil {
		t.Fatal(err)
	}
	if row, _ := r.standby.st.Row(context.Background(), "incidents", standby.AlertKey); row["state"] != "resolved" {
		t.Fatalf("standby alert after recovery: %v", row)
	}
	// The primary has the alert's history, resolved, and pushed none of it.
	row, err = r.primary.st.Row(context.Background(), "incidents", standby.AlertKey)
	if err != nil || row["state"] != "resolved" {
		t.Fatalf("primary alert: %v %v", row, err)
	}
	if got := r.primary.apns.pushes(); len(got) != 0 {
		t.Fatalf("primary pushes: %v", got)
	}
	// Up again: a new outage raises a new alert.
	r.primary.down.Store(true)
	r.clock.add(11 * time.Minute)
	_ = r.tick(t)
	if row, _ := r.standby.st.Row(context.Background(), "incidents", standby.AlertKey); row["state"] != "open" {
		t.Fatalf("second outage: %v", row)
	}
}

func TestProxyErrorCountsAsDown(t *testing.T) {
	r := newRig(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer proxy.Close()
	sb := standby.New(standby.Config{
		PrimaryURL: proxy.URL, Token: standbyTok, Store: r.standby.st, Emit: r.standby.h.Emit,
		Now: r.clock.now, DownAfter: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	r.clock.add(2 * time.Minute)
	_ = sb.Tick(context.Background())
	if row, err := r.standby.st.Row(context.Background(), "incidents", standby.AlertKey); err != nil || row["state"] != "open" {
		t.Fatalf("alert on 502: %v %v", row, err)
	}
}
