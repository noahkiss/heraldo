package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
)

type activityIn struct {
	Key           string `json:"key" jsonschema:"The activity's progress key, such as print/42."`
	TimelineLimit *int   `json:"timeline_limit,omitempty" jsonschema:"How many of the newest timeline entries to keep. Default 50."`
}

const activityDesc = `Read one Live Activity: merged content, the instance on each device and its state, and the event timeline.

Best for: a key from overview's activities.
Returns: the Activity; timeline trimmed to the newest timeline_limit entries (default 50), with timeline_total.
Not for: listing activities; overview lists the running ones.`

func (t *tools) addActivityRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "activity_read", Description: activityDesc, Annotations: readOnly("Read activity")},
		func(is *jsonschema.Schema) {
			k := is.Properties["key"]
			k.MinLength = ptr(1)
			maxLen(k, 255)
			tl := is.Properties["timeline_limit"]
			tl.Types, tl.Type = nil, "integer"
			between(tl, 0, 200)
			def(tl, 50)
		},
		map[string]*jsonschema.Schema{
			"key": str(""), "id": str("act_ plus the id of the event that began it."), "state": str("active or ended"),
			"source": str(""), "content": anyObj("LiveContent, merged."), "started_at": str(""), "updated_at": str(""),
			"ended_at": str(""), "end_reason": str(""),
			"instances":      arr("One per device link.", anyObj("")),
			"timeline":       arr("Oldest first: id, received_at, action, content.", anyObj("")),
			"timeline_total": integer("Entries the server returned before the trim."),
		},
		t.activityRead)
}

func (t *tools) activityRead(ctx context.Context, _ *mcp.CallToolRequest, in activityIn) (result, error) {
	if in.Key == "" {
		return result{}, toolErr("invalid", "key is required", "overview lists running activities")
	}
	limit := 50
	if in.TimelineLimit != nil {
		limit = *in.TimelineLimit
	}
	a, err := t.API.Activity(ctx, in.Key)
	if err != nil {
		return result{}, t.apiErr(err, call{what: "activity on key " + in.Key, lister: "overview"})
	}
	total := 0
	if a.Timeline != nil {
		tl := *a.Timeline
		total = len(tl)
		if len(tl) > limit {
			tl = tl[len(tl)-limit:]
		}
		a.Timeline = &tl
	}
	b, err := json.Marshal(a)
	if err != nil {
		return result{}, err
	}
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	m["timeline_total"] = total
	return result{out: m, text: activityText(a, total)}, nil
}

func liveLine(c api.LiveContent) string {
	var parts []string
	if v, err := c.Title.Get(); err == nil && v != "" {
		parts = append(parts, v)
	}
	if v, err := c.Status.Get(); err == nil && v != "" {
		parts = append(parts, v)
	}
	if v, err := c.Progress.Get(); err == nil {
		parts = append(parts, fmt.Sprintf("%.0f%%", v*100))
	}
	if v, err := c.Steps.Get(); err == nil {
		parts = append(parts, fmt.Sprintf("step %d/%d", v.Current, v.Total))
	}
	return strings.Join(parts, " · ")
}

func activityText(a api.Activity, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, %s) %s: %s\n", a.Key, a.Id, a.Source, a.State, liveLine(a.Content))
	fmt.Fprintf(&b, "started %s, updated %s", a.StartedAt.UTC().Format(time.RFC3339), a.UpdatedAt.UTC().Format(time.RFC3339))
	if a.EndedAt != nil {
		fmt.Fprintf(&b, ", ended %s", a.EndedAt.UTC().Format(time.RFC3339))
		if a.EndReason != nil {
			b.WriteString(" (" + string(*a.EndReason) + ")")
		}
	}
	b.WriteString("\n")
	for _, i := range a.Instances {
		fmt.Fprintf(&b, "device %s link %d: %s\n", i.Device, i.Chain, i.State)
	}
	if a.Timeline != nil {
		fmt.Fprintf(&b, "timeline: %d of %d entries\n", len(*a.Timeline), total)
		for _, e := range *a.Timeline {
			line := ""
			if e.Content != nil {
				line = liveLine(*e.Content)
			}
			fmt.Fprintf(&b, "%s %s %s\n", e.ReceivedAt.UTC().Format(time.RFC3339), e.Action, line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
