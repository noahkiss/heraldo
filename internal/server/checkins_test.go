package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

func (e *env) tick(t *testing.T) {
	t.Helper()
	e.h.(*Server).s.tickCheckins(context.Background())
}

func (e *env) ping(t *testing.T, path, body string) int {
	t.Helper()
	method := http.MethodGet
	var b []byte
	if body != "" {
		method, b = http.MethodPost, []byte(body)
	}
	rec := e.do(t, method, path, writeTok, b)
	if rec.Code == http.StatusOK && rec.Body.String() != "OK" {
		t.Fatalf("%s: body %q", path, rec.Body.String())
	}
	return rec.Code
}

func (e *env) check(t *testing.T, slug string) api.Check {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/v1/checkins/"+slug, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get %s: %d %s", slug, rec.Code, rec.Body.String())
	}
	return decode[api.Check](t, rec)
}

// incidents returns the incident events for key, oldest first.
func (e *env) incidents(t *testing.T, key string) []api.Event {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/v1/events?kind=incident&key="+key, readTok, nil)
	var p struct{ Events []api.Event }
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Events
}

func actions(evs []api.Event) string {
	var out []string
	for _, e := range evs {
		level := "-"
		if e.Level != nil {
			level = string(*e.Level)
		}
		out = append(out, string(*e.Action)+"/"+level)
	}
	return strings.Join(out, " ")
}

func TestPingUnknownAndCreate(t *testing.T) {
	e := newEnv(t)
	if c := e.ping(t, "/ping/nightly", ""); c != http.StatusNotFound {
		t.Fatalf("unknown: %d", c)
	}
	if c := e.ping(t, "/ping/nightly?create=1&period=1h&grace=600&tz=UTC", ""); c != http.StatusBadRequest {
		t.Fatalf("tz without cron: %d", c)
	}
	if c := e.ping(t, "/ping/nightly?create=1&period=1h&grace=600&host=box-1", ""); c != http.StatusOK {
		t.Fatalf("create: %d", c)
	}
	c := e.check(t, "nightly")
	if c.State != api.CheckStateUp || *c.Schedule.PeriodS != 3600 || *c.Schedule.GraceS != 600 || *c.Host != "box-1" {
		t.Fatalf("created: %+v", c)
	}
	if !c.NextDue.Equal(e.clock.now().Add(time.Hour)) || !c.DownAfter.Equal(e.clock.now().Add(70*time.Minute)) {
		t.Fatalf("due: %v %v", c.NextDue, c.DownAfter)
	}
	// create on a known check changes nothing about its schedule.
	e.ping(t, "/ping/nightly?create=1&period=5m", "")
	if c := e.check(t, "nightly"); *c.Schedule.PeriodS != 3600 {
		t.Fatalf("schedule changed: %+v", c.Schedule)
	}
	for _, bad := range []string{"/ping/nightly/7000", "/ping/nightly/01", "/ping/nightly/stop"} {
		if c := e.ping(t, bad, ""); c != http.StatusNotFound {
			t.Errorf("%s: %d", bad, c)
		}
	}
	if c := e.ping(t, "/ping/nightly?colour=red", ""); c != http.StatusBadRequest {
		t.Errorf("unknown parameter: %d", c)
	}
	if c := e.ping(t, "/ping/bad%20slug?create=1", ""); c != http.StatusBadRequest {
		t.Errorf("bad slug: %d", c)
	}
}

func TestPingAuth(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodGet, "/ping/a?create=1", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/ping/a?create=1", readTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", rec.Code)
	}
	// A write token in the URL is refused: a URL-only client uses a ping key.
	if rec := e.do(t, http.MethodGet, "/ping/a?create=1&token="+writeTok, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodPut, "/v1/checkins/a", readTok, []byte(`{}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("put with read token: %d", rec.Code)
	}
}

func TestPingRunAndLog(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/backup/start?create=1&rid=r1", "")
	if c := e.check(t, "backup"); c.State != api.CheckStateStarted || c.StartedAt == nil {
		t.Fatalf("start: %+v", c)
	}
	e.clock.add(42 * time.Second)
	e.ping(t, "/ping/backup?rid=r1", "copied 3 files\n")
	c := e.check(t, "backup")
	if c.State != api.CheckStateUp || *c.LastDurationMs != 42000 || *c.LastLog != "copied 3 files\n" || c.StartedAt != nil {
		t.Fatalf("success: %+v", c)
	}
	e.clock.add(time.Minute)
	e.ping(t, "/ping/backup/log", "still fine")
	if c := e.check(t, "backup"); c.State != api.CheckStateUp || *c.LastLog != "still fine" {
		t.Fatalf("log: %+v", c)
	}
	// An exit code: 0 succeeds, anything else fails, and the event says so.
	e.ping(t, "/ping/backup/3", "disk full")
	c = e.check(t, "backup")
	if c.State != api.CheckStateDown || *c.DownReason != api.CheckDownReasonFailed {
		t.Fatalf("exit 3: %+v", c)
	}
	ev := decode[api.Event](t, e.do(t, http.MethodGet, "/v1/events/"+*c.LastEvent, readTok, nil))
	if *ev.Action != api.EventActionFail || *ev.Checkin.ExitCode != 3 || *ev.Title != "backup failed (exit 3)" || *ev.Body != "disk full" || ev.Source != "checkin" {
		t.Fatalf("fail event: %+v", ev)
	}
	e.ping(t, "/ping/backup/0", "")
	if c := e.check(t, "backup"); c.State != api.CheckStateUp {
		t.Fatalf("exit 0: %+v", c)
	}
}

func TestPingLogTruncated(t *testing.T) {
	e := newEnv(t)
	// A multi-byte character straddles the limit, so the cut moves back.
	body := strings.Repeat("a", LogLimit-1) + "é" + strings.Repeat("b", 5000)
	if c := e.ping(t, "/ping/big?create=1", body); c != http.StatusOK {
		t.Fatalf("ping: %d", c)
	}
	c := e.check(t, "big")
	if len(*c.LastLog) != LogLimit-1 {
		t.Fatalf("log length %d", len(*c.LastLog))
	}
	ev := decode[map[string]any](t, e.do(t, http.MethodGet, "/v1/events/"+*c.LastEvent, readTok, nil))
	if ev["details"].(map[string]any)["heraldo"].(map[string]any)["log_truncated"] != true {
		t.Fatalf("details: %v", ev["details"])
	}
}

// The acceptance test: a job that stops pinging goes late at its period,
// then down with an incident when the grace runs out; the next success
// resolves it.
func TestMissedPingOpensAndResolves(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/job?create=1&period=1h&grace=10m", "")
	e.clock.add(59 * time.Minute)
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateUp {
		t.Fatalf("before due: %s", c.State)
	}
	e.clock.add(time.Minute)
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateLate {
		t.Fatalf("due: %s", c.State)
	}
	if evs := e.incidents(t, "checkin:job"); len(evs) != 0 {
		t.Fatalf("late raised %s", actions(evs))
	}
	e.clock.add(10 * time.Minute)
	e.tick(t)
	c := e.check(t, "job")
	if c.State != api.CheckStateDown || *c.DownReason != api.CheckDownReasonMissed || *c.Incident != api.CheckIncidentOpen {
		t.Fatalf("down: %+v", c)
	}
	evs := e.incidents(t, "checkin:job")
	if actions(evs) != "open/active" || *evs[0].Title != "job is down" || evs[0].Source != "heraldo" {
		t.Fatalf("incident: %s %+v", actions(evs), evs)
	}
	// Another tick raises nothing new.
	e.tick(t)
	if n := len(e.incidents(t, "checkin:job")); n != 1 {
		t.Fatalf("%d incident events", n)
	}
	e.ping(t, "/ping/job", "")
	if c := e.check(t, "job"); c.State != api.CheckStateUp || *c.Incident != api.CheckIncidentResolved {
		t.Fatalf("recovered: %+v", c)
	}
	if got := actions(e.incidents(t, "checkin:job")); got != "open/active resolve/passive" {
		t.Fatalf("incidents: %s", got)
	}
}

// A check made without a ping is due at its first slot after creation; if
// no ping comes it goes down, never pinged.
func TestNeverPingedOpens(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPut, "/v1/checkins/job", writeTok, []byte(`{"period_s":3600,"grace_s":600}`)); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if c := e.check(t, "job"); c.State != api.CheckStateNew || c.DownAfter == nil || !c.DownAfter.Equal(e.clock.now().Add(70*time.Minute)) {
		t.Fatalf("new: %+v", c)
	}
	e.clock.add(time.Hour)
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateLate {
		t.Fatalf("first slot: %s", c.State)
	}
	e.clock.add(10 * time.Minute)
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateDown || *c.DownReason != api.CheckDownReasonMissed {
		t.Fatalf("down: %+v", c)
	}
	evs := e.incidents(t, "checkin:job")
	if actions(evs) != "open/active" || *evs[0].Title != "job never pinged" {
		t.Fatalf("incident: %s %+v", actions(evs), evs)
	}
	e.ping(t, "/ping/job", "")
	if got := actions(e.incidents(t, "checkin:job")); got != "open/active resolve/passive" {
		t.Fatalf("first ping: %s", got)
	}
}

func TestReminders(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/job?create=1&period=1h&grace=0&reminder=2h", "")
	e.clock.add(time.Hour)
	e.tick(t)
	e.clock.add(119 * time.Minute)
	e.tick(t)
	if got := actions(e.incidents(t, "checkin:job")); got != "open/active" {
		t.Fatalf("before the reminder: %s", got)
	}
	e.clock.add(time.Minute)
	e.tick(t)
	e.tick(t)
	evs := e.incidents(t, "checkin:job")
	if actions(evs) != "open/active update/active" || *evs[1].Title != "job is still down" {
		t.Fatalf("reminder: %s", actions(evs))
	}
	// An acked incident gets no more reminders.
	e.post(t, `{"source":"me","kind":"incident","key":"checkin:job","action":"ack"}`)
	e.clock.add(5 * time.Hour)
	e.tick(t)
	if got := actions(e.incidents(t, "checkin:job")); got != "open/active update/active ack/-" {
		t.Fatalf("after ack: %s", got)
	}
}

func TestMaxRuntime(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/sync/start?create=1&period=24h&max_runtime=30m", "")
	e.clock.add(30 * time.Minute)
	e.tick(t)
	c := e.check(t, "sync")
	if c.State != api.CheckStateDown || *c.DownReason != api.CheckDownReasonOverran {
		t.Fatalf("overran: %+v", c)
	}
	evs := e.incidents(t, "checkin:sync")
	if actions(evs) != "open/active" || *evs[0].Title != "sync is running too long" {
		t.Fatalf("incident: %s", actions(evs))
	}
}

// A scheduled check's failure opens a quiet incident (the fail event already
// pushed), and its next success resolves it. An unscheduled check, such as
// one an ingest rule makes from an OnFailure post, gets no incident.
func TestFailIncident(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/sched?create=1&period=1h", "")
	e.ping(t, "/ping/sched/fail", "")
	if got := actions(e.incidents(t, "checkin:sched")); got != "open/passive" {
		t.Fatalf("scheduled fail: %s", got)
	}
	e.ping(t, "/ping/sched", "")
	if got := actions(e.incidents(t, "checkin:sched")); got != "open/passive resolve/passive" {
		t.Fatalf("scheduled success: %s", got)
	}
	e.post(t, `{"source":"rule","kind":"checkin","key":"job:box:unit","action":"fail"}`)
	e.tick(t)
	if c := e.check(t, "job:box:unit"); c.State != api.CheckStateDown || c.Incident != nil {
		t.Fatalf("unscheduled fail: %+v", c)
	}
}

func TestPauseResume(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/job?create=1&period=1h&grace=0", "")
	e.clock.add(time.Hour)
	e.tick(t)
	rec := e.do(t, http.MethodPost, "/v1/checkins/job/pause", writeTok, nil)
	if rec.Code != http.StatusOK || decode[api.Check](t, rec).State != api.CheckStatePaused {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	evs := e.incidents(t, "checkin:job")
	if actions(evs) != "open/active resolve/passive" || *evs[1].Title != "job is paused" {
		t.Fatalf("pause resolves: %s", actions(evs))
	}
	// Pings while paused are recorded; the check stays paused and its
	// failures are quiet.
	e.clock.add(time.Minute)
	e.ping(t, "/ping/job/fail", "maintenance")
	c := e.check(t, "job")
	if c.State != api.CheckStatePaused || !c.LastPing.Equal(e.clock.now()) || *c.LastLog != "maintenance" {
		t.Fatalf("paused ping: %+v", c)
	}
	ev := decode[api.Event](t, e.do(t, http.MethodGet, "/v1/events/"+*c.LastEvent, readTok, nil))
	if *ev.Level != api.LevelPassive {
		t.Fatalf("paused fail level: %v", *ev.Level)
	}
	e.clock.add(48 * time.Hour)
	e.tick(t)
	if n := len(e.incidents(t, "checkin:job")); n != 2 {
		t.Fatalf("paused check raised: %d", n)
	}
	rec = e.do(t, http.MethodPost, "/v1/checkins/job/resume", writeTok, nil)
	if c := decode[api.Check](t, rec); c.State != api.CheckStateNew || c.NextDue != nil {
		t.Fatalf("resume: %+v", c)
	}
	if rec := e.do(t, http.MethodPost, "/v1/checkins/nope/pause", writeTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("pause unknown: %d", rec.Code)
	}
}

func TestPutAndListChecks(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPut, "/v1/checkins/weekly", writeTok, []byte(`{"cron":"0 4 * * 1","tz":"Europe/Madrid","grace_s":7200,"host":"nas"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	c := decode[api.Check](t, rec)
	if c.State != api.CheckStateNew || *c.Schedule.Cron != "0 4 * * 1" || *c.Schedule.ReminderS != 86400 || c.NextDue != nil {
		t.Fatalf("created: %+v", c)
	}
	// 2026-10-27 is a Tuesday; the next Monday 04:00 in Madrid is 03:00 UTC.
	e.ping(t, "/ping/weekly", "")
	c = e.check(t, "weekly")
	if want := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC); !c.NextDue.Equal(want) {
		t.Fatalf("next due %v, want %v", c.NextDue, want)
	}
	for _, bad := range []string{`{"cron":"nope"}`, `{"period_s":60,"cron":"* * * * *"}`, `{"tz":"UTC"}`, `{"colour":1}`, `{"period_s":0}`} {
		if rec := e.do(t, http.MethodPut, "/v1/checkins/weekly", writeTok, []byte(bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	// A new schedule moves the due time from the last run.
	rec = e.do(t, http.MethodPut, "/v1/checkins/weekly", writeTok, []byte(`{"period_s":600}`))
	if c := decode[api.Check](t, rec); rec.Code != http.StatusOK || !c.NextDue.Equal(e.clock.now().Add(10*time.Minute)) || c.Host != nil {
		t.Fatalf("replace: %d %+v", rec.Code, c)
	}
	e.ping(t, "/ping/a?create=1", "")
	list := decode[api.CheckList](t, e.do(t, http.MethodGet, "/v1/checkins", readTok, nil))
	if len(list.Checkins) != 2 || list.Checkins[0].Key != "a" || list.Checkins[1].Key != "weekly" {
		t.Fatalf("list: %+v", list)
	}
	if rec := e.do(t, http.MethodGet, "/v1/checkins/none", readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown: %d", rec.Code)
	}
}

// After a restart the scheduler judges from the database alone, and gives
// every check its grace from the restart before a missed ping counts.
func TestRestartKeepsState(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/job?create=1&period=1h&grace=10m", "")
	e.clock.add(5 * time.Hour)
	old := e.h.(*Server).s
	restarted := New(Options{Store: e.st, Tokens: old.Tokens, Validator: old.Validator, Now: e.clock.now, Logger: old.Logger})
	e.h = restarted
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateLate {
		t.Fatalf("at restart: %s", c.State)
	}
	e.clock.add(10 * time.Minute)
	e.tick(t)
	if c := e.check(t, "job"); c.State != api.CheckStateDown || *c.Incident != api.CheckIncidentOpen {
		t.Fatalf("after the start-up grace: %+v", c)
	}
}

func TestPingKey(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/job?create=1&period=1h", "")
	e.ping(t, "/ping/other?create=1", "")
	if rec := e.do(t, http.MethodPost, "/v1/checkins/job/key", readTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("mint with a read token: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPost, "/v1/checkins/nope/key", writeTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("mint for an unknown check: %d", rec.Code)
	}
	rec := e.do(t, http.MethodPost, "/v1/checkins/job/key", writeTok, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	k := decode[api.PingKey](t, rec)
	if len(k.Key) != 32 || k.Path != "/p/"+k.Key {
		t.Fatalf("key: %+v", k)
	}
	if c := e.check(t, "job"); c.PingKeyAt == nil {
		t.Fatalf("no ping_key_at: %+v", c)
	}
	// The key alone pings its check, with no bearer token.
	e.clock.add(time.Minute)
	if c := e.ping(t, k.Path+"/start?rid=r", ""); c != http.StatusOK {
		t.Fatalf("key start: %d", c)
	}
	e.clock.add(time.Minute)
	if rec := e.do(t, http.MethodPost, k.Path+"/0?rid=r", "", []byte("done")); rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Fatalf("key exit 0: %d %s", rec.Code, rec.Body.String())
	}
	c := e.check(t, "job")
	if c.State != api.CheckStateUp || *c.LastDurationMs != 60000 || *c.LastLog != "done" {
		t.Fatalf("after key pings: %+v", c)
	}
	if o := e.check(t, "other"); o.LastPing.After(c.CreatedAt.Add(time.Second)) {
		t.Fatalf("the key pinged another check: %+v", o)
	}
	// A key cannot create checks, and bad or unknown keys get 404.
	if c := e.ping(t, k.Path+"?create=1", ""); c != http.StatusBadRequest {
		t.Fatalf("create by key: %d", c)
	}
	for _, p := range []string{"/p/short", "/p/" + strings.Repeat("A", 32), "/p/" + strings.Repeat("A", 32) + "/fail"} {
		if rec := e.do(t, http.MethodGet, p, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	// Minting again rotates: the old key stops working.
	k2 := decode[api.PingKey](t, e.do(t, http.MethodPost, "/v1/checkins/job/key", writeTok, nil))
	if rec := e.do(t, http.MethodGet, k.Path, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("old key after rotate: %d", rec.Code)
	}
	if c := e.ping(t, k2.Path, ""); c != http.StatusOK {
		t.Fatalf("new key: %d", c)
	}
	// A tick keeps the key (it writes the whole row).
	e.clock.add(2 * time.Hour)
	e.tick(t)
	if c := e.ping(t, k2.Path, ""); c != http.StatusOK {
		t.Fatalf("key after a tick: %d", c)
	}
	rec = e.do(t, http.MethodDelete, "/v1/checkins/job/key", writeTok, nil)
	if rec.Code != http.StatusOK || decode[api.Check](t, rec).PingKeyAt != nil {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, k2.Path, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("removed key: %d", rec.Code)
	}
}

func TestRedactPath(t *testing.T) {
	for in, want := range map[string]string{
		"/p/abc":        "/p/<key>",
		"/p/abc/start":  "/p/<key>/start",
		"/ping/a/start": "/ping/a/start",
		"/v1/events":    "/v1/events",
	} {
		if got := redactPath(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// A ping key never reaches the log, however the path is spelled, and a
// ping on a non-canonical /p path is served, not redirected.
func TestPingKeyOddPaths(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.h.(*Server).s.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	e.ping(t, "/ping/job?create=1", "")
	k := decode[api.PingKey](t, e.do(t, http.MethodPost, "/v1/checkins/job/key", writeTok, nil)).Key
	for _, p := range []string{"//p/" + k, "/p//" + k, "/./p/" + k, "/p/" + k + "/", "/x/../p/" + k + "/log", "/p/" + k + "/../" + k} {
		rec := e.do(t, http.MethodGet, p, "", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	e.do(t, http.MethodGet, "/p/"+k+"/../../v1/x", "", nil)
	if strings.Contains(buf.String(), k) {
		t.Fatalf("key in the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "path=/p/<key>/log") {
		t.Fatalf("log: %s", buf.String())
	}
}

// A long run that sent /start raises nothing while it runs.
func TestLongRunNoIncident(t *testing.T) {
	e := newEnv(t)
	e.ping(t, "/ping/nightly?create=1&period=1h&grace=10m", "")
	e.clock.add(55 * time.Minute)
	e.ping(t, "/ping/nightly/start", "")
	// Past the old due time plus grace (70 min), before the start's own
	// deadline (start + period + grace).
	e.clock.add(60 * time.Minute)
	e.tick(t)
	c := e.check(t, "nightly")
	if c.State != api.CheckStateStarted || c.Incident != nil {
		t.Fatalf("while running: %+v", c)
	}
	if want := e.clock.now().Add(-60 * time.Minute).Add(70 * time.Minute); !c.DownAfter.Equal(want) {
		t.Fatalf("down_after %v, want %v", c.DownAfter, want)
	}
}
