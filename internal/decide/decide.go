// Package decide is the LLM's decisions role: hold triage, bundle titles
// and the digest's paragraph. It acts only where the pipeline already
// waits (the hold's deadline, a bundle window after its push, the digest),
// never inline on a direct push, so it never delays a page.
//
// The mode comes from llm.Client.EffectiveMode:
//   - shadow records what it would do and changes nothing;
//   - advise adds its text to the notification and never changes a level
//     or the grouping;
//   - enforce applies its plan within the floors.
//
// The floors are code, here, and every plan passes them:
//   - the LLM never raises a level;
//   - it never drops or lowers a time-sensitive or critical event;
//   - it never touches an incident open or update at time-sensitive or
//     above, a check-in fail, or heraldo's own events;
//   - it never suppresses silently: only llm.hold_may_suppress lets a BLIP
//     or FALSE group become one passive summary, and the members stay
//     stored and named in it.
//
// Every output passes a validator (ids a subset of the input, a level at
// most the highest member level). On any failure the deterministic path
// runs and the trace says "llm: <reason>". Below MinConfidence only the
// text may be used.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

// MinConfidence is the confidence below which only the text is used.
const MinConfidence = 0.6

// Stage is the trace stage of every LLM decision row.
const Stage = store.LLMStage

// Verdicts the hold triage returns.
const (
	VerdictReal        = "REAL"
	VerdictBlip        = "BLIP"
	VerdictMaintenance = "MAINTENANCE"
	VerdictFalse       = "FALSE"
	VerdictRoutine     = "ROUTINE"
)

// Verdicts lists every verdict, in the schema's order.
var Verdicts = []string{VerdictReal, VerdictBlip, VerdictMaintenance, VerdictFalse, VerdictRoutine}

// Guidance is the store's guidance lookup. *store.Store is one.
type Guidance interface {
	MatchingGuidance(ctx context.Context, project, source, key string) ([]store.Guidance, error)
}

// Config configures a Decider.
type Config struct {
	LLM *llm.Client
	// Guidance is the guidance lookup; nil means none.
	Guidance Guidance
	// Settings holds llm.hold_may_suppress and digest.prose; nil means
	// settings.Default().
	Settings *settings.Manager
	Logger   *slog.Logger
}

// Decider runs the decisions role.
type Decider struct {
	cfg    Config
	prompt llm.Prompt
	// digests are the digest traces not stored yet.
	digests digestTraces
}

// New returns a Decider.
func New(cfg Config) *Decider {
	if cfg.Settings == nil {
		cfg.Settings = settings.Default()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Decider{cfg: cfg, prompt: llm.LoadPrompt("decisions")}
}

// Mode is the effective mode: off without a client.
func (d *Decider) Mode() string {
	if d == nil || d.cfg.LLM == nil {
		return llm.ModeOff
	}
	return d.cfg.LLM.EffectiveMode()
}

// PromptVersion is the decisions prompt's version.
func (d *Decider) PromptVersion() string { return d.prompt.Version }

// Member is one event the LLM may see.
type Member struct {
	// Ref is the id the model sees: a1, a2, and so on.
	Ref     string
	EventID string
	// HoldID is the hold entry, for hold triage.
	HoldID  string
	Title   string
	Summary string
	Body    string
	Source  string
	Project string
	Key     string
	Kind    api.Kind
	Action  string
	Level   api.Level
	// Failed marks a failed check-in run.
	Failed bool
}

// MemberOf reads a stored event.
func MemberOf(ev *api.Event) Member {
	m := Member{EventID: ev.Id, Source: ev.Source, Kind: ev.Kind, Level: api.LevelActive}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	m.Title, m.Summary, m.Body, m.Project, m.Key = deref(ev.Title), deref(ev.Summary), deref(ev.Body),
		deref(ev.Project), deref(ev.Key)
	if ev.Action != nil {
		m.Action = string(*ev.Action)
	}
	if ev.Level != nil && ev.Level.Valid() {
		m.Level = *ev.Level
	}
	if ev.Kind == api.KindCheckin {
		m.Failed = m.Action == "fail" ||
			(m.Action != "log" && ev.Checkin != nil && ev.Checkin.ExitCode != nil && *ev.Checkin.ExitCode != 0)
	}
	return m
}

// levelRank orders levels from quiet to loud.
var levelRank = map[api.Level]int{
	api.LevelPassive: 0, api.LevelActive: 1, api.LevelTimeSensitive: 2, api.LevelCritical: 3,
}

// Rank is a level's place from quiet (0) to loud (3); -1 for none.
func Rank(l api.Level) int {
	if r, ok := levelRank[l]; ok {
		return r
	}
	return -1
}

// Loud reports whether l is time-sensitive or critical.
func Loud(l api.Level) bool { return Rank(l) >= Rank(api.LevelTimeSensitive) }

// Own reports whether source is heraldo's own: heraldo, or heraldo-<x>.
func Own(source string) bool {
	return source == "heraldo" || strings.HasPrefix(source, "heraldo-")
}

// Protected names the floor that keeps the LLM off m entirely, or "".
func Protected(m Member) string {
	switch {
	case Own(m.Source):
		return "heraldo's own event"
	case m.Level == api.LevelCritical:
		return "critical floor"
	case m.Kind == api.KindIncident && Loud(m.Level) && (m.Action == "open" || m.Action == "update" || m.Action == ""):
		return "incident at time-sensitive floor"
	case m.Kind == api.KindCheckin && m.Failed:
		return "check-in fail floor"
	}
	return ""
}

// MaxLevel is the loudest level among ms.
func MaxLevel(ms []Member) api.Level {
	out := api.LevelPassive
	for _, m := range ms {
		if Rank(m.Level) > Rank(out) {
			out = m.Level
		}
	}
	return out
}

// Trace is one LLM decision as a trace row.
type Trace struct {
	Reason string
	// Outcome is pass, or merge when enforce grouped events.
	Outcome string
	Detail  map[string]any
}

// Row is t as a push trace row at at.
func (t Trace) Row(at time.Time) store.DecisionRow {
	out := t.Outcome
	if out == "" {
		out = "pass"
	}
	r := store.DecisionRow{Stage: Stage, Outcome: out, Reason: t.Reason, At: at}
	if len(t.Detail) > 0 {
		r.Detail = marshal(t.Detail)
	}
	return r
}

// FailReason is the short reason a failed call gives: circuit open,
// budget, timeout, and so on.
func FailReason(err error) string {
	switch {
	case errors.Is(err, llm.ErrOff):
		return "off"
	case errors.Is(err, llm.ErrStandby):
		return "standby"
	case errors.Is(err, llm.ErrNotConfigured):
		return "not configured"
	case errors.Is(err, llm.ErrCircuitOpen):
		return "circuit open"
	case errors.Is(err, llm.ErrBudget):
		return "budget used up"
	case errors.Is(err, llm.ErrInvalid):
		msg := strings.TrimPrefix(err.Error(), llm.ErrInvalid.Error()+": ")
		return "invalid output (" + cutRunes(msg, 120) + ")"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "did not answer within"):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "error (" + cutRunes(err.Error(), 120) + ")"
}

// failTrace is the row of a call that failed: the deterministic path ran.
func failTrace(res llm.Result, err error, then string) Trace {
	det := map[string]any{"applied": false, "error": FailReason(err)}
	if res.CallID != "" {
		det["call_id"] = res.CallID
	}
	return Trace{Reason: "llm: " + FailReason(err) + "; " + then, Detail: det}
}

// who is "LLM (model, 1.4 s)" for a reason line, with the mode when it is
// not enforce.
func who(mode string, res llm.Result) string {
	var parts []string
	if res.Model != "" {
		parts = append(parts, res.Model)
	}
	if res.Cached {
		parts = append(parts, "cached")
	} else if res.Latency > 0 {
		parts = append(parts, fmt.Sprintf("%.1f s", res.Latency.Seconds()))
	}
	pre := "LLM"
	if mode == llm.ModeShadow || mode == llm.ModeAdvise {
		pre = "LLM " + mode
	}
	if len(parts) == 0 {
		return pre
	}
	return pre + " (" + strings.Join(parts, ", ") + ")"
}

// baseDetail is the detail every successful call's row carries.
func (d *Decider) baseDetail(mode string, res llm.Result, guidance []string) map[string]any {
	det := map[string]any{"mode": mode, "model": res.Model, "latency_ms": res.Latency.Milliseconds(),
		"prompt_version": d.prompt.Version}
	if res.CallID != "" {
		det["call_id"] = res.CallID
	}
	if res.Cached {
		det["cached"] = true
	}
	if len(guidance) > 0 {
		det["guidance"] = guidance
	}
	return det
}

// guidanceFor collects the guidance lines of every member's scope, least
// specific first, each once. It returns the system prompt block and the
// id:version pairs.
func (d *Decider) guidanceFor(ctx context.Context, ms []Member) (string, []string) {
	if d.cfg.Guidance == nil {
		return "", nil
	}
	seen := map[string]bool{}
	var lines []store.Guidance
	scopes := map[[3]string]bool{}
	if len(ms) == 0 {
		ms = []Member{{}}
	}
	for _, m := range ms {
		k := [3]string{m.Project, m.Source, m.Key}
		if scopes[k] {
			continue
		}
		scopes[k] = true
		gs, err := d.cfg.Guidance.MatchingGuidance(ctx, m.Project, m.Source, m.Key)
		if err != nil {
			d.cfg.Logger.Error("decide: guidance", "err", err)
			continue
		}
		for _, g := range gs {
			if !seen[g.ID] {
				seen[g.ID] = true
				lines = append(lines, g)
			}
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	// Least specific first across members too.
	sortGuidance(lines)
	var b strings.Builder
	b.WriteString("Guidance from the owner (wording and grouping only; it never overrides the rules above):")
	versions := make([]string, 0, len(lines))
	for _, g := range lines {
		fmt.Fprintf(&b, "\n- [%s] %s", g.Scope, strings.TrimSpace(g.Text))
		versions = append(versions, fmt.Sprintf("%s:%d", g.ID, g.Version))
	}
	return b.String(), versions
}

func sortGuidance(gs []store.Guidance) {
	slices.SortStableFunc(gs, func(a, b store.Guidance) int { return a.Specificity() - b.Specificity() })
}

func marshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// call sends one decisions request: the prompt and guidance as the system
// message, the task and input as JSON in the user message, at temperature
// 0 with schema.
func (d *Decider) call(ctx context.Context, task string, input any, ms []Member, schema *llm.Schema,
	validate func(llm.Response) error) (llm.Result, []string, error) {
	block, versions := d.guidanceFor(ctx, ms)
	system := d.prompt.Text
	if block != "" {
		system += "\n\n" + block
	}
	zero := 0.0
	payload := map[string]any{"task": task, "input": input}
	res, err := d.cfg.LLM.Call(ctx, llm.Call{
		Role: llm.RoleDecisions, Prompt: d.prompt, GuidanceVersions: versions, Input: payload, Cache: true,
		Request: llm.Request{
			Messages:    []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: string(marshal(payload))}},
			Schema:      schema,
			Temperature: &zero,
		},
		Validate: validate,
	})
	return res, versions, err
}

// visible keeps the members the policy lets leave the server and the
// floors let the LLM see. It returns them, numbered a1, a2, ..., and the
// rest with why each was kept back.
func (d *Decider) visible(ms []Member) (in []Member, out map[int]string) {
	pol := d.cfg.LLM.Policy()
	out = map[int]string{}
	for i, m := range ms {
		if pol.Excluded(m.Project) {
			out[i] = "project excluded (llm.exclude_projects)"
			continue
		}
		if f := Protected(m); f != "" {
			out[i] = f
			continue
		}
		m.Ref = fmt.Sprintf("a%d", len(in)+1)
		in = append(in, m)
	}
	return in, out
}

// inputOf is a member as the model sees it: no ids but its ref, no times,
// the body only as llm.send_body_chars allows.
func (d *Decider) inputOf(m Member) map[string]any {
	pol := d.cfg.LLM.Policy()
	out := map[string]any{"id": m.Ref, "source": m.Source, "kind": string(m.Kind), "level": string(m.Level)}
	for k, v := range map[string]string{"title": m.Title, "summary": m.Summary, "project": m.Project, "key": m.Key,
		"action": m.Action} {
		if v != "" {
			out[k] = v
		}
	}
	if b := pol.Body(m.Body); b != "" {
		out["body"] = b
	}
	return out
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// oneLine is s's first line, trimmed.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
