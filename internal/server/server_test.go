package server

import (
	"bytes"
	"context"
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

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/teeimport"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	writeTok = "write-token-for-tests"
	readTok  = "read-token-for-tests"
	adminTok = "admin-token-for-tests"
	// bridgeTok carries the trusted-ingest capability.
	bridgeTok = "bridge-token-for-tests"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	h     http.Handler
	st    *store.Store
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	st.SetClock(c.now)
	list := fmt.Sprintf("sender write %s\nphone read %s\nops admin %s\nbridge write %s trusted-ingest\n",
		auth.Hash(writeTok), auth.Hash(readTok), auth.Hash(adminTok), auth.Hash(bridgeTok))
	tokens, err := auth.Parse(strings.NewReader(list), '\n')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{
		Version: "v-test", Store: st, Tokens: tokens, Validator: v, Now: c.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &env{h: h, st: st, clock: c}
}

func (e *env) do(t *testing.T, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, "/v1/events", writeTok, []byte(body))
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

// storedSchema is $defs/Event, what the read API returns.
func storedSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile("../../schema/envelope.v1.schema.json#/$defs/Event")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkStored(t *testing.T, s *jsonschema.Schema, raw []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(v); err != nil {
		t.Fatalf("stored event fails $defs/Event: %v\n%s", err, raw)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	h := decode[api.Health](t, rec)
	if h.Status != api.HealthStatusOk || h.Version != "v-test" || h.Db != api.HealthDbOk || h.Events == nil || *h.Events != 0 {
		t.Fatalf("body = %+v", h)
	}
	e.post(t, `{"source":"s","kind":"message"}`)
	if h := decode[api.Health](t, e.do(t, http.MethodGet, "/healthz", "", nil)); *h.Events != 1 {
		t.Fatalf("events = %d, want 1", *h.Events)
	}
}

func TestHealthzDatabaseDown(t *testing.T) {
	e := newEnv(t)
	e.st.Close()
	rec := e.do(t, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if h := decode[api.Health](t, rec); h.Db != api.HealthDbError || h.Events != nil {
		t.Fatalf("body = %+v", h)
	}
}

func TestHealthzMethod(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPost, "/healthz", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestAuthMatrix(t *testing.T) {
	e := newEnv(t)
	created := e.post(t, `{"source":"s","kind":"message"}`)
	id := decode[api.Event](t, created).Id
	body := []byte(`{"source":"s","kind":"message"}`)
	calls := []struct {
		name, method, target string
		body                 []byte
		ok                   int
		scope                auth.Scope
	}{
		{"post", http.MethodPost, "/v1/events", body, http.StatusCreated, auth.ScopeWrite},
		{"send", http.MethodGet, "/v1/send?source=s&kind=message", nil, http.StatusCreated, auth.ScopeWrite},
		{"list", http.MethodGet, "/v1/events", nil, http.StatusOK, auth.ScopeRead},
		{"get", http.MethodGet, "/v1/events/" + id, nil, http.StatusOK, auth.ScopeRead},
	}
	tokens := []struct {
		name, tok string
		scope     auth.Scope
	}{
		{"none", "", ""},
		{"unknown", "not-a-token", ""},
		{"write", writeTok, auth.ScopeWrite},
		{"read", readTok, auth.ScopeRead},
		{"admin", adminTok, auth.ScopeAdmin},
	}
	for _, c := range calls {
		for _, tk := range tokens {
			t.Run(c.name+"/"+tk.name, func(t *testing.T) {
				rec := e.do(t, c.method, c.target, tk.tok, c.body)
				want := c.ok
				switch {
				case tk.scope == "":
					want = http.StatusUnauthorized
				case !tk.scope.Allows(c.scope):
					want = http.StatusForbidden
				}
				if rec.Code != want {
					t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
				}
				if want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("401 without WWW-Authenticate")
				}
			})
		}
	}
}

func TestAuthScheme(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Basic "+readTok)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestTokenParamOnlyOnSend(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodGet, "/v1/send?source=s&kind=message&token="+writeTok, "", nil); rec.Code != http.StatusCreated {
		t.Fatalf("send with token param: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?token="+readTok, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list with token param: %d, want 401", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/send?source=s&kind=message&token="+readTok, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("send with a read token: %d, want 403", rec.Code)
	}
}

func examples(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("../../schema/examples", dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples in %s", dir)
	}
	return files
}

func TestPostValidExamples(t *testing.T) {
	e := newEnv(t)
	s := storedSchema(t)
	for _, path := range examples(t, "valid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rec := e.do(t, http.MethodPost, "/v1/events", writeTok, raw)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			checkStored(t, s, rec.Body.Bytes())
			ev := decode[api.Event](t, rec)
			if !ids.Valid(ev.Id) || !ev.ReceivedAt.Equal(e.clock.now()) {
				t.Fatalf("id %q received_at %v", ev.Id, ev.ReceivedAt)
			}
			got := e.do(t, http.MethodGet, "/v1/events/"+ev.Id, readTok, nil)
			if got.Code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(got.Body.Bytes()), bytes.TrimSpace(rec.Body.Bytes())) {
				t.Fatalf("GET by id: %d %s", got.Code, got.Body)
			}
		})
	}
}

func TestPostKeepsNumbersExact(t *testing.T) {
	e := newEnv(t)
	rec := e.post(t, `{"source":"s","kind":"message","relevance":0.3}`)
	if !strings.Contains(rec.Body.String(), `"relevance":0.3`) {
		t.Fatalf("relevance changed: %s", rec.Body)
	}
}

func TestPostTsDefaultsToArrival(t *testing.T) {
	e := newEnv(t)
	ev := decode[api.Event](t, e.post(t, `{"source":"s","kind":"message"}`))
	if !ev.Ts.Equal(e.clock.now()) {
		t.Fatalf("ts = %v", ev.Ts)
	}
	ev = decode[api.Event](t, e.post(t, `{"source":"s","kind":"message","ts":"2026-01-02T03:04:05Z"}`))
	if ev.Ts.Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
		t.Fatalf("sender ts not kept: %v", ev.Ts)
	}
}

func TestPostInvalidExamples(t *testing.T) {
	e := newEnv(t)
	for _, path := range examples(t, "invalid") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if rec := e.do(t, http.MethodPost, "/v1/events", writeTok, raw); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
	if n, _ := e.st.Count(context.Background()); n != 0 {
		t.Fatalf("stored %d invalid events", n)
	}
}

func TestPostRefusesServerFields(t *testing.T) {
	e := newEnv(t)
	for _, f := range []string{
		`"id":"01JBA6Z0000000000000000000"`,
		`"received_at":"2026-10-26T12:00:00Z"`,
		`"hold":{}`,
		`"delivery":[]`,
	} {
		rec := e.post(t, `{"source":"s","kind":"message",`+f+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "set by the server") {
			t.Fatalf("%s: %d %s", f, rec.Code, rec.Body)
		}
	}
	rec := e.do(t, http.MethodGet, "/v1/send?source=s&kind=message&id=x", writeTok, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "set by the server") {
		t.Fatalf("send id: %d %s", rec.Code, rec.Body)
	}
}

func TestPostBodyLimitCountsBytes(t *testing.T) {
	e := newEnv(t)
	// 100k three-byte characters: within the schema's character limit,
	// over the 256 KiB byte limit.
	body := strings.Repeat("€", 100_000)
	raw, _ := json.Marshal(map[string]string{"source": "s", "kind": "message", "body": body})
	rec := e.post(t, string(raw))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bytes") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	ok, _ := json.Marshal(map[string]string{"source": "s", "kind": "message", "body": strings.Repeat("a", validate.MaxBodyBytes)})
	if rec := e.post(t, string(ok)); rec.Code != http.StatusCreated {
		t.Fatalf("256 KiB body: %d", rec.Code)
	}
}

func TestPostTooLarge(t *testing.T) {
	e := newEnv(t)
	raw := `{"source":"s","kind":"message","body":"` + strings.Repeat("a", MaxRequestBytes) + `"}`
	if rec := e.post(t, raw); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestPostStepsCurrentOverTotal(t *testing.T) {
	e := newEnv(t)
	rec := e.post(t, `{"source":"p","kind":"progress","key":"job","action":"update","content":{"steps":{"total":3,"current":4}}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "steps") {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestPostNotJSON(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{"", "nope", "[]", `{"source":"s","kind":"message"} {}`} {
		if rec := e.post(t, b); rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d", b, rec.Code)
		}
	}
}

func TestDedup(t *testing.T) {
	e := newEnv(t)
	first := e.post(t, `{"source":"s","kind":"message","dedup":"run-1","title":"one"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d", first.Code)
	}
	e.clock.add(time.Hour)
	again := e.post(t, `{"source":"s","kind":"message","dedup":"run-1","title":"two"}`)
	if again.Code != http.StatusOK {
		t.Fatalf("repeat: %d, want 200", again.Code)
	}
	a, b := decode[api.Event](t, first), decode[api.Event](t, again)
	if a.Id != b.Id || *b.Title != "one" {
		t.Fatalf("repeat returned %+v, want the first event", b)
	}
	// GET /v1/send shares the dedup.
	if rec := e.do(t, http.MethodGet, "/v1/send?source=s&kind=message&dedup=run-1", writeTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("send repeat: %d", rec.Code)
	}
	if n, _ := e.st.Count(context.Background()); n != 1 {
		t.Fatalf("stored %d events, want 1", n)
	}
	e.clock.add(24 * time.Hour)
	late := e.post(t, `{"source":"s","kind":"message","dedup":"run-1"}`)
	if late.Code != http.StatusCreated || decode[api.Event](t, late).Id == a.Id {
		t.Fatalf("after 25 h: %d, want a new event", late.Code)
	}
}

func TestSend(t *testing.T) {
	e := newEnv(t)
	q := url.Values{
		"source": {"door"}, "kind": {"message"}, "title": {"Doorbell"}, "level": {"time-sensitive"},
		"tags": {"home", "front"}, "relevance": {"0.5"}, "url": {"https://example.org/cam"},
	}
	rec := e.do(t, http.MethodGet, "/v1/send?"+q.Encode(), writeTok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	checkStored(t, storedSchema(t), rec.Body.Bytes())
	ev := decode[api.Event](t, rec)
	if ev.Tags == nil || len(*ev.Tags) != 2 || *ev.Level != api.LevelTimeSensitive || *ev.Relevance != 0.5 {
		t.Fatalf("event = %+v", ev)
	}
	for _, bad := range []string{
		"source=s&kind=message&relevance=high",
		"source=s&kind=message&color=red",
		"source=s&kind=message&title=a&title=b",
		"source=s&kind=nope",
		"kind=message",
		"source=s&kind=incident",
	} {
		if rec := e.do(t, http.MethodGet, "/v1/send?"+bad, writeTok, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestListPagesAndFilters(t *testing.T) {
	e := newEnv(t)
	for _, b := range []string{
		`{"source":"backup","host":"box-a","kind":"message","title":"Backup finished","tags":["nightly"]}`,
		`{"source":"backup","host":"box-b","kind":"message","title":"Backup failed","body":"disk full"}`,
		`{"source":"monitor","host":"box-a","kind":"incident","key":"disk","action":"open","summary":"disk almost full"}`,
		`{"source":"monitor","host":"box-a","kind":"incident","key":"disk","action":"resolve"}`,
		`{"source":"cron","kind":"checkin","key":"job","action":"success","tags":["nightly","cron"]}`,
	} {
		if rec := e.post(t, b); rec.Code != http.StatusCreated {
			t.Fatalf("post: %d %s", rec.Code, rec.Body)
		}
	}
	list := func(q string) api.EventPage {
		t.Helper()
		rec := e.do(t, http.MethodGet, "/v1/events?"+q, readTok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		return decode[api.EventPage](t, rec)
	}
	// Paging: 2 + 2 + 1, then an empty page without a cursor.
	var all []api.Event
	cursor := ""
	for i := 0; i < 3; i++ {
		p := list("limit=2&after=" + cursor)
		all = append(all, p.Events...)
		if p.NextCursor == nil {
			t.Fatalf("page %d: no next_cursor", i)
		}
		cursor = *p.NextCursor
	}
	if len(all) != 5 {
		t.Fatalf("paged %d events, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Id <= all[i-1].Id {
			t.Fatalf("ids not ascending: %s then %s", all[i-1].Id, all[i].Id)
		}
	}
	if p := list("after=" + cursor); len(p.Events) != 0 || p.NextCursor != nil {
		t.Fatalf("after the end: %+v", p)
	}
	for q, want := range map[string]int{
		"source=backup":            2,
		"kind=incident":            2,
		"host=box-a":               3,
		"key=disk":                 2,
		"tag=nightly":              2,
		"tag=cron":                 1,
		"q=backup":                 2,
		"q=disk":                   2, // body "disk full" and summary "disk almost full"
		"q=backup+fail*":           0, // words are quoted, so * is literal
		"q=failed":                 1,
		"source=backup&host=box-a": 1,
	} {
		if got := len(list(q).Events); got != want {
			t.Errorf("%s: %d events, want %d", q, got, want)
		}
	}
	for _, bad := range []string{"limit=0", "limit=501", "limit=x", "kind=nope", "after=garbage"} {
		if rec := e.do(t, http.MethodGet, "/v1/events?"+bad, readTok, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestGetUnknown(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodGet, "/v1/events/01JBA6Z0000000000000000000", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDerivedState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	row := func(table, key string) map[string]any {
		t.Helper()
		r, err := e.st.Row(ctx, table, key)
		if err != nil {
			t.Fatalf("%s %s: %v", table, key, err)
		}
		return r
	}
	post := func(b string) {
		t.Helper()
		if rec := e.post(t, b); rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", b, rec.Code, rec.Body)
		}
	}

	post(`{"source":"m","kind":"incident","key":"db","action":"open"}`)
	if r := row("incidents", "db"); r["state"] != "open" || r["resolved_at"] != nil {
		t.Fatalf("open: %v", r)
	}
	post(`{"source":"m","kind":"incident","key":"db","action":"ack"}`)
	post(`{"source":"m","kind":"incident","key":"db","action":"update"}`)
	if r := row("incidents", "db"); r["state"] != "acked" {
		t.Fatalf("update after ack: %v", r)
	}
	post(`{"source":"m","kind":"incident","key":"db","action":"resolve"}`)
	if r := row("incidents", "db"); r["state"] != "resolved" || r["resolved_at"] == nil {
		t.Fatalf("resolve: %v", r)
	}
	e.clock.add(time.Minute)
	post(`{"source":"m","kind":"incident","key":"db","action":"open"}`)
	if r := row("incidents", "db"); r["state"] != "open" || r["resolved_at"] != nil {
		t.Fatalf("reopen: %v", r)
	}

	post(`{"source":"c","kind":"checkin","key":"job","action":"start"}`)
	if r := row("checkins", "job"); r["state"] != "running" {
		t.Fatalf("start: %v", r)
	}
	post(`{"source":"c","kind":"checkin","key":"job","checkin":{"exit_code":2}}`)
	if r := row("checkins", "job"); r["state"] != "down" {
		t.Fatalf("exit 2: %v", r)
	}
	post(`{"source":"c","kind":"checkin","key":"job","action":"success"}`)
	if r := row("checkins", "job"); r["state"] != "up" {
		t.Fatalf("success: %v", r)
	}

	post(`{"source":"p","kind":"progress","key":"print","action":"start","content":{"progress":0.1}}`)
	if r := row("activities", "print"); r["state"] != "active" {
		t.Fatalf("progress start: %v", r)
	}
	post(`{"source":"p","kind":"progress","key":"print","action":"end"}`)
	if r := row("activities", "print"); r["state"] != "ended" {
		t.Fatalf("progress end: %v", r)
	}

	post(`{"source":"s","kind":"status","key":"ups","action":"set","content":{"title":"UPS","status":"On mains","trailing":"100 %"}}`)
	post(`{"source":"s","kind":"status","key":"ups","action":"set","content":{"status":"On battery","trailing":null}}`)
	var content map[string]any
	if err := json.Unmarshal([]byte(row("status", "ups")["content"].(string)), &content); err != nil {
		t.Fatal(err)
	}
	if content["title"] != "UPS" || content["status"] != "On battery" || content["trailing"] != nil {
		t.Fatalf("merged content = %v", content)
	}
	if _, has := content["trailing"]; has {
		t.Fatal("null did not clear trailing")
	}
	post(`{"source":"s","kind":"status","key":"ups","action":"clear"}`)
	if _, err := e.st.Row(ctx, "status", "ups"); err != store.ErrNotFound {
		t.Fatalf("clear left a row: %v", err)
	}
}

func TestTrustedIngestHold(t *testing.T) {
	e := newEnv(t)
	body := []byte(`{"source":"diskwatch","kind":"message","tags":["alert"],"dedup":"n-1",
		"details":{"tee":{"hold":{"id":"h-1","state":"held","forwarded":false,"store_only":true}}}}`)
	rec := e.do(t, http.MethodPost, "/v1/events", bridgeTok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	checkStored(t, storedSchema(t), rec.Body.Bytes())
	ev := decode[api.Event](t, rec)
	if ev.Hold == nil || (*ev.Hold)["state"] != "held" || (*ev.Hold)["id"] != "h-1" || (*ev.Hold)["store_only"] != true {
		t.Fatalf("hold = %v", ev.Hold)
	}
	// The same input from a token without the capability keeps no hold.
	plain := bytes.Replace(body, []byte(`"n-1"`), []byte(`"n-2"`), 1)
	if ev := decode[api.Event](t, e.do(t, http.MethodPost, "/v1/events", writeTok, plain)); ev.Hold != nil {
		t.Fatalf("untrusted token set hold: %v", ev.Hold)
	}
	// A trusted event without details.tee.hold has no hold.
	if ev := decode[api.Event](t, e.do(t, http.MethodPost, "/v1/events", bridgeTok,
		[]byte(`{"source":"s","kind":"message","dedup":"n-3"}`))); ev.Hold != nil {
		t.Fatalf("hold without details.tee.hold: %v", ev.Hold)
	}
}

func TestTrustedIngestDedupHasNoWindow(t *testing.T) {
	e := newEnv(t)
	b := []byte(`{"source":"s","kind":"message","dedup":"n-20261027-120000-abcdef"}`)
	first := e.do(t, http.MethodPost, "/v1/events", bridgeTok, b)
	e.clock.add(30 * 24 * time.Hour)
	again := e.do(t, http.MethodPost, "/v1/events", bridgeTok, b)
	if again.Code != http.StatusOK || decode[api.Event](t, again).Id != decode[api.Event](t, first).Id {
		t.Fatalf("trusted repeat after 30 days: %d", again.Code)
	}
	// A plain token still gets the 24 h window.
	if rec := e.do(t, http.MethodPost, "/v1/events", writeTok, b); rec.Code != http.StatusCreated {
		t.Fatalf("plain token after 30 days: %d, want 201", rec.Code)
	}
}

// Live forwarding and the backfill share the tee id as dedup, so the same
// record is stored once whichever comes first.
func TestBackfillAndLiveStoreOnce(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	v, _ := validate.New()

	// Live first, then the backfill.
	e := newEnv(t)
	live := e.do(t, http.MethodPost, "/v1/events", bridgeTok,
		[]byte(`{"source":"backup","kind":"message","dedup":"n-20260901-080000-a1b2c3"}`))
	if live.Code != http.StatusCreated {
		t.Fatalf("live: %d", live.Code)
	}
	s, err := teeimport.Run(ctx, "../teeimport/testdata/catalogue", e.st, v, quiet)
	if err != nil || s.Imported != 2 || s.Skipped != 1 {
		t.Fatalf("import after live: %+v, %v", s, err)
	}

	// Backfill first, then live.
	e = newEnv(t)
	if _, err := teeimport.Run(ctx, "../teeimport/testdata/catalogue", e.st, v, quiet); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, http.MethodPost, "/v1/events", bridgeTok,
		[]byte(`{"source":"backup","kind":"message","dedup":"n-20260901-090500-d4e5f6"}`))
	if rec.Code != http.StatusOK || decode[api.Event](t, rec).Title == nil {
		t.Fatalf("live after import: %d %s, want the imported event", rec.Code, rec.Body)
	}
	if n, _ := e.st.Count(ctx); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
}
