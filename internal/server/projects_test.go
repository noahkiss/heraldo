package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const projectRules = `
rules:
  - name: unit-failed
    match: {title: '^(?P<host>[\w.-]+): unit failed'}
    set: {kind: incident, action: open, key: 'unit:{host}', thread: units, project: Fleet}
  - name: printer-ping
    match: {kind: '^checkin$', key: '^printer-'}
    set: {project: printing}
projects:
  - match: {source: '^printer$'}
    project: printing
  - name: by-thread
    match: {thread: '^backups$'}
    project: fleet
  - match: {source: '^team-(?P<team>[a-z]+)$'}
    project: '{team}'
aliases:
  infra: fleet
`

// newProjectEnv is newEnv with projectRules.
func newProjectEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(projectRules), 0o600); err != nil {
		t.Fatal(err)
	}
	rw, err := rules.NewWatcher(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return newEnvRules(t, rw)
}

// projectOf posts body and returns the stored event's project.
func projectOf(t *testing.T, e *env, body string) any {
	t.Helper()
	rec := e.post(t, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post %s: %d %s", body, rec.Code, rec.Body)
	}
	return decode[map[string]any](t, rec)["project"]
}

// listed returns the titles GET target lists.
func listed(t *testing.T, e *env, target string) []string {
	t.Helper()
	rec := e.do(t, http.MethodGet, target, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
	}
	var out []string
	for _, ev := range decode[api.EventPage](t, rec).Events {
		out = append(out, deref(ev.Title))
	}
	return out
}

func TestProjectFoldsAndFilters(t *testing.T) {
	e := newEnv(t)
	if p := projectOf(t, e, `{"source":"s","title":"one","project":"Web"}`); p != "web" {
		t.Fatalf("project = %v, want web", p)
	}
	projectOf(t, e, `{"source":"s","title":"two","project":"fleet"}`)
	projectOf(t, e, `{"source":"s","title":"three"}`)
	for _, bad := range []string{`"web/x"`, `"-web"`, `""`, `"` + strings.Repeat("p", 65) + `"`} {
		rec := e.post(t, `{"source":"s","title":"t","project":`+bad+`}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "project") {
			t.Fatalf("project %s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	for target, want := range map[string]string{
		"/v1/events?project=web":               "one",
		"/v1/events?project=WEB,fleet":         "one,two",
		"/v1/events?project=web&project=fleet": "one,two",
		"/v1/events?project=-":                 "three",
		"/v1/events?project=fleet,-":           "two,three",
		"/v1/events?project=nobody":            "",
	} {
		if got := strings.Join(listed(t, e, target), ","); got != want {
			t.Errorf("%s = %q, want %q", target, got, want)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/events?project=a/b", readTok, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad filter: %d", rec.Code)
	}
	rec := e.do(t, http.MethodGet, "/v1/send?source=s&title=sent&project=Web&token="+writeTok, "", nil)
	if rec.Code != http.StatusCreated || decode[map[string]any](t, rec)["project"] != "web" {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
}

func TestProjectRulesMapAndAliases(t *testing.T) {
	e := newProjectEnv(t)
	for _, c := range []struct{ body, want string }{
		// A typing rule's set.project, folded.
		{`{"source":"mon","title":"web-1: unit failed"}`, "fleet"},
		// The sender's project wins over the rule's.
		{`{"source":"mon","title":"web-2: unit failed","project":"web"}`, "web"},
		// The map runs on an explicit kind too.
		{`{"source":"printer","kind":"message","title":"done"}`, "printing"},
		// The map matches the thread.
		{`{"source":"nightly","title":"backup ok","thread":"backups"}`, "fleet"},
		// A capture in the map.
		{`{"source":"team-ops","title":"hi"}`, "ops"},
		// An alias is stored as its target.
		{`{"source":"s","title":"old name","project":"infra"}`, "fleet"},
	} {
		if p := projectOf(t, e, c.body); p != c.want {
			t.Errorf("%s: project = %v, want %s", c.body, p, c.want)
		}
	}
	if p := projectOf(t, e, `{"source":"nobody","title":"x"}`); p != nil {
		t.Errorf("unmatched: project = %v", p)
	}

	// An event stored under the old name before the alias existed.
	v, _ := validate.New()
	in, err := v.Fields(map[string]any{"source": "s", "kind": "message", "title": "stored as infra", "project": "infra"})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.now()
	id := ids.New(now)
	in.Fields["id"], in.Fields["received_at"] = id, now.Format("2006-01-02T15:04:05Z")
	if _, _, err := e.st.Insert(context.Background(), store.NewEvent{ID: id, ReceivedAt: now, Fields: in.Fields, Typed: in.Typed}); err != nil {
		t.Fatal(err)
	}
	got := listed(t, e, "/v1/events?project=fleet")
	if len(got) != 4 || got[3] != "stored as infra" {
		t.Fatalf("fleet = %q", got)
	}
	if got := listed(t, e, "/v1/events?project=infra"); len(got) != 4 {
		t.Fatalf("an alias filters as its target: %q", got)
	}

	rec := e.do(t, http.MethodGet, "/v1/projects", readTok, nil)
	pl := decode[api.ProjectList](t, rec)
	counts := map[string]int{}
	for _, p := range pl.Projects {
		counts[p.Name] = p.Events
	}
	if counts["fleet"] != 4 || counts["infra"] != 0 || counts["printing"] != 1 || counts["web"] != 1 || pl.Unfiled != 1 {
		t.Fatalf("projects = %+v", pl)
	}
}

func TestProjectPingsAndChecks(t *testing.T) {
	e := newProjectEnv(t)
	ping := func(target string) {
		t.Helper()
		if rec := e.do(t, http.MethodGet, target, writeTok, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
	ping("/ping/printer-1?create=1")
	ping("/ping/job-a?create=1&project=Batch")
	// No project: the check's key keeps its project.
	ping("/ping/job-a")
	ping("/ping/job-b?create=1")
	evs := decode[api.EventPage](t, e.do(t, http.MethodGet, "/v1/events?kind=checkin", readTok, nil)).Events
	var got []string
	for _, ev := range evs {
		got = append(got, *ev.Key+"="+deref(ev.Project))
	}
	if strings.Join(got, " ") != "printer-1=printing job-a=batch job-a=batch job-b=" {
		t.Fatalf("pings = %q", got)
	}
	cl := decode[api.CheckList](t, e.do(t, http.MethodGet, "/v1/checkins?project=batch", readTok, nil))
	if len(cl.Checkins) != 1 || cl.Checkins[0].Key != "job-a" || deref(cl.Checkins[0].Project) != "batch" {
		t.Fatalf("checkins = %+v", cl.Checkins)
	}
	cl = decode[api.CheckList](t, e.do(t, http.MethodGet, "/v1/checkins?project=-", readTok, nil))
	if len(cl.Checkins) != 1 || cl.Checkins[0].Key != "job-b" {
		t.Fatalf("checkins with none = %+v", cl.Checkins)
	}

	// A scheduled check that fails opens its incident under the check's
	// project.
	ping("/ping/job-c/fail?create=1&period=1h&project=batch")
	st := decode[statePage](t, e.do(t, http.MethodGet, "/v1/state?project=batch", readTok, nil))
	if len(st.Incidents) != 1 || len(st.Checkins) != 2 {
		t.Fatalf("state = %d incidents, %d checkins", len(st.Incidents), len(st.Checkins))
	}
	var inc map[string]any
	json.Unmarshal(st.Incidents[0], &inc)
	if inc["key"] != "checkin:job-c" || inc["project"] != "batch" {
		t.Fatalf("incident = %v", inc)
	}
}

func TestProjectKeyContinuity(t *testing.T) {
	e := newEnv(t)
	projectOf(t, e, `{"source":"mon","kind":"incident","key":"k1","action":"open","title":"down","project":"web"}`)
	if p := projectOf(t, e, `{"source":"cli","kind":"incident","key":"k1","action":"ack"}`); p != "web" {
		t.Fatalf("ack: project = %v", p)
	}
	st := decode[statePage](t, e.do(t, http.MethodGet, "/v1/state?project=web", readTok, nil))
	if len(st.Incidents) != 1 {
		t.Fatalf("state web = %d incidents", len(st.Incidents))
	}
	if st := decode[statePage](t, e.do(t, http.MethodGet, "/v1/state?project=fleet", readTok, nil)); len(st.Incidents) != 0 {
		t.Fatalf("state fleet = %d incidents", len(st.Incidents))
	}
	if p := projectOf(t, e, `{"source":"other","kind":"incident","key":"k1","action":"resolve"}`); p != "web" {
		t.Fatalf("resolve: project = %v", p)
	}
	// A key with no project stays without one.
	if p := projectOf(t, e, `{"source":"cli","kind":"incident","key":"k2","action":"ack"}`); p != nil {
		t.Fatalf("k2: project = %v", p)
	}
	// Status and progress keep their key's project too.
	projectOf(t, e, `{"source":"s","kind":"status","key":"st","action":"set","content":{"status":"1"},"project":"web"}`)
	if p := projectOf(t, e, `{"source":"s","kind":"status","key":"st","action":"set","content":{"status":"2"}}`); p != "web" {
		t.Fatalf("status: project = %v", p)
	}
	if st := decode[statePage](t, e.do(t, http.MethodGet, "/v1/state?project=web", readTok, nil)); len(st.Statuses) != 1 {
		t.Fatalf("state web = %d statuses", len(st.Statuses))
	}
}

func TestProjectCompatPaths(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/v1/notify", writeTok, []byte(`{"body":"b","project":"Web"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("notify: %d %s", rec.Code, rec.Body)
	}
	// A bad name is dropped; the message is stored all the same.
	if rec := e.do(t, http.MethodPost, "/v1/notify", writeTok, []byte(`{"body":"b2","project":"no good"}`)); rec.Code != http.StatusOK {
		t.Fatalf("notify bad project: %d %s", rec.Code, rec.Body)
	}
	evs := decode[api.EventPage](t, e.do(t, http.MethodGet, "/v1/events", readTok, nil)).Events
	if len(evs) != 2 || deref(evs[0].Project) != "web" || evs[1].Project != nil {
		t.Fatalf("notify events = %+v", evs)
	}
	entry := jsonBody(t, e.do(t, http.MethodGet, "/entry/"+evs[0].Id, readTok, nil))
	if entry["project"] != "web" {
		t.Fatalf("entry = %v", entry)
	}

	// Notify!: a start names the project, an update keeps it.
	rec = e.do(t, http.MethodPost, "/live-activity/dev-1?new=1&token="+writeTok, "", []byte(`{"title":"Print","progress":10,"project":"printing"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	act := jsonBody(t, rec)["activityId"].(string)
	if rec := e.do(t, http.MethodPost, "/live-activity/"+act+"?token="+writeTok, "", []byte(`{"progress":50}`)); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, http.MethodPost, "/notify-json/dev-1?token="+writeTok, "", []byte(`{"text":"done","project":"printing"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("notify-json: %d %s", rec.Code, rec.Body)
	}
	got := listed(t, e, "/v1/events?project=printing")
	if len(got) != 3 {
		t.Fatalf("printing = %q", got)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func jsonBody(t *testing.T, rec interface{ Result() *http.Response }) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(rec.Result().Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
