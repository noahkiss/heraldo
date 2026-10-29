package decide

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/settings"
)

func TestHoldFloors(t *testing.T) {
	P, A, TS, C := api.LevelPassive, api.LevelActive, api.LevelTimeSensitive, api.LevelCritical
	cases := []struct {
		max, sug    api.Level
		verdict     string
		suppress    bool
		want        api.Level
		wantRefused string
	}{
		{A, TS, "REAL", false, A, "never raises a level"},
		{A, A, "REAL", false, A, ""},
		{TS, A, "MAINTENANCE", true, TS, "time-sensitive floor"},
		{TS, P, "BLIP", true, TS, "time-sensitive floor"},
		{C, A, "FALSE", true, C, "critical floor"},
		{A, P, "BLIP", false, A, "suppression needs llm.hold_may_suppress"},
		{A, P, "ROUTINE", true, A, "only a BLIP or FALSE verdict may suppress"},
		{A, P, "BLIP", true, P, ""},
		{A, A, "FALSE", true, P, ""},
		{P, P, "REAL", false, P, ""},
	}
	for _, c := range cases {
		got, refused := HoldFloors(c.max, c.sug, c.verdict, c.suppress)
		if got != c.want || refused != c.wantRefused {
			t.Errorf("HoldFloors(%s, %s, %s, %v) = %s %q, want %s %q", c.max, c.sug, c.verdict, c.suppress, got, refused,
				c.want, c.wantRefused)
		}
	}
}

func TestProtected(t *testing.T) {
	cases := []struct {
		m    Member
		want string
	}{
		{Member{Source: "heraldo", Level: api.LevelPassive}, "heraldo's own event"},
		{Member{Source: "heraldo-hold", Level: api.LevelActive}, "heraldo's own event"},
		{Member{Source: "x", Level: api.LevelCritical}, "critical floor"},
		{Member{Source: "x", Kind: api.KindIncident, Action: "open", Level: api.LevelTimeSensitive}, "incident at time-sensitive floor"},
		{Member{Source: "x", Kind: api.KindIncident, Action: "update", Level: api.LevelTimeSensitive}, "incident at time-sensitive floor"},
		{Member{Source: "x", Kind: api.KindIncident, Action: "open", Level: api.LevelActive}, ""},
		{Member{Source: "x", Kind: api.KindCheckin, Action: "fail", Failed: true, Level: api.LevelActive}, "check-in fail floor"},
		{Member{Source: "x", Kind: api.KindMessage, Level: api.LevelTimeSensitive}, ""},
	}
	for _, c := range cases {
		if got := Protected(c.m); got != c.want {
			t.Errorf("Protected(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
	// A check-in run with a non-zero exit code is a fail.
	code := 3
	ev := api.Event{Id: "e", Source: "cron", Kind: api.KindCheckin, Checkin: &api.Checkin{ExitCode: &code}}
	if m := MemberOf(&ev); !m.Failed || Protected(m) != "check-in fail floor" {
		t.Fatalf("exit code: %+v", m)
	}
}

func TestValidateHold(t *testing.T) {
	ms := []Member{{Ref: "a1", Level: api.LevelActive}, {Ref: "a2", Level: api.LevelTimeSensitive}}
	ok := HoldGroup{IDs: []string{"a1", "a2"}, Title: "t", Summary: "one\ntwo", Verdict: "REAL", SuggestedLevel: "time-sensitive", Confidence: 0.7}
	if err := ValidateHold(HoldOutput{Groups: []HoldGroup{ok}}, ms); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(g *HoldGroup){
		"not in the input":     func(g *HoldGroup) { g.IDs = []string{"a3"} },
		"has no ids":           func(g *HoldGroup) { g.IDs = nil },
		"verdict":              func(g *HoldGroup) { g.Verdict = "real" },
		"suggested_level":      func(g *HoldGroup) { g.SuggestedLevel = "loud" },
		"louder than":          func(g *HoldGroup) { g.SuggestedLevel = "critical" },
		"confidence":           func(g *HoldGroup) { g.Confidence = 1.5 },
		"title":                func(g *HoldGroup) { g.Title = " " },
		"at most two lines":    func(g *HoldGroup) { g.Summary = "a\nb\nc" },
		"louder than its memb": func(g *HoldGroup) { g.IDs, g.SuggestedLevel = []string{"a1"}, "time-sensitive" },
	}
	for want, mut := range bad {
		g := ok
		g.IDs = append([]string{}, ok.IDs...)
		mut(&g)
		if err := ValidateHold(HoldOutput{Groups: []HoldGroup{g}}, ms); err == nil || !strings.Contains(err.Error(), strings.Split(want, " ")[0]) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if err := ValidateHold(HoldOutput{}, ms); err == nil {
		t.Error("no groups passed")
	}
	two := HoldOutput{Groups: []HoldGroup{ok, ok}}
	if err := ValidateHold(two, ms); err == nil || !strings.Contains(err.Error(), "two groups") {
		t.Errorf("twice: %v", err)
	}
}

func newDecider(t *testing.T, f *llm.Fake, vars map[string]string) *Decider {
	t.Helper()
	env := map[string]string{"HERALDO_LLM_BASE_URL": "https://llm.example.net/v1", "HERALDO_LLM_DECISIONS_MODEL": "dm"}
	for k, v := range vars {
		env[k] = v
	}
	m, err := settings.New(context.Background(), settings.Options{Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	c := llm.New(llm.Config{Settings: m, NewProvider: f.Factory})
	return New(Config{LLM: c, Settings: m})
}

func TestHoldCircuit(t *testing.T) {
	f := &llm.Fake{Err: errors.New("down")}
	d := newDecider(t, f, map[string]string{"HERALDO_LLM_MODE": "enforce"})
	ms := []Member{{EventID: "e1", Source: "backup", Title: "nas failed", Kind: api.KindMessage, Level: api.LevelActive}}
	var last HoldResult
	for range llm.CircuitFailures + 1 {
		last = d.Hold(context.Background(), ms)
	}
	if len(f.Requests()) != llm.CircuitFailures {
		t.Fatalf("calls %d", len(f.Requests()))
	}
	if len(last.Alone) != 1 || len(last.Groups) != 0 || last.Alone[0].Trace.Reason != "llm: circuit open; forwarded as usual" {
		t.Fatalf("plan %+v", last)
	}
	if d.Mode() != llm.ModeShadow {
		t.Fatalf("mode with the circuit open: %s", d.Mode())
	}
}

func TestHoldExcludedProject(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{{Content: `{"groups":[{"ids":["a1"],"title":"t","summary":"s","verdict":"REAL","suggested_level":"active","confidence":1}]}`}}}
	d := newDecider(t, f, map[string]string{"HERALDO_LLM_MODE": "enforce", "HERALDO_LLM_EXCLUDE_PROJECTS": "secret"})
	ms := []Member{
		{EventID: "e1", Source: "x", Project: "secret", Title: "private thing", Kind: api.KindMessage, Level: api.LevelActive},
		{EventID: "e2", Source: "x", Title: "public thing", Kind: api.KindMessage, Level: api.LevelActive},
	}
	plans := d.Hold(context.Background(), ms)
	if u := f.Requests()[0].Messages[1].Content; strings.Contains(u, "private thing") {
		t.Fatalf("an excluded project went: %s", u)
	}
	if plans.Alone[0].Member.EventID != "e1" || !strings.Contains(plans.Alone[0].Trace.Reason, "project excluded") ||
		len(plans.Groups) != 1 || plans.Groups[0].Members[0].EventID != "e2" || plans.Groups[0].Apply != ApplyGroup {
		t.Fatalf("plans %+v", plans)
	}
}

func TestNoTraceWhenOff(t *testing.T) {
	f := &llm.Fake{}
	d := newDecider(t, f, map[string]string{"HERALDO_LLM_MODE": "off"})
	ms := []Member{{EventID: "e1", Source: "x", Kind: api.KindMessage, Level: api.LevelActive}}
	if p := d.Hold(context.Background(), ms); len(p.Groups)+len(p.Alone) != 0 {
		t.Fatalf("plans %+v", p)
	}
	if a := d.Bundle(context.Background(), BundleAsk{Members: ms}); a.Apply || a.Trace.Reason != "" {
		t.Fatalf("bundle %+v", a)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("mode off called")
	}
}
