package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/noahkiss/heraldo/internal/llm"
)

// Bundle titles. The pusher sends a view's bundle notification first, as it
// always has, then asks here. An answer that comes back while the window is
// still open replaces the notification through the same collapse id. The
// LLM changes only the text: never the level, the count or which events
// bundle.

// MaxBundleMembers caps the events one bundle call sees: the newest.
const MaxBundleMembers = 20

// BundleText is the pusher's bundle text hook: Decider.Bundle is one.
type BundleText func(ctx context.Context, ask BundleAsk) BundleAnswer

// BundleAsk is one bundle window as the pusher sees it.
type BundleAsk struct {
	View     string
	ViewName string
	// Count is how many pushes the window holds.
	Count int
	// Members are the window's events, oldest first.
	Members []Member
}

// BundleAnswer is what the bundle notification may carry.
type BundleAnswer struct {
	// Apply is set when the pusher may send Title and Summary: advise or
	// enforce, and a valid answer.
	Apply   bool
	Title   string
	Summary string
	// Trace is the row for the bundle's latest event; an empty Reason
	// writes none.
	Trace Trace
}

type bundleOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

var bundleSchema = &llm.Schema{Name: "bundle_title", Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "summary"],
  "properties": {
    "title": {"type": "string", "maxLength": 80},
    "summary": {"type": "string", "maxLength": 200}
  }
}`)}

func validateBundle(o bundleOutput) error {
	if t := strings.TrimSpace(o.Title); t == "" || len([]rune(t)) > 80 || strings.Contains(t, "\n") {
		return errors.New("title must be one line of 1 to 80 characters")
	}
	if s := strings.TrimSpace(o.Summary); s == "" || len([]rune(s)) > 200 || strings.Contains(s, "\n") {
		return errors.New("summary must be one line of 1 to 200 characters")
	}
	return nil
}

// Bundle asks for a title and a one-line summary of a bundle window. It
// never errors: a failure is an answer without Apply, and its trace says
// why.
func (d *Decider) Bundle(ctx context.Context, ask BundleAsk) BundleAnswer {
	mode := d.Mode()
	if mode == llm.ModeOff {
		return BundleAnswer{}
	}
	ms := ask.Members
	if len(ms) > MaxBundleMembers {
		ms = ms[len(ms)-MaxBundleMembers:]
	}
	in, _ := d.visible(ms)
	if len(in) == 0 {
		return BundleAnswer{Trace: Trace{Reason: "llm: not consulted: no event in the bundle may go to the LLM; the bundle keeps its own text",
			Detail: map[string]any{"applied": false}}}
	}
	inputs := make([]map[string]any, len(in))
	for i, m := range in {
		inputs[i] = d.inputOf(m)
	}
	var out bundleOutput
	res, guidance, err := d.call(ctx, "bundle", map[string]any{"view": ask.ViewName, "count": ask.Count, "events": inputs},
		in, bundleSchema, func(r llm.Response) error {
			var o bundleOutput
			if err := llm.ParseJSON(r.Content, &o); err != nil {
				return err
			}
			return validateBundle(o)
		})
	if err == nil {
		err = llm.ParseJSON(res.Content, &out)
	}
	if err != nil {
		return BundleAnswer{Trace: failTrace(res, err, "the bundle keeps its own text")}
	}
	title, summary := strings.TrimSpace(out.Title), strings.TrimSpace(out.Summary)
	det := d.baseDetail(mode, res, guidance)
	det["title"], det["summary"], det["view"], det["count"] = title, summary, ask.View, ask.Count
	if mode == llm.ModeShadow {
		det["applied"] = false
		return BundleAnswer{Trace: Trace{Reason: fmt.Sprintf("%s would title the bundle %q; nothing changed (shadow)", who(mode, res), title),
			Detail: det}}
	}
	det["applied"] = "text"
	return BundleAnswer{Apply: true, Title: title, Summary: summary,
		Trace: Trace{Reason: fmt.Sprintf("%s titled the bundle %q", who(mode, res), title), Detail: det}}
}
