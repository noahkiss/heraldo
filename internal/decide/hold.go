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

// Hold triage. The hold asks when an entry reaches its deadline with the
// owner's triage job still silent; a triage release or suppress never goes
// to the LLM. The deadline forward never waits: it goes out at once, and
// the call runs beside it. Hold returns the model's groups after the
// floors; the server applies them to what was already pushed and to the
// members whose deadline is still to come (internal/server/hold_llm.go).

// ErrNotDecided is a replacer's answer for an event whose push is not
// decided yet: the caller may try again.
var ErrNotDecided = errors.New("the event's push is not decided yet")

// MaxHoldBatch caps the entries one triage call sees; the rest keep their
// usual forward.
const MaxHoldBatch = 40

// How a group may be applied.
const (
	// ApplyNone records only: shadow.
	ApplyNone = "none"
	// ApplyText adds the text and changes nothing else: advise, or enforce
	// below MinConfidence.
	ApplyText = "text"
	// ApplyGroup makes the group one notification at Level: enforce.
	ApplyGroup = "group"
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

// GroupPlan is one group of the answer after the mode and the floors.
type GroupPlan struct {
	Members []Member
	Title   string
	Summary string
	// Text is the one line a notification carries: the summary's first
	// line, or the title.
	Text       string
	Verdict    string
	Confidence float64
	// Max is the loudest member's level; Level the level the floors allow
	// the group (passive only for a suppression).
	Max   api.Level
	Level api.Level
	// Want is the level the model asked for, and Refused the floor that
	// said no to it; "" when none did.
	Want    api.Level
	Refused string
	Apply   string
	// Head starts every reason line: "LLM (model, 1.4 s) grouped 3
	// alerts: BLIP".
	Head string
	// Detail is the detail every row of the group shares.
	Detail map[string]any
}

// Suppress reports whether the group may become one passive summary: in
// enforce, with the floors' leave. It acts only on members not pushed yet.
func (g GroupPlan) Suppress() bool {
	return g.Apply == ApplyGroup && g.Level == api.LevelPassive && Rank(g.Max) > Rank(api.LevelPassive)
}

// Note is the refusal, ready to append to a reason: "; LLM suggested
// passive; refused: time-sensitive floor".
func (g GroupPlan) Note() string {
	if g.Refused == "" {
		return ""
	}
	return "; LLM suggested " + string(g.Want) + "; refused: " + g.Refused
}

// Aside is a member no group took, with the row its events get.
type Aside struct {
	Member Member
	Trace  Trace
}

// HoldResult is the answer to one triage call.
type HoldResult struct {
	Groups []GroupPlan
	Alone  []Aside
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

// Hold asks the decisions model to group ms: the entries that just reached
// their deadline and the ones still held beside them. With the mode off it
// returns nothing.
func (d *Decider) Hold(ctx context.Context, ms []Member) HoldResult {
	var out HoldResult
	mode := d.Mode()
	if mode == llm.ModeOff {
		return out
	}
	aside := func(m Member, reason string, det map[string]any) {
		out.Alone = append(out.Alone, Aside{Member: m, Trace: Trace{Reason: reason, Detail: det}})
	}
	in, kept := d.visible(ms)
	for i := range ms {
		if why, ok := kept[i]; ok {
			aside(ms[i], "llm: not consulted: "+why+"; forwarded as usual", map[string]any{"applied": false, "why_not": why})
		}
	}
	if len(in) > MaxHoldBatch {
		for _, m := range in[MaxHoldBatch:] {
			aside(m, fmt.Sprintf("llm: over %d at once; forwarded as usual", MaxHoldBatch), map[string]any{"applied": false})
		}
		in = in[:MaxHoldBatch]
	}
	if len(in) == 0 {
		return out
	}
	inputs := make([]map[string]any, len(in))
	for i, m := range in {
		inputs[i] = d.inputOf(m)
	}
	var answer HoldOutput
	res, guidance, err := d.call(ctx, "hold", map[string]any{"alerts": inputs}, in, holdSchema, func(r llm.Response) error {
		var o HoldOutput
		if err := llm.ParseJSON(r.Content, &o); err != nil {
			return err
		}
		return ValidateHold(o, in)
	})
	if err == nil {
		err = llm.ParseJSON(res.Content, &answer)
	}
	if err != nil {
		t := failTrace(res, err, "forwarded as usual")
		for _, m := range in {
			aside(m, t.Reason, t.Detail)
		}
		return out
	}
	maySuppress := settings.From[bool](d.cfg.Settings, "llm.hold_may_suppress")
	by := map[string]Member{}
	for _, m := range in {
		by[m.Ref] = m
	}
	grouped := map[string]bool{}
	for _, g := range answer.Groups {
		var members []Member
		for _, id := range g.IDs {
			members = append(members, by[id])
			grouped[id] = true
		}
		out.Groups = append(out.Groups, d.planGroup(mode, res, guidance, g, members, maySuppress))
	}
	for _, m := range in {
		if !grouped[m.Ref] {
			det := d.baseDetail(mode, res, guidance)
			det["applied"] = false
			aside(m, who(mode, res)+" left it out of every group; forwarded as usual", det)
		}
	}
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// planGroup applies the mode and the floors to one group.
func (d *Decider) planGroup(mode string, res llm.Result, guidance []string, g HoldGroup, ms []Member, maySuppress bool) GroupPlan {
	p := GroupPlan{Members: ms, Title: cutRunes(strings.TrimSpace(g.Title), 120), Summary: strings.TrimSpace(g.Summary),
		Verdict: g.Verdict, Confidence: g.Confidence, Max: MaxLevel(ms)}
	p.Text = oneLine(p.Summary)
	if p.Text == "" {
		p.Text = p.Title
	}
	p.Want = api.Level(g.SuggestedLevel)
	if maySuppress && (g.Verdict == VerdictBlip || g.Verdict == VerdictFalse) {
		p.Want = api.LevelPassive
	}
	p.Level, p.Refused = HoldFloors(p.Max, api.Level(g.SuggestedLevel), g.Verdict, maySuppress)
	switch {
	case mode == llm.ModeShadow:
		p.Apply = ApplyNone
	case mode == llm.ModeAdvise || g.Confidence < MinConfidence:
		p.Apply = ApplyText
	default:
		p.Apply = ApplyGroup
	}
	p.Head = fmt.Sprintf("%s grouped %s: %s", who(mode, res), plural(len(ms), "alert"), g.Verdict)
	if p.Apply == ApplyText && mode == llm.ModeEnforce {
		p.Head += fmt.Sprintf(" at confidence %.2f (below %.1f): text only", g.Confidence, MinConfidence)
	}
	ids := make([]string, len(ms))
	for i, x := range ms {
		ids[i] = x.EventID
	}
	det := d.baseDetail(mode, res, guidance)
	det["verdict"], det["confidence"], det["suggested_level"], det["title"], det["events"], det["level"] =
		g.Verdict, g.Confidence, g.SuggestedLevel, p.Title, ids, p.Level
	if p.Refused != "" {
		det["refused"] = p.Refused
	}
	p.Detail = det
	return p
}
