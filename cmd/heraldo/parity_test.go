package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/server"
)

func (f *fixture) req(method, path, tok, body string) (int, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMuteAndMutes(t *testing.T) {
	f := newFixture(t)
	if r := f.run("mute", "--source", "noisy"); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN is not set") {
		t.Fatalf("without a write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("mute"); r.code != 1 || !strings.Contains(r.stderr, "--source, --key, --project") {
		t.Fatalf("no target: %+v", r)
	}
	if r := f.run("mutes"); r.code != 0 || !strings.Contains(r.stderr, "no mutes in force") {
		t.Fatalf("empty: %+v", r)
	}
	r := f.run("mute", "--source", "noisy", "--for", "2h")
	if r.code != 0 || !strings.Contains(r.stdout, "muted source=noisy until") {
		t.Fatalf("mute: %+v", r)
	}
	id := strings.TrimSpace(strings.Split(strings.TrimSpace(r.stdout), "\n")[1])
	if !strings.HasPrefix(id, "mute_") {
		t.Fatalf("id %q", id)
	}
	if r := f.run("mutes", "ls"); r.code != 0 || !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, "source=noisy") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("mutes", "end", id); r.code != 0 || !strings.Contains(r.stdout, "ended "+id) {
		t.Fatalf("end: %+v", r)
	}
	if r := f.run("mutes", "--json"); r.code != 0 || r.stdout != "" {
		t.Fatalf("after end: %+v", r)
	}
	if r := f.run("mutes", "end", "mute_nope"); r.code != 1 || !strings.Contains(r.stderr, "404") {
		t.Fatalf("unknown: %+v", r)
	}
}

func TestHoldQueue(t *testing.T) {
	f := newFixtureWith(t, func(o *server.Options) { o.Hold.Mode = hold.On })
	for _, body := range []string{
		`{"title":"web-1 down","body":"1","tag":"alert"}`,
		`{"title":"web-2 down","body":"2","tag":"alert"}`,
	} {
		if code, b := f.req(http.MethodPost, "/v1/notify", writeTok, body); code != http.StatusOK {
			t.Fatalf("notify: %d %s", code, b)
		}
	}
	r := f.run("hold", "--pending", "--json")
	var q struct {
		Mode string
		Held []struct{ ID, Status, Title string }
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &q) != nil || q.Mode != "on" || len(q.Held) != 2 {
		t.Fatalf("hold --json: %+v", r)
	}
	if r := f.run("hold"); r.code != 0 || !strings.Contains(r.stdout, "web-1 down") || !strings.Contains(r.stdout, "mode on") {
		t.Fatalf("hold: %+v", r)
	}
	// A look is not a poll: the dead-man clock stays where it was.
	_, b := f.req(http.MethodGet, "/v1/hold?peek=1", readTok, "")
	if !strings.Contains(b, `"last_poll_ts":null`) {
		t.Fatalf("the CLI polled: %s", b)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("hold", "suppress", "--reason", "BLIP", q.Held[0].ID); r.code != 0 || !strings.Contains(r.stdout, "suppressed 1") {
		t.Fatalf("suppress: %+v", r)
	}
	if r := f.run("hold", "release"); r.code != 2 {
		t.Fatalf("release without flags: %+v", r)
	}
	r = f.run("hold", "release", "--verdict", "real", "--title", "web down", "--body", "both", "--tag", "general",
		q.Held[0].ID, q.Held[1].ID)
	if r.code != 0 || !strings.Contains(r.stdout, "released 1, sent ") || !strings.Contains(r.stderr, q.Held[0].ID) {
		t.Fatalf("release: %+v", r)
	}
	if r := f.run("hold", "--pending", "--json"); !strings.Contains(r.stdout, `"held":[]`) {
		t.Fatalf("after close: %+v", r)
	}
}

func TestDevices(t *testing.T) {
	f := newFixture(t)
	if r := f.run("devices"); r.code != 0 || !strings.Contains(r.stderr, "no devices") {
		t.Fatalf("empty: %+v", r)
	}
	code, b := f.req(http.MethodPost, "/v1/devices", readTok,
		`{"token":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","environment":"production","topic":"org.example.heraldo","name":"Test Phone","platform":"ios"}`)
	var d struct{ ID string }
	if code != http.StatusCreated && code != http.StatusOK || json.Unmarshal([]byte(b), &d) != nil {
		t.Fatalf("register: %d %s", code, b)
	}
	if r := f.run("devices", "ls"); r.code != 0 || !strings.Contains(r.stdout, "Test Phone") || !strings.Contains(r.stdout, "…89abcdef") ||
		!strings.Contains(r.stdout, "active") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("devices", "retire", d.ID); r.code != 0 || !strings.Contains(r.stdout, "retired "+d.ID) {
		t.Fatalf("retire: %+v", r)
	}
	if r := f.run("devices"); !strings.Contains(r.stdout, "retired removed") {
		t.Fatalf("after retire: %+v", r)
	}
	if r := f.run("devices", "retire", "dev_nope"); r.code != 1 {
		t.Fatalf("unknown: %+v", r)
	}
}

func TestProjectsStateActivities(t *testing.T) {
	f := newFixture(t)
	f.post(writeTok, `{"source":"s","kind":"incident","key":"web:down","action":"open","title":"web down","project":"web"}`)
	f.post(writeTok, `{"source":"s","kind":"message","title":"plain"}`)
	f.post(writeTok, `{"source":"ci","kind":"progress","key":"build/7","action":"start","content":{"title":"Build 7","status":"compiling","progress":0.5}}`)

	r := f.run("projects")
	if r.code != 0 || !regexpMatch(`(?m)^web\s+1\s`, r.stdout) || !regexpMatch(`(?m)^-\s+2$`, r.stdout) {
		t.Fatalf("projects: %+v", r)
	}
	r = f.run("state")
	if r.code != 0 || !strings.Contains(r.stdout, "incidents (1)") || !strings.Contains(r.stdout, "web down") ||
		!strings.Contains(r.stdout, "progress (1)") {
		t.Fatalf("state: %+v", r)
	}
	if r := f.run("state", "--project", "-"); !strings.Contains(r.stdout, "incidents (0)") {
		t.Fatalf("state --project -: %+v", r)
	}
	r = f.run("activities")
	if r.code != 0 || !strings.Contains(r.stdout, "build/7") || !strings.Contains(r.stdout, "50%") ||
		!strings.Contains(r.stdout, "Build 7: compiling") {
		t.Fatalf("activities: %+v", r)
	}
	r = f.run("activities", "show", "build/7")
	if r.code != 0 || !strings.Contains(r.stdout, "state:    active") || !strings.Contains(r.stdout, "start") {
		t.Fatalf("activities show: %+v", r)
	}
	if r := f.run("activities", "show", "build/none"); r.code != 1 {
		t.Fatalf("unknown activity: %+v", r)
	}
}
