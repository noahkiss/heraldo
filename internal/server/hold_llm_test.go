package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/hold"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

// newHoldLLM is a hold env in mode on whose LLM is f, with settings from
// vars and an optional rules file. The dead-man is far off, so only the
// deadline forwards.
func newHoldLLM(t *testing.T, f *llm.Fake, vars map[string]string, rulesYAML string) *holdEnv {
	t.Helper()
	dir := t.TempDir()
	e := &holdEnv{t: t, path: filepath.Join(dir, "test.db"), clock: &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}}
	st, err := store.Open(context.Background(), e.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClock(e.clock.now)
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	e.log = &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(e.log, nil))
	m, err := settings.New(context.Background(), settings.Options{Backend: st, Now: e.clock.now, Logger: log,
		Getenv: func(k string) string { return vars[k] }})
	if err != nil {
		t.Fatal(err)
	}
	var rw *rules.Watcher
	if rulesYAML != "" {
		p := filepath.Join(dir, "rules.yaml")
		if err := os.WriteFile(p, []byte(rulesYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		if rw, err = rules.NewWatcher(p, log); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf("sender write %s\nphone read %s\n",
		auth.Hash(writeTok), auth.Hash(readTok))), '\n')
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	e.srv = NewServer(Options{
		Version: "v-test", Store: st, Tokens: tokens, Validator: v, Now: e.clock.now, Logger: log, Settings: m, Rules: rw,
		TrustedNets: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
		LLMProvider: f.Factory,
		Hold:        HoldOptions{Mode: hold.On, Bypass: &hold.Bypass{}, Deadman: 10 * time.Hour},
	})
	return e
}

func decisionsOn(mode string, extra map[string]string) map[string]string {
	v := map[string]string{"HERALDO_LLM_MODE": mode, "HERALDO_LLM_BASE_URL": "https://llm.example.net/v1",
		"HERALDO_LLM_DECISIONS_MODEL": "dm"}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

// groupReply is a fake answer with one group.
func groupReply(ids, verdict, level string, conf float64) llm.Response {
	q := `"` + strings.Join(strings.Split(ids, ","), `","`) + `"`
	return llm.Response{Content: fmt.Sprintf(`{"groups":[{"ids":[%s],"title":"Backups failing on nas",`+
		`"summary":"Three backup jobs failed on nas.\nNo recovery yet.","verdict":%q,"suggested_level":%q,"confidence":%v}]}`,
		q, verdict, level, conf), TokensIn: 10, TokensOut: 10}
}

// post sends n held alerts and moves past their deadline.
func (e *holdEnv) heldBurst(titles ...string) {
	e.t.Helper()
	for _, title := range titles {
		sentOK(e.t, e.send(`{"title":"`+title+`","body":"exit 1","tag":"alert"}`))
		e.clock.add(time.Second)
	}
	if n := len(e.rows("?state=pending")); n != len(titles) {
		e.t.Fatalf("held %d, want %d", n, len(titles))
	}
	e.clock.add(31 * time.Minute)
}

// llmRows returns the llm rows of an event's trace.
func (e *holdEnv) llmRows(id string) []store.DecisionRow {
	e.t.Helper()
	rows, err := e.st.Decisions(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.DecisionRow
	for _, r := range rows {
		if r.Stage == store.LLMStage {
			out = append(out, r)
		}
	}
	return out
}

// heldEvents and newer split the stored events into the held ones and the
// rest.
func (e *holdEnv) split() (held, rest []map[string]any) {
	for _, ev := range e.events() {
		if h := holdOf(ev); h != nil && h["state"] == "held" {
			held = append(held, ev)
		} else {
			rest = append(rest, ev)
		}
	}
	return held, rest
}

var burst = []string{"backup nas failed", "backup vm failed", "backup pi failed"}

func TestHoldLLMEnforceGroups(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("enforce", nil), "")
	e.heldBurst(burst...)
	e.tick()

	held, rest := e.split()
	if len(rest) != 1 {
		t.Fatalf("want one release event, got %d: %v", len(rest), rest)
	}
	rel := rest[0]
	if rel["title"] != "Backups failing on nas" || rel["level"] != "time-sensitive" || rel["source"] != holdSource ||
		rel["summary"] != "Three backup jobs failed on nas." || !strings.Contains(rel["body"].(string), "- backup vm failed") {
		t.Fatalf("release %v", rel)
	}
	if h := holdOf(rel); h["reason"] != "llm" || h["verdict"] != "REAL" || len(h["ids"].([]any)) != 3 {
		t.Fatalf("release hold %v", h)
	}
	for _, r := range e.rows("") {
		if r["status"] != store.HoldReleased || r["verdict"] != "REAL" || r["reason"] != "llm" {
			t.Fatalf("entry %v", r)
		}
	}
	for _, ev := range append(held, rel) {
		rows := e.llmRows(ev["id"].(string))
		if len(rows) != 1 || rows[0].Outcome != "merge" ||
			!strings.Contains(rows[0].Reason, "grouped 3 alerts: REAL → released as 1 time-sensitive push") {
			t.Fatalf("trace of %v: %+v", ev["title"], rows)
		}
		var det map[string]any
		_ = json.Unmarshal(rows[0].Detail, &det)
		if det["call_id"] == nil || det["applied"] != true || det["verdict"] != "REAL" {
			t.Fatalf("detail %v", det)
		}
	}
	// The why summary carries the LLM's line.
	rec, _ := e.st.Get(context.Background(), held[0]["id"].(string))
	recs := []store.Record{rec}
	if err := e.st.WithWhy(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recs[0].Event), "LLM (dm, ") {
		t.Fatalf("why %s", recs[0].Event)
	}
	// One call, temperature 0, with a schema; the model sees refs, not
	// hold ids.
	reqs := f.Requests()
	if len(reqs) != 1 || reqs[0].Temperature == nil || *reqs[0].Temperature != 0 || reqs[0].Schema == nil {
		t.Fatalf("requests %+v", reqs)
	}
	if user := reqs[0].Messages[1].Content; !strings.Contains(user, `"id":"a1"`) || strings.Contains(user, "h-2026") ||
		strings.Contains(user, "exit 1") {
		t.Fatalf("user message %s", user)
	}
	// A second tick does nothing more.
	e.tick()
	if _, rest := e.split(); len(rest) != 1 {
		t.Fatalf("second tick stored %d", len(rest))
	}
}

func TestHoldLLMFloors(t *testing.T) {
	quietRules := `rules:
  - name: blips
    match: {title: '^blip '}
    set: {kind: message, level: active}
`
	cases := []struct {
		name, rules, verdict, level string
		suppress                    bool
		titles                      []string
		wantLevel, wantReason       string
	}{
		{"time-sensitive is never lowered", "", "BLIP", "passive", true, burst, "time-sensitive",
			"LLM suggested passive; refused: time-sensitive floor"},
		{"blip becomes one passive summary", quietRules, "BLIP", "passive", true,
			[]string{"blip a", "blip b"}, "passive", "BLIP → released as 1 passive summary"},
		{"suppression needs the setting", quietRules, "BLIP", "passive", false,
			[]string{"blip a", "blip b"}, "active", "refused: suppression needs llm.hold_may_suppress"},
		{"only blip or false suppress", quietRules, "MAINTENANCE", "passive", true,
			[]string{"blip a", "blip b"}, "active", "refused: only a BLIP or FALSE verdict may suppress"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids := "a1,a2,a3"[:len(c.titles)*3-1]
			f := &llm.Fake{Replies: []llm.Response{groupReply(ids, c.verdict, c.level, 0.95)}}
			extra := map[string]string{"HERALDO_LLM_HOLD_MAY_SUPPRESS": fmt.Sprint(c.suppress)}
			e := newHoldLLM(t, f, decisionsOn("enforce", extra), c.rules)
			e.heldBurst(c.titles...)
			e.tick()
			held, rest := e.split()
			if len(rest) != 1 || rest[0]["level"] != c.wantLevel {
				t.Fatalf("events %v", rest)
			}
			rows := e.llmRows(held[0]["id"].(string))
			if len(rows) != 1 || !strings.Contains(rows[0].Reason, c.wantReason) {
				t.Fatalf("trace %+v", rows)
			}
		})
	}
}

func TestHoldLLMModes(t *testing.T) {
	cases := []struct {
		mode    string
		conf    float64
		summary string
		reason  string
	}{
		{"shadow", 0.9, "", "would be released as 1 time-sensitive push; nothing changed (shadow)"},
		{"advise", 0.9, "Three backup jobs failed on nas.", "added its summary; level and grouping unchanged (advise)"},
		{"enforce", 0.4, "Three backup jobs failed on nas.", "at confidence 0.40 (below 0.6): text only"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "time-sensitive", c.conf)}}
			e := newHoldLLM(t, f, decisionsOn(c.mode, nil), "")
			e.heldBurst(burst...)
			e.tick()
			_, rest := e.split()
			if len(rest) != 3 {
				t.Fatalf("want the 3 usual forwards, got %d", len(rest))
			}
			for _, ev := range rest {
				if !strings.HasPrefix(ev["title"].(string), "[not triaged in 30 min] ") || ev["level"] != "time-sensitive" {
					t.Fatalf("forward %v", ev)
				}
				if got, _ := ev["summary"].(string); got != c.summary {
					t.Fatalf("summary %q, want %q", got, c.summary)
				}
				rows := e.llmRows(ev["id"].(string))
				if len(rows) != 1 || !strings.Contains(rows[0].Reason, c.reason) {
					t.Fatalf("trace %+v", rows)
				}
			}
			for _, r := range e.rows("") {
				if r["status"] != store.HoldExpired {
					t.Fatalf("entry %v", r)
				}
			}
		})
	}
}

func TestHoldLLMFallback(t *testing.T) {
	cases := []struct {
		name   string
		fake   *llm.Fake
		extra  map[string]string
		reason string
	}{
		{"error", &llm.Fake{Err: errors.New("boom")}, nil, "llm: error (boom); forwarded as usual"},
		{"unknown id", &llm.Fake{Replies: []llm.Response{groupReply("a1,a9", "REAL", "active", 0.9)}}, nil,
			`llm: invalid output (group 1: id "a9" is not in the input)`},
		{"id twice", &llm.Fake{Replies: []llm.Response{{Content: `{"groups":[` +
			`{"ids":["a1"],"title":"t","summary":"s","verdict":"REAL","suggested_level":"active","confidence":1},` +
			`{"ids":["a1"],"title":"t","summary":"s","verdict":"REAL","suggested_level":"active","confidence":1}]}`}}}, nil,
			"llm: invalid output (id \"a1\" is in two groups)"},
		{"raise", &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "critical", 0.9)}}, nil,
			"louder than its members"},
		{"bad verdict", &llm.Fake{Replies: []llm.Response{groupReply("a1", "MAYBE", "active", 0.9)}}, nil, `verdict "MAYBE"`},
		{"not json", &llm.Fake{Replies: []llm.Response{{Content: "sure, here you go"}}}, nil, "llm: invalid output"},
		{"budget", &llm.Fake{Replies: []llm.Response{groupReply("a1", "REAL", "active", 0.9)}},
			map[string]string{"HERALDO_LLM_CALLS_PER_MINUTE": "0"}, "llm: budget used up"},
		{"not configured", &llm.Fake{}, map[string]string{"HERALDO_LLM_BASE_URL": " "}, "llm: not configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := decisionsOn("enforce", c.extra)
			if c.extra["HERALDO_LLM_BASE_URL"] == " " {
				delete(vars, "HERALDO_LLM_BASE_URL")
			}
			e := newHoldLLM(t, c.fake, vars, "")
			e.heldBurst(burst...)
			e.tick()
			_, rest := e.split()
			var forwards []map[string]any
			for _, ev := range rest {
				if ev["source"] != "heraldo" {
					forwards = append(forwards, ev)
				}
			}
			if len(forwards) != 3 {
				t.Fatalf("want the 3 usual forwards, got %v", rest)
			}
			for _, ev := range forwards {
				if !strings.HasPrefix(ev["title"].(string), "[not triaged in 30 min] ") || ev["summary"] != nil {
					t.Fatalf("forward %v", ev)
				}
				rows := e.llmRows(ev["id"].(string))
				if len(rows) != 1 || !strings.Contains(rows[0].Reason, c.reason) {
					t.Fatalf("trace %+v", rows)
				}
			}
		})
	}
}

// slowProvider answers after wait, or fails when ctx ends first, as a
// real endpoint does.
type slowProvider struct{ wait time.Duration }

func (p slowProvider) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	select {
	case <-time.After(p.wait):
		return groupReply("a1,a2,a3", "REAL", "time-sensitive", 0.9), nil
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}

func TestHoldLLMTimeout(t *testing.T) {
	e := newHoldLLM(t, &llm.Fake{}, decisionsOn("enforce", map[string]string{"HERALDO_LLM_DECISIONS_TIMEOUT": "50ms"}), "")
	old := e.srv.s.Options
	old.LLMProvider = func(string, string, string) llm.Provider { return slowProvider{wait: 5 * time.Second} }
	e.srv = NewServer(old)
	e.heldBurst(burst...)
	start := time.Now()
	e.tick()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the tick waited %s", d)
	}
	_, rest := e.split()
	if len(rest) != 3 {
		t.Fatalf("want the 3 usual forwards, got %d", len(rest))
	}
	for _, ev := range rest {
		rows := e.llmRows(ev["id"].(string))
		if len(rows) != 1 || rows[0].Reason != "llm: timeout; forwarded as usual" {
			t.Fatalf("trace %+v", rows)
		}
	}
}

func TestHoldLLMFloorsKeepEventsOut(t *testing.T) {
	r := `rules:
  - name: host-down
    match: {title: '^(?P<host>\w+) is down$'}
    set: {kind: incident, action: open, key: 'host:{host}'}
  - name: job-failed
    match: {title: '^job (?P<job>\w+) failed$'}
    set: {kind: checkin, action: fail, key: 'job:{job}'}
`
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1", "BLIP", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("enforce", map[string]string{"HERALDO_LLM_HOLD_MAY_SUPPRESS": "true"}), r)
	e.heldBurst("web is down", "job sync failed", "backup nas failed")
	e.tick()
	user := f.Requests()[0].Messages[1].Content
	if strings.Contains(user, "web is down") || strings.Contains(user, "job sync") || !strings.Contains(user, "backup nas") {
		t.Fatalf("the model saw %s", user)
	}
	held, _ := e.split()
	want := map[string]string{"web is down": "incident at time-sensitive floor", "job sync failed": "check-in fail floor"}
	for _, ev := range held {
		w, ok := want[ev["title"].(string)]
		if !ok {
			continue
		}
		rows := e.llmRows(ev["id"].(string))
		if len(rows) != 1 || rows[0].Reason != "llm: not consulted: "+w+"; forwarded as usual" {
			t.Fatalf("%v: %+v", ev["title"], rows)
		}
	}
}

func TestHoldLLMDecidesAtDeadlineOnly(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("enforce", nil), "")
	sentOK(t, e.send(`{"title":"backup nas failed","body":"b","tag":"alert"}`))
	e.clock.add(5 * time.Minute)
	e.tick()
	id := e.rows("?state=pending")[0]["id"].(string)
	rec := e.req("POST", "/held/release", writeTok, "", "application/json",
		[]byte(`{"ids":["`+id+`"],"verdict":"BLIP","title":"t","body":"b","tag":"general"}`))
	if rec.Code != 200 {
		t.Fatalf("release %d %s", rec.Code, rec.Body)
	}
	e.clock.add(40 * time.Minute)
	e.tick()
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("the LLM was asked %d times; triage had released before the deadline", n)
	}
}

func TestHoldLLMOffAsks(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("off", nil), "")
	e.heldBurst(burst...)
	e.tick()
	if len(f.Requests()) != 0 {
		t.Fatal("mode off asked the LLM")
	}
	held, rest := e.split()
	if len(rest) != 3 || len(e.llmRows(held[0]["id"].(string))) != 0 {
		t.Fatalf("mode off: %d events", len(rest))
	}
}

func TestHoldLLMGuidance(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("shadow", nil), "")
	g := store.Guidance{ID: "gd_test1", Scope: "global", Text: "Group backups by host.", Enabled: true, CreatedBy: "t",
		CreatedAt: e.clock.now(), UpdatedAt: e.clock.now()}
	if err := e.st.AddGuidance(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.heldBurst(burst...)
	e.tick()
	if sys := f.Requests()[0].Messages[0].Content; !strings.Contains(sys, "[global] Group backups by host.") {
		t.Fatalf("system %s", sys)
	}
	held, _ := e.split()
	var det map[string]any
	_ = json.Unmarshal(e.llmRows(held[0]["id"].(string))[0].Detail, &det)
	if gs, _ := det["guidance"].([]any); len(gs) != 1 || gs[0] != g.ID+":1" {
		t.Fatalf("guidance %v", det)
	}
}
