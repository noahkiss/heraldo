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
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/decide"
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

// tickSettle runs the hold's tick and waits for its LLM call.
func (e *holdEnv) tickSettle() {
	e.tick()
	e.srv.WaitHold()
}

// staggered sends first, then later ten minutes on, and moves just past
// first's deadline: first is due, the others are still held.
func (e *holdEnv) staggered(first string, later ...string) {
	e.t.Helper()
	sentOK(e.t, e.send(`{"title":"`+first+`","body":"exit 1","tag":"alert"}`))
	e.clock.add(10 * time.Minute)
	for _, title := range later {
		sentOK(e.t, e.send(`{"title":"`+title+`","body":"exit 1","tag":"alert"}`))
	}
	e.clock.add(21 * time.Minute)
}

// fakePusher takes every event and records each replace.
type fakePusher struct {
	mu       sync.Mutex
	replaces [][3]string
}

func (f *fakePusher) Enqueue(store.Record) bool { return true }

func (f *fakePusher) Replace(_ context.Context, id, title, summary string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaces = append(f.replaces, [3]string{id, title, summary})
	return 1, nil
}

func (f *fakePusher) calls() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][3]string{}, f.replaces...)
}

func (e *holdEnv) withPusher(p Pusher) {
	old := e.srv.s.Options
	old.Pusher = p
	e.srv = NewServer(old)
}

// byTitle finds the one stored event whose title has prefix.
func byTitle(t *testing.T, evs []map[string]any, title string) map[string]any {
	t.Helper()
	var out map[string]any
	for _, ev := range evs {
		if s, _ := ev["title"].(string); strings.Contains(s, title) {
			if out != nil {
				t.Fatalf("two events titled %q", title)
			}
			out = ev
		}
	}
	if out == nil {
		t.Fatalf("no event titled %q in %v", title, evs)
	}
	return out
}

func (e *holdEnv) reasonOf(id string) string {
	e.t.Helper()
	rows := e.llmRows(id)
	if len(rows) != 1 {
		e.t.Fatalf("want one llm row on %s, got %+v", id, rows)
	}
	return rows[0].Reason
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

func TestHoldLLMNeverDelaysTheForward(t *testing.T) {
	e := newHoldLLM(t, &llm.Fake{}, decisionsOn("enforce", map[string]string{"HERALDO_LLM_DECISIONS_TIMEOUT": "300ms"}), "")
	old := e.srv.s.Options
	old.LLMProvider = func(string, string, string) llm.Provider { return slowProvider{wait: 5 * time.Second} }
	e.srv = NewServer(old)
	e.heldBurst(burst...)
	start := time.Now()
	e.tick()
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("the tick waited %s for the LLM", d)
	}
	// The forwards are stored before the LLM answers.
	_, rest := e.split()
	if len(rest) != 3 {
		t.Fatalf("want the 3 forwards at once, got %d", len(rest))
	}
	for _, ev := range rest {
		if !strings.HasPrefix(ev["title"].(string), "[not triaged in 30 min] ") || ev["level"] != "time-sensitive" {
			t.Fatalf("forward %v", ev)
		}
	}
	e.srv.WaitHold()
	for _, ev := range rest {
		if r := e.reasonOf(ev["id"].(string)); r != "llm: timeout; forwarded as usual" {
			t.Fatalf("trace %q", r)
		}
	}
}

func TestHoldLLMEnforceReplacesAndFolds(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("enforce", nil), "")
	fp := &fakePusher{}
	e.withPusher(fp)
	e.staggered("backup nas failed", "backup vm failed", "backup pi failed")
	e.tickSettle()

	held, rest := e.split()
	if len(rest) != 1 {
		t.Fatalf("want the one due forward, got %v", rest)
	}
	first := rest[0]
	nasHold := holdOf(first)["ids"].([]any)[0].(string)
	if holdOf(first)["collapse"] != "hold:"+nasHold {
		t.Fatalf("forward hold %v", holdOf(first))
	}
	// The forwarded notification is replaced with the group's text.
	calls := fp.calls()
	if len(calls) != 1 || calls[0] != [3]string{first["id"].(string), "Backups failing on nas", "Three backup jobs failed on nas. (3 alerts)"} {
		t.Fatalf("replaces %v", calls)
	}
	if r := e.reasonOf(first["id"].(string)); !strings.Contains(r, "grouped 3 alerts: REAL → replaced its notification with the group's") {
		t.Fatalf("first trace %q", r)
	}
	vm := byTitle(t, held, "backup vm failed")
	if r := e.reasonOf(vm["id"].(string)); !strings.Contains(r, "→ folds into the group's notification at its deadline") {
		t.Fatalf("held trace %q", r)
	}
	// The model saw the held members, without their bodies.
	if user := f.Requests()[0].Messages[1].Content; !strings.Contains(user, "backup pi failed") || strings.Contains(user, "exit 1") {
		t.Fatalf("user message %s", user)
	}

	// At their own deadline the later members fold into that notification.
	e.clock.add(10 * time.Minute)
	e.tickSettle()
	_, rest = e.split()
	if len(rest) != 3 {
		t.Fatalf("events %d", len(rest))
	}
	for _, ev := range rest[1:] {
		h := holdOf(ev)
		if ev["title"] != "Backups failing on nas" || ev["summary"] != "Three backup jobs failed on nas. (3 alerts)" ||
			ev["level"] != "time-sensitive" || h["collapse"] != "hold:"+nasHold || h["fold"] != true {
			t.Fatalf("fold %v", ev)
		}
		if r := e.reasonOf(ev["id"].(string)); !strings.Contains(r, "→ folded into the group's notification") {
			t.Fatalf("fold trace %q", r)
		}
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("calls %d: the planned members asked again", n)
	}
}

func TestHoldLLMSuppression(t *testing.T) {
	quietRules := `rules:
  - name: blips
    match: {title: '^blip '}
    set: {kind: message, level: active}
`
	cases := []struct {
		name, rules, verdict string
		suppress             bool
		titles               []string
		later                string // the later members' outcome
		reason               string
	}{
		{"blip becomes one passive summary", quietRules, "BLIP", true, []string{"blip a", "blip b", "blip c"}, "summary",
			"BLIP → released as 1 passive summary"},
		{"suppression needs the setting", quietRules, "BLIP", false, []string{"blip a", "blip b", "blip c"}, "fold",
			"refused: suppression needs llm.hold_may_suppress"},
		{"only blip or false suppress", quietRules, "MAINTENANCE", true, []string{"blip a", "blip b", "blip c"}, "fold",
			"refused: only a BLIP or FALSE verdict may suppress"},
		{"time-sensitive is never lowered", "", "BLIP", true, burst, "fold", "refused: time-sensitive floor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", c.verdict, "passive", 0.95)}}
			extra := map[string]string{"HERALDO_LLM_HOLD_MAY_SUPPRESS": fmt.Sprint(c.suppress)}
			e := newHoldLLM(t, f, decisionsOn("enforce", extra), c.rules)
			e.staggered(c.titles[0], c.titles[1:]...)
			e.tickSettle()
			_, rest := e.split()
			first := rest[0]
			r := e.reasonOf(first["id"].(string))
			if c.later == "summary" && !strings.Contains(r, "already pushed: suppression recorded, not applied") {
				t.Fatalf("pushed member %q", r)
			}
			e.clock.add(10 * time.Minute)
			e.tickSettle()
			_, rest = e.split()
			switch c.later {
			case "summary":
				if len(rest) != 2 {
					t.Fatalf("want the first forward and one summary, got %v", rest)
				}
				sum := rest[1]
				if sum["level"] != "passive" || sum["source"] != holdSource || !strings.Contains(sum["body"].(string), "- blip c") {
					t.Fatalf("summary %v", sum)
				}
				if r := e.reasonOf(sum["id"].(string)); !strings.Contains(r, c.reason) {
					t.Fatalf("summary trace %q", r)
				}
				for _, row := range e.rows("") {
					if row["title"] != c.titles[0] && (row["status"] != store.HoldReleased || row["verdict"] != "BLIP" || row["reason"] != "llm") {
						t.Fatalf("entry %v", row)
					}
				}
			case "fold":
				if len(rest) != 3 {
					t.Fatalf("want 3 forwards, got %v", rest)
				}
				for _, ev := range rest[1:] {
					if ev["level"] != first["level"] || holdOf(ev)["fold"] != true {
						t.Fatalf("fold %v", ev)
					}
					if r := e.reasonOf(ev["id"].(string)); !strings.Contains(r, c.reason) {
						t.Fatalf("fold trace %q", r)
					}
				}
			}
		})
	}
}

func TestHoldLLMModes(t *testing.T) {
	cases := []struct {
		mode     string
		conf     float64
		replaces int
		reason   string
	}{
		{"shadow", 0.9, 0, "would fold into 1 notification; nothing changed (shadow)"},
		{"advise", 0.9, 3, "; added its summary to the notification"},
		{"enforce", 0.4, 3, "at confidence 0.40 (below 0.6): text only; added its summary to the notification"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2,a3", "REAL", "time-sensitive", c.conf)}}
			e := newHoldLLM(t, f, decisionsOn(c.mode, nil), "")
			fp := &fakePusher{}
			e.withPusher(fp)
			e.heldBurst(burst...)
			e.tickSettle()
			_, rest := e.split()
			if len(rest) != 3 {
				t.Fatalf("want the 3 usual forwards, got %d", len(rest))
			}
			for _, ev := range rest {
				if !strings.HasPrefix(ev["title"].(string), "[not triaged in 30 min] ") || ev["summary"] != nil {
					t.Fatalf("forward %v", ev)
				}
				if r := e.reasonOf(ev["id"].(string)); !strings.Contains(r, c.reason) {
					t.Fatalf("trace %q", r)
				}
			}
			calls := fp.calls()
			if len(calls) != c.replaces {
				t.Fatalf("replaces %v", calls)
			}
			for _, call := range calls {
				if call[1] != "" || call[2] != "Three backup jobs failed on nas." {
					t.Fatalf("replace %v: advise changes only the summary", call)
				}
			}
		})
	}
}

func TestHoldLLMAdviseLaterForward(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1,a2", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("advise", nil), "")
	e.staggered("backup nas failed", "backup vm failed")
	e.tickSettle()
	_, rest := e.split()
	// No pusher: the replace is recorded as not sent.
	if r := e.reasonOf(rest[0]["id"].(string)); !strings.HasSuffix(r, "added its summary to the notification; not sent: push is off") {
		t.Fatalf("first trace %q", r)
	}
	e.clock.add(10 * time.Minute)
	e.tickSettle()
	_, rest = e.split()
	later := rest[1]
	if later["title"] != "[not triaged in 30 min] backup vm failed" || later["summary"] != "Three backup jobs failed on nas." ||
		holdOf(later)["fold"] != nil {
		t.Fatalf("advised forward %v", later)
	}
}

func TestHoldLLMTriageClosesFirst(t *testing.T) {
	gate := make(chan struct{})
	f := &llm.Fake{Reply: func(llm.Request) (llm.Response, error) {
		<-gate
		return groupReply("a1,a2", "REAL", "time-sensitive", 0.9), nil
	}}
	e := newHoldLLM(t, f, decisionsOn("enforce", nil), "")
	e.staggered("backup nas failed", "backup vm failed")
	e.tick()
	id := e.rows("?state=pending")[0]["id"].(string)
	rec := e.req("POST", "/held/release", writeTok, "", "application/json",
		[]byte(`{"ids":["`+id+`"],"verdict":"REAL","title":"vm","body":"b","tag":"general"}`))
	if rec.Code != 200 {
		t.Fatalf("release %d %s", rec.Code, rec.Body)
	}
	close(gate)
	e.srv.WaitHold()
	held, _ := e.split()
	vm := byTitle(t, held, "backup vm failed")
	if r := e.reasonOf(vm["id"].(string)); !strings.HasSuffix(r, "; triage closed it first") {
		t.Fatalf("trace %q", r)
	}
	e.clock.add(10 * time.Minute)
	e.tickSettle()
	if _, rest := e.split(); len(rest) != 2 { // the first forward and triage's release
		t.Fatalf("events %v", rest)
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
			e.tickSettle()
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
				if r := e.reasonOf(ev["id"].(string)); !strings.Contains(r, c.reason) {
					t.Fatalf("trace %q", r)
				}
			}
		})
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
	e.tickSettle()
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
		if r := e.reasonOf(ev["id"].(string)); r != "llm: not consulted: "+w+"; forwarded as usual" {
			t.Fatalf("%v: %q", ev["title"], r)
		}
	}
}

func TestHoldLLMDecidesAtDeadlineOnly(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("enforce", nil), "")
	sentOK(t, e.send(`{"title":"backup nas failed","body":"b","tag":"alert"}`))
	e.clock.add(5 * time.Minute)
	e.tickSettle()
	id := e.rows("?state=pending")[0]["id"].(string)
	rec := e.req("POST", "/held/release", writeTok, "", "application/json",
		[]byte(`{"ids":["`+id+`"],"verdict":"BLIP","title":"t","body":"b","tag":"general"}`))
	if rec.Code != 200 {
		t.Fatalf("release %d %s", rec.Code, rec.Body)
	}
	e.clock.add(40 * time.Minute)
	e.tickSettle()
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("the LLM was asked %d times; triage had released before the deadline", n)
	}
}

func TestHoldLLMOffAsks(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{groupReply("a1", "REAL", "time-sensitive", 0.9)}}
	e := newHoldLLM(t, f, decisionsOn("off", nil), "")
	e.heldBurst(burst...)
	e.tickSettle()
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
	e.tickSettle()
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

func TestLLMTraceOncePerEventAndCall(t *testing.T) {
	e := newHoldLLM(t, &llm.Fake{}, decisionsOn("enforce", nil), "")
	sentOK(t, e.send(`{"title":"x","body":"b","tag":"general"}`))
	id := e.events()[0]["id"].(string)
	tr := decide.Trace{Reason: "LLM (dm, 1.0 s) grouped 1 alert: REAL", Detail: map[string]any{"call_id": "llm_1"}}
	e.srv.s.appendTrace(context.Background(), id, tr)
	e.srv.s.appendTrace(context.Background(), id, tr)
	if n := len(e.llmRows(id)); n != 1 {
		t.Fatalf("rows %d after a retry", n)
	}
	tr.Detail = map[string]any{"call_id": "llm_2"}
	e.srv.s.appendTrace(context.Background(), id, tr)
	if n := len(e.llmRows(id)); n != 2 {
		t.Fatalf("rows %d after a second call", n)
	}
}
