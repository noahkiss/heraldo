package mcptools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
)

// ---- delivery_read ------------------------------------------------------

type deliveryReadIn struct {
	Sources bool `json:"sources,omitempty" jsonschema:"Also list every source, new ones first, with its 30-day count and how many of those counted toward the badge."`
}

const deliveryReadDesc = `Read the delivery settings: the owner's rows that say, per source, tag, level, kind, project, ingest rule or key prefix, whether an event is delivered as a banner, silently (inbox only), filed for the digest, or dropped, and whether it counts toward the badge.

Best for: explaining why an event arrived silent, was filed or dropped (its deliver field names the row), or why the badge counts what it counts.
Returns: enabled (the rows act only while it is on), new_source_level, and the rows in order (the first match wins). With sources=true, every source with new, its 30-day count, badge_in and badge_out.
Not for: changing a row; the owner does that in the apps or with heraldo delivery.`

func (t *tools) addDeliveryRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "delivery_read", Description: deliveryReadDesc, Annotations: readOnly("Read delivery settings")},
		nil,
		map[string]*jsonschema.Schema{
			"enabled":          {Type: "boolean", Description: "delivery.enabled: while it is off, the rows do nothing."},
			"new_source_level": str("The level of a new source's event that carries none."),
			"settings": arr("The rows, by position.", obj("", map[string]*jsonschema.Schema{
				"id": str(""), "match": anyObj(""), "deliver": str("banner, silent, digest or drop; absent: no change."),
				"badge": str("auto, on or off."), "default_level": str(""), "position": integer(""), "note": str(""),
			})),
			"sources": arr("With sources=true.", obj("", map[string]*jsonschema.Schema{
				"source": str(""), "new": {Type: "boolean"}, "events": integer("In the last 30 days, routine left out."),
				"badge_in": integer(""), "badge_out": integer(""),
			})),
		},
		t.deliveryRead)
}

func (t *tools) deliveryRead(ctx context.Context, _ *mcp.CallToolRequest, in deliveryReadIn) (result, error) {
	l, err := t.API.Delivery(ctx)
	if err != nil {
		return result{}, t.apiErr(err, call{})
	}
	rows := make([]map[string]any, 0, len(l.Settings))
	state := "off: the rows do nothing"
	if l.Enabled {
		state = "on"
	}
	lines := []string{fmt.Sprintf("delivery settings %s; a new source's level-less events get %s", state, l.NewSourceLevel)}
	for _, d := range l.Settings {
		o := map[string]any{"id": d.Id, "match": d.Match, "badge": d.Badge, "position": d.Position}
		line := d.Id + ": " + deliveryMatchText(d.Match)
		if d.Deliver != nil {
			o["deliver"] = *d.Deliver
			line += " -> " + string(*d.Deliver)
		}
		if d.Badge != "auto" {
			line += ", badge " + string(d.Badge)
		}
		if d.DefaultLevel != nil {
			o["default_level"] = *d.DefaultLevel
			line += ", default level " + string(*d.DefaultLevel)
		}
		if d.Note != nil {
			o["note"] = *d.Note
			line += " (" + *d.Note + ")"
		}
		rows = append(rows, o)
		lines = append(lines, line)
	}
	out := map[string]any{"enabled": l.Enabled, "new_source_level": l.NewSourceLevel, "settings": rows}
	if in.Sources {
		srcs, err := t.API.Sources(ctx)
		if err != nil {
			return result{}, t.apiErr(err, call{})
		}
		list := make([]map[string]any, 0, len(srcs))
		for _, s := range srcs {
			list = append(list, map[string]any{"source": s.Source, "new": s.New, "events": s.Events,
				"badge_in": s.BadgeIn, "badge_out": s.BadgeOut})
			n := ""
			if s.New {
				n = " (new)"
			}
			lines = append(lines, fmt.Sprintf("source %s%s: %d events in 30 days, %d counted toward the badge",
				s.Source, n, s.Events, s.BadgeIn))
		}
		out["sources"] = list
	}
	return result{out: out, text: strings.Join(lines, "\n")}, nil
}

func deliveryMatchText(m api.DeliveryMatch) string {
	var parts []string
	add := func(k string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, k+"="+*v)
		}
	}
	add("source", m.Source)
	add("tag", m.Tag)
	add("project", m.Project)
	add("rule", m.Rule)
	add("key", m.Key)
	if m.Kind != nil {
		parts = append(parts, "kind="+string(*m.Kind))
	}
	if m.Level != nil && len(*m.Level) > 0 {
		var ls []string
		for _, l := range *m.Level {
			ls = append(ls, string(l))
		}
		parts = append(parts, "level="+strings.Join(ls, ","))
	}
	if len(parts) == 0 {
		return "every event"
	}
	return strings.Join(parts, " ")
}
