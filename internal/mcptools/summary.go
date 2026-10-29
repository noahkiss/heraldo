package mcptools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/noahkiss/heraldo/internal/client"
)

// EventSummary is one event in a list: no body.
type EventSummary struct {
	ID            string   `json:"id"`
	Cursor        string   `json:"cursor,omitempty"`
	ReceivedAt    string   `json:"received_at,omitempty"`
	Source        string   `json:"source"`
	Kind          string   `json:"kind"`
	Action        string   `json:"action,omitempty"`
	Key           string   `json:"key,omitempty"`
	Host          string   `json:"host,omitempty"`
	Project       string   `json:"project,omitempty"`
	Level         string   `json:"level,omitempty"`
	Title         string   `json:"title,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Hold          string   `json:"hold,omitempty"`
	IncidentState string   `json:"incident_state,omitempty"`
	HasBody       bool     `json:"has_body,omitempty"`
	BodyBytes     int      `json:"body_bytes,omitempty"`
	// SummaryFromBody marks a summary taken from the body's first line, so
	// the LLM's privacy policy treats it as body.
	SummaryFromBody bool `json:"summary_from_body,omitempty"`
}

var eventSummarySchema = obj("One event, without its body. event_read gives the body.", map[string]*jsonschema.Schema{
	"id": str("Event id, a ULID."), "cursor": str("The event's list cursor."), "received_at": str("RFC 3339 UTC."),
	"source": str(""), "kind": str(""), "action": str(""), "key": str(""), "host": str(""), "project": str(""),
	"level": str(""), "title": str(""), "summary": str("The event's summary, else the start of its body."),
	"tags": arr("", str("")), "hold": str("hold.state"), "incident_state": str("open, acked or resolved"),
	"has_body": boolean(""), "body_bytes": integer(""),
	"summary_from_body": boolean("The summary is the body's first line."),
})

// rawProject reads project from an event's JSON. ok is false when the JSON
// has no project field.
func rawProject(raw json.RawMessage) (p string, ok bool) {
	var v struct {
		Project *string `json:"project"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Project == nil {
		return "", false
	}
	return *v.Project, true
}

func deref[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func summarize(e client.Event) EventSummary {
	s := EventSummary{
		ID: e.Id, Cursor: deref(e.Cursor), Source: e.Source, Kind: string(e.Kind),
		Action: deref(e.Action), Key: deref(e.Key), Host: deref(e.Host), Title: deref(e.Title),
		Summary: deref(e.Summary),
	}
	if !e.ReceivedAt.IsZero() {
		s.ReceivedAt = e.ReceivedAt.UTC().Format(time.RFC3339)
	}
	if e.Level != nil {
		s.Level = string(*e.Level)
	}
	s.Project, _ = rawProject(e.Raw)
	if e.Tags != nil {
		s.Tags = *e.Tags
	}
	if e.Hold != nil {
		if st, ok := (*e.Hold)["state"].(string); ok {
			s.Hold = st
		}
	}
	if e.Incident != nil {
		s.IncidentState = string(e.Incident.State)
	}
	if e.Body != nil && *e.Body != "" {
		s.HasBody, s.BodyBytes = true, len(*e.Body)
		if s.Summary == "" {
			line, _, _ := strings.Cut(strings.TrimSpace(*e.Body), "\n")
			s.Summary = cutRunes(strings.TrimSpace(line), 160)
			s.SummaryFromBody = s.Summary != ""
		}
	}
	return s
}

// cutRunes keeps the first n characters of s.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// headline is the title, else the summary, else the key.
func (s EventSummary) headline() string {
	for _, v := range []string{s.Title, s.Summary, s.Key} {
		if v != "" {
			return v
		}
	}
	return "(no title)"
}

// line renders one event for the text block.
func (s EventSummary) line() string {
	ka := s.Kind
	if s.Action != "" {
		ka += "/" + s.Action
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s %s", s.ReceivedAt, s.ID, ka, s.Source)
	if s.Key != "" {
		b.WriteString(" key=" + s.Key)
	}
	if s.Project != "" {
		b.WriteString(" project=" + s.Project)
	}
	if s.IncidentState != "" {
		b.WriteString(" [" + s.IncidentState + "]")
	}
	b.WriteString(": " + cutRunes(s.headline(), 120))
	return b.String()
}

// projectSet holds a project filter, case-folded.
type projectSet []string

func newProjectSet(ps []string) projectSet {
	var out projectSet
	for _, p := range ps {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// wantsNone reports whether the filter asks for events with no project.
func (ps projectSet) wantsNone() bool {
	for _, p := range ps {
		if p == "-" {
			return true
		}
	}
	return false
}

// ignored reports whether a server answer shows the server ignored the
// filter: an item has no project field at all. An item with another project
// may be an alias the server expanded, so it does not count.
func (ps projectSet) ignored(raws []json.RawMessage) bool {
	if len(ps) == 0 || ps.wantsNone() {
		return false
	}
	for _, r := range raws {
		if _, ok := rawProject(r); !ok {
			return true
		}
	}
	return false
}

func unsupportedFilter() *ToolError {
	return toolErr("unsupported_filter", "this heraldo server ignores the project filter: it has no project field yet",
		"call again without project, or upgrade the server")
}
