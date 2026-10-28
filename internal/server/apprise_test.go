package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/push"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// keyTok is a write token with the notify-key capability.
const keyTok = "key-token-for-tests"

const bypassJSON = `[
  {"tag": "page"},
  {"title_regex": "^🚨 ZFS (pool|dataset) "},
  {"title_regex": "SMART .*(FAILED|[Ff]ailed|[Ff]ailure)"},
  {"title_regex": "^(Alert triage|rounds:)"}
]`

type holdEnv struct {
	t     *testing.T
	srv   *Server
	st    *store.Store
	clock *clock
	path  string
	log   *bytes.Buffer
}

// newHoldEnv is a server with the hold in mode, the bypass list above, and
// 198.51.100.0/24 as the trusted network.
func newHoldEnv(t *testing.T, mode hold.Mode) *holdEnv {
	t.Helper()
	dir := t.TempDir()
	bp := filepath.Join(dir, "bypass.json")
	if err := os.WriteFile(bp, []byte(bypassJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &holdEnv{t: t, path: filepath.Join(dir, "test.db"), clock: &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}}
	e.open(mode, bp)
	return e
}

func (e *holdEnv) open(mode hold.Mode, bypassFile string) {
	t := e.t
	st, err := store.Open(context.Background(), e.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClock(e.clock.now)
	list := fmt.Sprintf("sender write %s\nphone read %s\nkeyed write %s notify-key\n",
		auth.Hash(writeTok), auth.Hash(readTok), auth.Hash(keyTok))
	tokens, err := auth.Parse(strings.NewReader(list), '\n')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	e.log = &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(e.log, nil))
	bypass, err := hold.NewBypass(bypassFile, log)
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	e.srv = NewServer(Options{
		Version: "v-test", Store: st, Tokens: tokens, Validator: v, Now: e.clock.now, Logger: log,
		TrustedNets: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
		Hold:        HoldOptions{Mode: mode, Bypass: bypass},
	})
}

// req sends a request from addr (an untrusted address when empty).
func (e *holdEnv) req(method, target, token, addr, ctype string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	r.RemoteAddr = "203.0.113.9:4242"
	if addr != "" {
		r.RemoteAddr = addr
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, r)
	return rec
}

// send posts JSON to /notify/apprise from the trusted network.
func (e *holdEnv) send(body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.req("POST", "/notify/apprise", "", "198.51.100.7:5000", "application/json", []byte(body))
}

func (e *holdEnv) tick() { e.srv.s.tickHold(context.Background()) }

func (e *holdEnv) held(query string) map[string]any {
	e.t.Helper()
	rec := e.req("GET", "/held"+query, readTok, "", "", nil)
	if rec.Code != 200 {
		e.t.Fatalf("/held: %d %s", rec.Code, rec.Body)
	}
	var v map[string]any
	json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

func (e *holdEnv) rows(query string) []map[string]any {
	var out []map[string]any
	for _, r := range e.held(query)["held"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

// events lists every stored event, oldest first.
func (e *holdEnv) events() []map[string]any {
	e.t.Helper()
	recs, err := e.st.List(context.Background(), store.Filter{Limit: 500})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []map[string]any
	for _, r := range recs {
		var ev map[string]any
		json.Unmarshal(r.Event, &ev)
		out = append(out, ev)
	}
	return out
}

func holdOf(ev map[string]any) map[string]any {
	h, _ := ev["hold"].(map[string]any)
	return h
}

func sentOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != 200 || !strings.Contains(rec.Body.String()[:min(500, rec.Body.Len())], "Sent") {
		t.Fatalf("want 200 with Sent: %d %s", rec.Code, rec.Body)
	}
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// ---- ingest ------------------------------------------------------------

func TestNotifyContract(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	sentOK(t, e.send(`{"title":"kopia: snapshot failed","body":"exit 1","tag":"general","type":"failure"}`))
	ev := e.events()[0]
	if ev["source"] != "kopia" || ev["kind"] != "message" || ev["level"] != "active" || ev["title"] != "kopia: snapshot failed" {
		t.Fatalf("event = %v", ev)
	}
	if tags := ev["tags"].([]any); len(tags) != 1 || tags[0] != "general" {
		t.Fatalf("tags = %v", tags)
	}
	d := ev["details"].(map[string]any)["notify"].(map[string]any)
	if d["client"] != "198.51.100.7" || d["path"] != "/notify/apprise" || d["type"] != "failure" {
		t.Fatalf("details = %v", d)
	}
	if holdOf(ev) != nil {
		t.Fatalf("hold in off mode: %v", ev["hold"])
	}
}

func TestNotifyFormMultipartAndLevel(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	// A form post; level replaces the tag; the source comes from the title.
	sentOK(t, e.req("POST", "/notify/apprise", "", "198.51.100.7:1", "application/x-www-form-urlencoded",
		[]byte("title=Gatus%2Fweb-1+down&body=b&tag=general&level=quiet")))
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("body", "from multipart")
	mw.WriteField("tags", "page, extra")
	fw, _ := mw.CreateFormFile("attach", "x.txt")
	fw.Write([]byte("ignored"))
	mw.Close()
	sentOK(t, e.req("POST", "/notify/apprise", "", "198.51.100.7:1", mw.FormDataContentType(), buf.Bytes()))
	evs := e.events()
	if evs[0]["source"] != "Gatus/web-1" || evs[0]["level"] != "passive" || evs[0]["tags"].([]any)[0] != "quiet" {
		t.Fatalf("form event = %v", evs[0])
	}
	if evs[1]["body"] != "from multipart" || evs[1]["level"] != "time-sensitive" || len(evs[1]["tags"].([]any)) != 2 {
		t.Fatalf("multipart event = %v", evs[1])
	}
	// No title prefix: a trusted sender's source is its address.
	if evs[1]["source"] != "198.51.100.7" {
		t.Fatalf("source = %v", evs[1]["source"])
	}
}

func TestNotifyBadBodies(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	for _, c := range []struct{ path, ctype, body string }{
		{"/notify/apprise", "application/json", `{}`},
		{"/notify/apprise", "application/json", `[1]`},
		{"/v1/notify", "application/json", `{"title":"only a title"}`},
	} {
		if rec := e.req("POST", c.path, writeTok, "", c.ctype, []byte(c.body)); rec.Code != 400 {
			t.Errorf("%s %s: %d", c.path, c.body, rec.Code)
		}
	}
	if len(e.events()) != 0 {
		t.Fatal("a bad body was stored")
	}
}

func TestNotifyV1Answer(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	rec := e.req("POST", "/v1/notify", writeTok, "", "application/json", []byte(`{"body":"b","tag":"quiet","url":"not a url"}`))
	sentOK(t, rec)
	v := jsonOf(t, rec)
	if v["delivered"] != true || v["id"] == nil || v["backends"].(map[string]any)["heraldo"] == nil {
		t.Fatalf("answer = %v", v)
	}
	ev := e.events()[0]
	// A bearer sender's source is its token name; a bad url is dropped.
	if ev["source"] != "sender" || ev["url"] != nil || ev["details"].(map[string]any)["notify"].(map[string]any)["path"] != "/v1/notify" {
		t.Fatalf("event = %v", ev)
	}
}

func TestNotifyStoreFailureIs424(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	e.st.Close()
	rec := e.send(`{"body":"b","tag":"general"}`)
	if rec.Code != http.StatusFailedDependency || strings.Contains(rec.Body.String(), "Sent") {
		t.Fatalf("closed store: %d %s", rec.Code, rec.Body)
	}
}

func TestNotifyAuth(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	body := []byte(`{"body":"b"}`)
	for _, c := range []struct {
		name, path, token, addr string
		want                    int
	}{
		{"untrusted, no credential", "/notify/apprise", "", "", 401},
		{"trusted network", "/notify/apprise", "", "198.51.100.200:1", 200},
		{"bearer write", "/notify/apprise", writeTok, "", 200},
		{"bearer read", "/notify/apprise", readTok, "", 403},
		{"key in the path", "/notify/" + keyTok, "", "", 200},
		{"write token without notify-key in the path", "/notify/" + writeTok, "", "", 401},
		{"v1 from the trusted network", "/v1/notify", "", "198.51.100.1:1", 200},
		{"v1 untrusted", "/v1/notify", "", "", 401},
	} {
		if rec := e.req("POST", c.path, c.token, c.addr, "application/json", body); rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body)
		}
	}
	if strings.Contains(e.log.String(), keyTok) {
		t.Fatal("the path key reached the log")
	}
	// With no source in the title: the address, the token name, the key's name.
	if ev := e.events(); ev[0]["source"] != "198.51.100.200" || ev[1]["source"] != "sender" || ev[2]["source"] != "keyed" {
		t.Fatalf("sources = %v, %v, %v", ev[0]["source"], ev[1]["source"], ev[2]["source"])
	}
	// The hold endpoints: read to look, write to close.
	if rec := e.req("GET", "/held", "", "", "", nil); rec.Code != 401 {
		t.Errorf("/held untrusted: %d", rec.Code)
	}
	if rec := e.req("GET", "/held", "", "198.51.100.3:1", "", nil); rec.Code != 200 {
		t.Errorf("/held trusted: %d", rec.Code)
	}
	if rec := e.req("POST", "/v1/hold/suppress", readTok, "", "application/json", []byte(`{"ids":["h-x"]}`)); rec.Code != 403 {
		t.Errorf("suppress with read: %d", rec.Code)
	}
}

func TestRecentAndEntry(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	for _, b := range []string{
		`{"title":"Gatus/web-1 down","body":"1","tag":"alert"}`,
		`{"title":"x","body":"2"}`,
		`{"title":"kopia: ok","body":"3","tag":"quiet"}`,
		`{"title":"Gatus/web-2 down","body":"4","tag":"alert"}`,
	} {
		sentOK(t, e.send(b))
		e.clock.add(time.Second)
	}
	get := func(q string) []map[string]any {
		rec := e.req("GET", "/recent"+q, readTok, "", "", nil)
		var out []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %s", q, rec.Body)
		}
		return out
	}
	if all := get("?n=3"); len(all) != 3 || all[2]["body"] != "4" || all[0]["body"] != "2" {
		t.Fatalf("n=3 = %v", all)
	}
	if g := get("?source=gatus"); len(g) != 2 || g[0]["body"] != "1" {
		t.Fatalf("source=gatus = %v", g)
	}
	if n := get("?tag=none"); len(n) != 1 || n[0]["body"] != "2" || n[0]["tag"] != nil {
		t.Fatalf("tag=none = %v", n)
	}
	last := get("?n=1")[0]
	if last["delivered"] != true || last["status"] != float64(200) || last["client"] != "198.51.100.7" || last["ts"] == nil {
		t.Fatalf("entry shape = %v", last)
	}
	rec := e.req("GET", "/entry/"+last["id"].(string), readTok, "", "", nil)
	if rec.Code != 200 || jsonOf(t, rec)["body"] != "4" {
		t.Fatalf("entry: %d %s", rec.Code, rec.Body)
	}
	if rec := e.req("GET", "/entry/nope", readTok, "", "", nil); rec.Code != 404 {
		t.Fatalf("unknown entry: %d", rec.Code)
	}
}

// ---- the hold: shadow --------------------------------------------------

func TestShadowAlertPushedAndRecorded(t *testing.T) {
	e := newHoldEnv(t, hold.Shadow)
	sentOK(t, e.send(`{"title":"disk full on web-1","body":"b","tag":"alert"}`))
	rows := e.rows("")
	if len(rows) != 1 || rows[0]["shadow"] != true || rows[0]["forwarded"] != true || rows[0]["status"] != "held" {
		t.Fatalf("rows = %v", rows)
	}
	h := holdOf(e.events()[0])
	if h["state"] != "passed" || h["id"] != rows[0]["id"] || h["shadow"] != true {
		t.Fatalf("event hold = %v", h)
	}
	if rows[0]["event_id"] != e.events()[0]["id"] {
		t.Fatal("the entry does not name its event")
	}
}

func TestBypassTitlesNotHeld(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	for _, title := range []string{"🚨 ZFS pool tank at 93%", "SMART failure on sda", "rounds: job failed"} {
		rec := e.send(fmt.Sprintf(`{"title":%q,"body":"b","tag":"alert"}`, title))
		sentOK(t, rec)
		if strings.Contains(rec.Body.String(), "Held") {
			t.Fatalf("%s was held", title)
		}
	}
	sentOK(t, e.send(`{"title":"anything","body":"b","tag":"page"}`))
	if rows := e.rows(""); len(rows) != 0 {
		t.Fatalf("bypassed alerts listed: %v", rows)
	}
	for _, ev := range e.events()[:3] {
		if holdOf(ev)["state"] != "bypass" {
			t.Fatalf("hold = %v", ev["hold"])
		}
	}
	if holdOf(e.events()[3]) != nil {
		t.Fatal("a page post is not a hold candidate")
	}
}

func TestSmartWarningAndZpoolWarnAreHeld(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	for _, title := range []string{"SMART WARNING on sda", "⚠️ ZFS pool tank at 86%"} {
		if rec := e.send(fmt.Sprintf(`{"title":%q,"body":"b","tag":"alert"}`, title)); !strings.Contains(rec.Body.String(), "Held for triage") {
			t.Fatalf("%s: %s", title, rec.Body)
		}
	}
	if len(e.rows("?state=pending")) != 2 {
		t.Fatal("want two held")
	}
}

func TestOtherTagsUntouched(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	for _, tag := range []string{"general", "quiet", "ops", "alert,general", ""} {
		sentOK(t, e.send(fmt.Sprintf(`{"title":"t","body":"b","tag":%q}`, tag)))
	}
	if len(e.rows("")) != 0 {
		t.Fatal("a non-alert post was held")
	}
}

func TestShadowReleaseAndSuppressOnlyRecord(t *testing.T) {
	e := newHoldEnv(t, hold.Shadow)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	e.send(`{"title":"c","body":"d","tag":"alert"}`)
	rows := e.rows("")
	rec := e.req("POST", "/held/release", writeTok, "", "application/json",
		[]byte(fmt.Sprintf(`{"ids":[%q],"verdict":"real","title":"diag","body":"x","tag":"general"}`, rows[0]["id"])))
	if v := jsonOf(t, rec); rec.Code != 200 || v["sent"] != false || v["mode"] != "shadow" {
		t.Fatalf("release: %d %v", rec.Code, v)
	}
	rec = e.req("POST", "/held/suppress", writeTok, "", "application/json",
		[]byte(fmt.Sprintf(`{"ids":[%q,"h-unknown"],"reason":"BLIP: recovered"}`, rows[1]["id"])))
	v := jsonOf(t, rec)
	if rec.Code != 200 || len(v["suppressed"].([]any)) != 1 || v["unknown_or_closed"].([]any)[0] != "h-unknown" {
		t.Fatalf("suppress: %d %v", rec.Code, v)
	}
	if len(e.events()) != 2 {
		t.Fatal("shadow sent a release")
	}
	rows = e.rows("")
	if rows[0]["status"] != "released" || rows[0]["verdict"] != "REAL" || rows[1]["status"] != "suppressed" || rows[1]["reason"] != "BLIP: recovered" || rows[1]["closed_ts"] == nil {
		t.Fatalf("rows = %v", rows)
	}
	if len(e.rows("?state=pending")) != 0 {
		t.Fatal("pending lists closed entries")
	}
	// Closed entries leave the list after 24 hours.
	e.clock.add(25 * time.Hour)
	if len(e.rows("")) != 0 {
		t.Fatal("closed entries still listed after 24 h")
	}
}

func TestHoldBadRequests(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	id := e.rows("")[0]["id"].(string)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/held/release", `{"ids":[],"verdict":"REAL","title":"t","body":"b","tag":"general"}`, 400},
		{"/held/release", fmt.Sprintf(`{"ids":[%q],"verdict":"MAYBE","title":"t","body":"b","tag":"general"}`, id), 400},
		{"/held/release", fmt.Sprintf(`{"ids":[%q],"verdict":"REAL","title":"t","tag":"general"}`, id), 400},
		{"/held/release", `{"ids":["h-none"],"verdict":"REAL","title":"t","body":"b","tag":"general"}`, 409},
		{"/held/suppress", `not json`, 400},
		{"/held/suppress", `{"ids":["h-none"]}`, 409},
	} {
		if rec := e.req("POST", c.path, writeTok, "", "application/json", []byte(c.body)); rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.path, c.body, rec.Code, c.want)
		}
	}
}

func TestShadowDeadlineOnlyRecords(t *testing.T) {
	e := newHoldEnv(t, hold.Shadow)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	e.clock.add(31 * time.Minute)
	e.held("") // a poll, so the dead-man does not trip
	e.tick()
	if rows := e.rows(""); rows[0]["status"] != "expired" {
		t.Fatalf("rows = %v", rows)
	}
	if len(e.events()) != 1 {
		t.Fatal("shadow forwarded an entry that already pushed")
	}
}

func TestHoldSurvivesRestart(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	e.held("")
	e.st.Close()
	e.open(hold.On, "")
	rows := e.rows("?peek=1")
	if len(rows) != 1 || rows[0]["status"] != "held" {
		t.Fatalf("after restart: %v", rows)
	}
	if e.held("?peek=1")["last_poll_ts"] == nil {
		t.Fatal("the last poll was lost")
	}
}

// ---- the hold: on ------------------------------------------------------

func TestOnHeldNotPushedButSaysSent(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	rec := e.send(`{"title":"disk full","body":"b","tag":"alert"}`)
	sentOK(t, rec)
	if !strings.Contains(rec.Body.String(), "Held for triage as h-") {
		t.Fatalf("answer = %s", rec.Body)
	}
	ev := e.events()[0]
	if h := holdOf(ev); h["state"] != "held" || h["forwarded"] != false {
		t.Fatalf("hold = %v", h)
	}
	v := e.held("")
	if v["mode"] != "on" || v["oldest_unforwarded_ts"] == nil || v["last_poll_ts"] == nil {
		t.Fatalf("/held = %v", v)
	}
	// /v1/notify says it was not delivered, and names the entry.
	rec = e.req("POST", "/v1/notify", writeTok, "", "application/json", []byte(`{"body":"b","tag":"alert"}`))
	if v := jsonOf(t, rec); v["delivered"] != false || !strings.HasPrefix(fmt.Sprint(v["hold_id"]), "h-") {
		t.Fatalf("v1 = %v", v)
	}
	// /recent marks the held event undelivered.
	r := e.req("GET", "/recent?n=1", readTok, "", "", nil)
	var out []map[string]any
	json.Unmarshal(r.Body.Bytes(), &out)
	if out[0]["delivered"] != false || out[0]["hold"] != "held" {
		t.Fatalf("recent = %v", out)
	}
}

func TestOnReleaseSendsOneMessage(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	e.send(`{"title":"c","body":"d","tag":"alert"}`)
	var ids []string
	for _, r := range e.rows("") {
		ids = append(ids, r["id"].(string))
	}
	body, _ := json.Marshal(map[string]any{"ids": ids, "verdict": "REAL", "title": "web-1 disk full", "body": "Checked live.", "tag": "general"})
	rec := e.req("POST", "/v1/hold/release", writeTok, "", "application/json", body)
	v := jsonOf(t, rec)
	if rec.Code != 200 || v["sent"] != true || len(v["released"].([]any)) != 2 {
		t.Fatalf("release: %d %v", rec.Code, v)
	}
	evs := e.events()
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	rel := evs[2]
	h := holdOf(rel)
	if rel["source"] != "triage" || rel["level"] != "active" || h["state"] != "forward" || h["reason"] != "release" || h["verdict"] != "REAL" || len(h["ids"].([]any)) != 2 {
		t.Fatalf("release event = %v", rel)
	}
	// A second release of the same ids is a conflict.
	if rec := e.req("POST", "/held/release", writeTok, "", "application/json", body); rec.Code != 409 {
		t.Fatalf("second release: %d", rec.Code)
	}
	if v := e.held(""); v["oldest_unforwarded_ts"] != nil {
		t.Fatalf("released entries still count as unforwarded: %v", v)
	}
}

func TestOnDeadlineForwardsOriginalWithPrefix(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"disk full","body":"b","tag":"alert","source":"monitor","url":"https://example.com/x"}`)
	e.clock.add(29 * time.Minute)
	e.held("")
	e.tick()
	if len(e.events()) != 1 {
		t.Fatal("forwarded before the deadline")
	}
	e.clock.add(2 * time.Minute)
	e.tick()
	evs := e.events()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	f := evs[1]
	h := holdOf(f)
	if f["title"] != "[not triaged in 30 min] disk full" || f["source"] != "monitor" || f["level"] != "time-sensitive" || f["url"] != "https://example.com/x" || h["reason"] != "deadline" {
		t.Fatalf("forward = %v", f)
	}
	if rows := e.rows("?peek=1"); rows[0]["status"] != "expired" || rows[0]["forwarded"] != true {
		t.Fatalf("rows = %v", rows)
	}
	e.tick()
	if len(e.events()) != 2 {
		t.Fatal("forwarded twice")
	}
	// A late release answers 409.
	body := fmt.Sprintf(`{"ids":[%q],"verdict":"REAL","title":"t","body":"b","tag":"general"}`, h["ids"].([]any)[0])
	if rec := e.req("POST", "/held/release", writeTok, "", "application/json", []byte(body)); rec.Code != 409 {
		t.Fatalf("late release: %d", rec.Code)
	}
}

func TestOnDeadmanForwardsAndPassesThrough(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	// Nothing polls: after 20 minutes the dead-man trips.
	e.clock.add(21 * time.Minute)
	e.tick()
	evs := e.events()
	if len(evs) != 3 {
		t.Fatalf("events = %d, want held + notice + forward: %v", len(evs), evs)
	}
	if evs[1]["title"] != "alert hold off: triage not polling" || evs[1]["source"] != "heraldo-hold" {
		t.Fatalf("notice = %v", evs[1])
	}
	if evs[2]["title"] != "[triage not polling] a" || holdOf(evs[2])["reason"] != "deadman" {
		t.Fatalf("forward = %v", evs[2])
	}
	if v := e.held("?peek=1"); v["deadman_active"] != true {
		t.Fatalf("/held = %v", v)
	}
	// New alerts pass straight through, and the hold records them.
	rec := e.send(`{"title":"c","body":"d","tag":"alert"}`)
	if strings.Contains(rec.Body.String(), "Held") || holdOf(e.events()[3])["state"] != "passed" {
		t.Fatalf("not passed through: %s", rec.Body)
	}
	// A poll clears it; holding resumes.
	e.held("")
	e.tick()
	if rec := e.send(`{"title":"e","body":"f","tag":"alert"}`); !strings.Contains(rec.Body.String(), "Held") {
		t.Fatalf("holding did not resume: %s", rec.Body)
	}
	if !strings.Contains(e.log.String(), "dead-man cleared") {
		t.Fatal("no cleared line in the log")
	}
}

func TestPeekDoesNotStamp(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.held("?peek=1")
	if v := e.held("?peek=1"); v["last_poll_ts"] != nil {
		t.Fatalf("peek stamped a poll: %v", v)
	}
	e.held("")
	if v := e.held("?peek=1"); v["last_poll_ts"] == nil {
		t.Fatal("a poll was not stamped")
	}
}

func TestFlipToShadowDoesNotStrand(t *testing.T) {
	e := newHoldEnv(t, hold.On)
	e.send(`{"title":"a","body":"b","tag":"alert"}`)
	e.st.Close()
	e.open(hold.Shadow, "")
	e.held("")
	e.tick()
	evs := e.events()
	if len(evs) != 2 || evs[1]["title"] != "[triage not polling] a" {
		t.Fatalf("the on-mode entry was stranded: %v", evs)
	}
	if rows := e.rows("?peek=1"); rows[0]["forwarded"] != true {
		t.Fatalf("rows = %v", rows)
	}
}

func TestOffHoldsNothing(t *testing.T) {
	e := newHoldEnv(t, hold.Off)
	rec := e.send(`{"title":"a","body":"b","tag":"alert"}`)
	sentOK(t, rec)
	if strings.Contains(rec.Body.String(), "Held") || holdOf(e.events()[0]) != nil || len(e.rows("")) != 0 {
		t.Fatal("off mode held an alert")
	}
}

// TestNativeHoldStates walks each path of the native hold and checks which
// stored events push, as TestBridgedHoldStates does for the tee's shapes.
// Only an alert the hold keeps back stays quiet; a bypass, a passed alert
// and every forward push at their level.
func TestNativeHoldStates(t *testing.T) {
	release := func(e *holdEnv) {
		var ids []string
		for _, r := range e.rows("") {
			ids = append(ids, r["id"].(string))
		}
		body, _ := json.Marshal(map[string]any{"ids": ids, "verdict": "REAL", "title": "t", "body": "b", "tag": "general"})
		if rec := e.req("POST", "/v1/hold/release", writeTok, "", "application/json", body); rec.Code != 200 {
			e.t.Fatalf("release: %d %s", rec.Code, rec.Body)
		}
	}
	for _, c := range []struct {
		name string
		mode hold.Mode
		run  func(e *holdEnv)
		push []bool // per stored event, oldest first
	}{
		{"held on", hold.On, func(e *holdEnv) { e.send(`{"title":"a","body":"b","tag":"alert"}`) }, []bool{false}},
		{"held shadow", hold.Shadow, func(e *holdEnv) { e.send(`{"title":"a","body":"b","tag":"alert"}`) }, []bool{true}},
		{"bypass", hold.On, func(e *holdEnv) { e.send(`{"title":"a","body":"b","tag":"page"}`) }, []bool{true}},
		{"release forward", hold.On, func(e *holdEnv) {
			e.send(`{"title":"a","body":"b","tag":"alert"}`)
			release(e)
		}, []bool{false, true}},
		{"deadline forward", hold.On, func(e *holdEnv) {
			e.send(`{"title":"a","body":"b","tag":"alert"}`)
			e.clock.add(15 * time.Minute)
			e.held("")
			e.clock.add(16 * time.Minute)
			e.tick()
		}, []bool{false, true}},
		{"dead-man notice, forward and pass-through", hold.On, func(e *holdEnv) {
			e.send(`{"title":"a","body":"b","tag":"alert"}`)
			e.clock.add(21 * time.Minute)
			e.tick()
			e.send(`{"title":"c","body":"d","tag":"alert"}`)
		}, []bool{false, true, true, true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newHoldEnv(t, c.mode)
			c.run(e)
			ctx := context.Background()
			recs, err := e.st.List(ctx, store.Filter{Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != len(c.push) {
				t.Fatalf("events = %d, want %d", len(recs), len(c.push))
			}
			for i, rec := range recs {
				var ev api.Event
				if err := json.Unmarshal(rec.Event, &ev); err != nil {
					t.Fatal(err)
				}
				d := push.Decide(&ev, e.clock.now())
				if d.Push != c.push[i] {
					t.Fatalf("event %d pushes %v (%s), want %v: %s", i, d.Push, d.Reason, c.push[i], rec.Event)
				}
				if c.push[i] && ev.Hold != nil {
					if so, _ := (*ev.Hold)["store_only"].(bool); so {
						t.Fatalf("event %d store_only: %s", i, rec.Event)
					}
				}
				if got, err := e.st.Pushes(ctx, rec.Seq); err != nil || got != c.push[i] {
					t.Fatalf("event %d badge rule %v %v, want %v", i, got, err, c.push[i])
				}
			}
		})
	}
}
