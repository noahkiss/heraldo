package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/settings"
)

// Hold triage. The hold asks at the deadline only: entries the owner's
// triage job did not close in time. A triage release or suppress is never
// sent to the LLM, so the two never race.

// MaxHoldBatch caps the entries one triage call sees; the rest forward as
// usual.
const MaxHoldBatch = 40

// Hold plan actions.
const (
	// ActForward forwards the entry as the hold always has. Text, when
	// set, is the LLM's one-line summary, added as the event's summary.
	ActForward = "forward"
	// ActRelease closes the group with one event at Level, Title and
	// Summary, as a triage release does.
	ActRelease = "release"
)

// HoldGroup is one group in the model's answer.
type HoldGroup struct {
	IDs            []string `json:"ids"`
	Title          string   `json:"title"`
	Summary        string   `json:"summary"`
	Verdict        string   `json:"verdict"`
	SuggestedLevel string   `json:"suggested_level"`
	Confidence     float64  `json:"confidence"`
}

// HoldOutput is the model's answer to a hold task.
type HoldOutput struct {
	Groups []HoldGroup `json:"groups"`
}

// HoldPlan is what to do with some of the due entries.
type HoldPlan struct {
	Action  string
	Members []Member
	// Text is the summary a forward carries; "" adds none.
	Text string
	// Title, Summary and Level are a release's.
	Title   string
	Summary string
	Level   api.Level
	Verdict string
	// Trace is the row each member's event (and a release's event) gets;
	// an empty Reason writes none.
	Trace Trace
}

var holdSchema = &llm.Schema{Name: "hold_triage", Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["groups"],
  "properties": {
    "groups": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["ids", "title", "summary", "verdict", "suggested_level", "confidence"],
        "properties": {
          "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1},
          "title": {"type": "string", "maxLength": 120},
          "summary": {"type": "string", "maxLength": 400},
          "verdict": {"type": "string", "enum": ["REAL", "BLIP", "MAINTENANCE", "FALSE", "ROUTINE"]},
          "suggested_level": {"type": "string", "enum": ["passive", "active", "time-sensitive", "critical"]},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1}
        }
      }
    }
  }
}`)}

// ValidateHold checks an answer against the members it was asked about:
// every id one of theirs and in one group only, a known verdict, a level
// no louder than the group's loudest member, a confidence from 0 to 1, a
// title, and a summary of at most two lines.
func ValidateHold(out HoldOutput, ms []Member) error {
	by := map[string]Member{}
	for _, m := range ms {
		by[m.Ref] = m
	}
	if len(out.Groups) == 0 {
		return errors.New("no groups")
	}
	seen := map[string]bool{}
	for i, g := range out.Groups {
		if len(g.IDs) == 0 {
			return fmt.Errorf("group %d has no ids", i+1)
		}
		var group []Member
		for _, id := range g.IDs {
			m, ok := by[id]
			if !ok {
				return fmt.Errorf("group %d: id %q is not in the input", i+1, cutRunes(id, 40))
			}
			if seen[id] {
				return fmt.Errorf("id %q is in two groups", id)
			}
			seen[id] = true
			group = append(group, m)
		}
		if !slices.Contains(Verdicts, g.Verdict) {
			return fmt.Errorf("group %d: verdict %q", i+1, cutRunes(g.Verdict, 40))
		}
		lvl := api.Level(g.SuggestedLevel)
		if !lvl.Valid() {
			return fmt.Errorf("group %d: suggested_level %q", i+1, cutRunes(g.SuggestedLevel, 40))
		}
		if Rank(lvl) > Rank(MaxLevel(group)) {
			return fmt.Errorf("group %d: suggested_level %s is louder than its members (%s)", i+1, lvl, MaxLevel(group))
		}
		if g.Confidence < 0 || g.Confidence > 1 {
			return fmt.Errorf("group %d: confidence %v", i+1, g.Confidence)
		}
		if strings.TrimSpace(g.Title) == "" || len([]rune(g.Title)) > 120 {
			return fmt.Errorf("group %d: title must be 1 to 120 characters", i+1)
		}
		if len([]rune(g.Summary)) > 400 || strings.Count(strings.TrimSpace(g.Summary), "\n") > 1 {
			return fmt.Errorf("group %d: summary must be at most two lines", i+1)
		}
	}
	return nil
}

// HoldFloors is the level a group may release at, from its loudest member
// M, the suggested level and the verdict, and the floor that refused the
// suggestion, if one did. Only a BLIP or FALSE verdict with
// llm.hold_may_suppress on may go passive, and nothing time-sensitive or
// louder is ever lowered.
func HoldFloors(m, suggested api.Level, verdict string, maySuppress bool) (api.Level, string) {
	suppress := maySuppress && (verdict == VerdictBlip || verdict == VerdictFalse)
	want := suggested
	if suppress {
		want = api.LevelPassive
	}
	switch {
	case Rank(want) > Rank(m):
		return m, "never raises a level"
	case Rank(want) == Rank(m):
		return m, ""
	case m == api.LevelCritical:
		return m, "critical floor"
	case Loud(m):
		return m, "time-sensitive floor"
	case want == api.LevelPassive && !suppress:
		if !maySuppress {
			return m, "suppression needs llm.hold_may_suppress"
		}
		return m, "only a BLIP or FALSE verdict may suppress"
	}
	return want, ""
}

// Hold asks the decisions model to group the due entries ms and returns a
// plan for every one of them. With the mode off it returns plain forwards
// and no trace.
func (d *Decider) Hold(ctx context.Context, ms []Member) []HoldPlan {
	mode := d.Mode()
	forward := func(m Member, t Trace) HoldPlan { return HoldPlan{Action: ActForward, Members: []Member{m}, Trace: t} }
	var plans []HoldPlan
	if mode == llm.ModeOff {
		for _, m := range ms {
			plans = append(plans, forward(m, Trace{}))
		}
		return plans
	}
	in, kept := d.visible(ms)
	for i := range ms {
		why, ok := kept[i]
		if !ok {
			continue
		}
		plans = append(plans, forward(ms[i], Trace{Reason: "llm: not consulted: " + why + "; forwarded as usual",
			Detail: map[string]any{"applied": false, "why_not": why}}))
	}
	if len(in) > MaxHoldBatch {
		for _, m := range in[MaxHoldBatch:] {
			plans = append(plans, forward(m, Trace{Reason: fmt.Sprintf("llm: over %d due at once; forwarded as usual", MaxHoldBatch),
				Detail: map[string]any{"applied": false}}))
		}
		in = in[:MaxHoldBatch]
	}
	if len(in) == 0 {
		return plans
	}
	inputs := make([]map[string]any, len(in))
	for i, m := range in {
		inputs[i] = d.inputOf(m)
	}
	var out HoldOutput
	res, guidance, err := d.call(ctx, "hold", map[string]any{"alerts": inputs}, in, holdSchema, func(r llm.Response) error {
		var o HoldOutput
		if err := llm.ParseJSON(r.Content, &o); err != nil {
			return err
		}
		return ValidateHold(o, in)
	})
	if err == nil {
		err = llm.ParseJSON(res.Content, &out)
	}
	if err != nil {
		t := failTrace(res, err, "forwarded as usual")
		for _, m := range in {
			plans = append(plans, forward(m, t))
		}
		return plans
	}
	maySuppress := settings.From[bool](d.cfg.Settings, "llm.hold_may_suppress")
	by := map[string]Member{}
	for _, m := range in {
		by[m.Ref] = m
	}
	grouped := map[string]bool{}
	for _, g := range out.Groups {
		var members []Member
		for _, id := range g.IDs {
			members = append(members, by[id])
			grouped[id] = true
		}
		plans = append(plans, d.planGroup(mode, res, guidance, g, members, maySuppress)...)
	}
	for _, m := range in {
		if !grouped[m.Ref] {
			det := d.baseDetail(mode, res, guidance)
			det["applied"] = false
			plans = append(plans, forward(m, Trace{Reason: who(mode, res) + " left it out of every group; forwarded as usual", Detail: det}))
		}
	}
	return plans
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// planGroup applies the mode and the floors to one group.
func (d *Decider) planGroup(mode string, res llm.Result, guidance []string, g HoldGroup, ms []Member, maySuppress bool) []HoldPlan {
	m := MaxLevel(ms)
	sug := api.Level(g.SuggestedLevel)
	level, refused := HoldFloors(m, sug, g.Verdict, maySuppress)
	title, summary := cutRunes(strings.TrimSpace(g.Title), 120), strings.TrimSpace(g.Summary)
	text := oneLine(summary)
	if text == "" {
		text = title
	}
	n := len(ms)
	ids := make([]string, n)
	for i, x := range ms {
		ids[i] = x.EventID
	}
	det := d.baseDetail(mode, res, guidance)
	det["verdict"], det["confidence"], det["suggested_level"], det["title"], det["events"] =
		g.Verdict, g.Confidence, g.SuggestedLevel, title, ids

	outcome := "released as 1 " + string(level) + " push"
	if level == api.LevelPassive {
		outcome = "released as 1 passive summary"
	}
	if n == 1 && level == m {
		outcome = "forwarded with its summary"
	}
	note := ""
	if refused != "" {
		want := sug
		if maySuppress && (g.Verdict == VerdictBlip || g.Verdict == VerdictFalse) {
			want = api.LevelPassive
		}
		note = "; LLM suggested " + string(want) + "; refused: " + refused
		det["refused"] = refused
	}
	head := fmt.Sprintf("%s grouped %s: %s", who(mode, res), plural(n, "alert"), g.Verdict)
	each := func(text string, t Trace) []HoldPlan {
		out := make([]HoldPlan, n)
		for i, x := range ms {
			out[i] = HoldPlan{Action: ActForward, Members: []Member{x}, Text: text, Verdict: g.Verdict, Trace: t}
		}
		return out
	}
	switch {
	case mode == llm.ModeShadow:
		det["applied"], det["would"] = false, outcome
		return each("", Trace{Reason: fmt.Sprintf("%s grouped %s: %s → would be %s%s; nothing changed (shadow)",
			who(mode, res), plural(n, "alert"), g.Verdict, outcome, note), Detail: det})
	case mode == llm.ModeAdvise:
		det["applied"], det["text"] = "text", text
		return each(text, Trace{Reason: head + "; added its summary; level and grouping unchanged (advise)", Detail: det})
	case g.Confidence < MinConfidence:
		det["applied"], det["text"], det["why_not"] = "text", text, "low confidence"
		return each(text, Trace{Reason: fmt.Sprintf("%s at confidence %.2f (below %.1f): text only%s", head, g.Confidence,
			MinConfidence, note), Detail: det})
	case n == 1 && level == m:
		det["applied"], det["text"] = "text", text
		return each(text, Trace{Reason: head + " → " + outcome + note, Detail: det})
	}
	det["applied"], det["level"] = true, level
	return []HoldPlan{{Action: ActRelease, Members: ms, Title: title, Summary: summary, Level: level, Verdict: g.Verdict,
		Trace: Trace{Outcome: "merge", Reason: head + " → " + outcome + note, Detail: det}}}
}
