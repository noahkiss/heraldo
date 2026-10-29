package mcptools

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- views_read ---------------------------------------------------------

type viewsReadIn struct {
	Name string `json:"name,omitempty" jsonschema:"One view, by name or id. Omit for every view."`
}

const viewsReadDesc = `List the saved views: named event filters the owner shares across the apps, CLI and agents.

Best for: finding the view to pass to events_search view=, or checking what a view keeps and whether it bundles pushes.
Returns: each view's id, name, filter (project, source, kind, host, tag, level, q, routine) and bundle (window, max_visible).
Not for: the events in a view; use events_search view=<name>.`

func (t *tools) addViewsRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "views_read", Description: viewsReadDesc, Annotations: readOnly("Read saved views")},
		func(is *jsonschema.Schema) { maxLen(is.Properties["name"], 64) },
		map[string]*jsonschema.Schema{
			"views": arr("By position, then name.", obj("", map[string]*jsonschema.Schema{
				"id": str(""), "name": str(""), "filter": anyObj(""), "bundle": anyObj("Absent when the view does not bundle."),
				"position": integer(""), "icon": str(""),
			})),
		},
		t.viewsRead)
}

func viewOut(v api.View) map[string]any {
	o := map[string]any{"id": v.Id, "name": v.Name, "filter": v.Filter, "position": v.Position}
	if v.Bundle != nil {
		o["bundle"] = v.Bundle
	}
	if v.Icon != nil {
		o["icon"] = *v.Icon
	}
	return o
}

func viewLine(v api.View) string {
	line := v.Name + " (" + v.Id + "): " + viewFilterText(v.Filter)
	if b := v.Bundle; b != nil {
		n := 1
		if b.MaxVisible != nil {
			n = *b.MaxVisible
		}
		line += fmt.Sprintf("; bundles pushes after %d in %s", n, b.Window)
	}
	return line
}

func viewFilterText(f api.ViewFilter) string {
	var parts []string
	add := func(k string, v *string) {
		if v != nil && *v != "" {
			parts = append(parts, k+"="+*v)
		}
	}
	if f.Project != nil {
		parts = append(parts, "project="+strings.Join(*f.Project, ","))
	}
	add("source", f.Source)
	if f.Kind != nil {
		parts = append(parts, "kind="+string(*f.Kind))
	}
	add("host", f.Host)
	add("tag", f.Tag)
	if f.Level != nil {
		var ls []string
		for _, l := range *f.Level {
			ls = append(ls, string(l))
		}
		parts = append(parts, "level="+strings.Join(ls, ","))
	}
	add("q", f.Q)
	if f.Routine != nil {
		parts = append(parts, "routine="+string(*f.Routine))
	}
	if len(parts) == 0 {
		return "every event"
	}
	return strings.Join(parts, " ")
}

func (t *tools) viewsRead(ctx context.Context, _ *mcp.CallToolRequest, in viewsReadIn) (result, error) {
	var list []api.View
	if in.Name != "" {
		v, err := t.API.View(ctx, in.Name)
		if err != nil {
			return result{}, t.apiErr(err, call{what: "view " + in.Name, lister: "views_read without name"})
		}
		list = []api.View{v}
	} else {
		all, err := t.API.Views(ctx)
		if err != nil {
			return result{}, t.apiErr(err, call{})
		}
		list = all
	}
	out := make([]map[string]any, 0, len(list))
	lines := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, viewOut(v))
		lines = append(lines, viewLine(v))
	}
	if len(lines) == 0 {
		lines = append(lines, "no saved views")
	}
	return result{out: map[string]any{"views": out}, text: strings.Join(lines, "\n")}, nil
}

// ---- views_write --------------------------------------------------------

type viewFilterIn struct {
	Project []string `json:"project,omitempty" jsonschema:"Events of any of these projects. - means no project."`
	Source  string   `json:"source,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Host    string   `json:"host,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	Level   []string `json:"level,omitempty" jsonschema:"Events at any of these levels."`
	Q       string   `json:"q,omitempty" jsonschema:"Full-text search over title, summary and body."`
	Routine string   `json:"routine,omitempty" jsonschema:"exclude (default) leaves routine events out; only keeps only them; include keeps both."`
}

type viewBundleIn struct {
	Window     string `json:"window" jsonschema:"A Go duration from 1m to 24h, such as 15m."`
	MaxVisible int    `json:"max_visible,omitempty" jsonschema:"How many pushes in a window go out on their own. Default 1."`
}

type viewsWriteIn struct {
	Operation string        `json:"operation" jsonschema:"save makes the view, or replaces the one with this name; delete removes it."`
	Name      string        `json:"name" jsonschema:"The view's name, or its id for delete."`
	Filter    *viewFilterIn `json:"filter,omitempty" jsonschema:"save: the events the view keeps. Fields combine with AND."`
	Bundle    *viewBundleIn `json:"bundle,omitempty" jsonschema:"save: fold a burst of the view's active pushes into one notification. Omit for none."`
	Icon      string        `json:"icon,omitempty" jsonschema:"save: an SF Symbol name the apps show."`
	Position  int           `json:"position,omitempty" jsonschema:"save: sort order in the apps, low first."`
}

const viewsWriteDesc = `Save or delete a saved view, which every app, the CLI and other agents then see.

Best for: the owner asks for a view such as "backups on the homelab", or to group a noisy source's pushes into one notification (bundle).
save replaces every field of a view with the same name. A bundle folds pushes at level active only: in the window, the first max_visible push on their own and the rest replace one "N new in <view>" notification. Time-sensitive and critical always push.
Returns: the view after the change.
Not for: muting; use mute. Not for: reading events; use events_search view=.`

func (t *tools) addViewsWrite(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "views_write", Description: viewsWriteDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Save or delete a view", IdempotentHint: true,
			DestructiveHint: ptr(true), OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			p := is.Properties
			enum(p["operation"], "save", "delete")
			maxLen(p["name"], 64)
			p["name"].MinLength = ptr(1)
			maxLen(p["icon"], 64)
			between(p["position"], 0, 10000)
			if f := p["filter"]; f != nil {
				f.Types, f.Type = nil, "object"
				fp := f.Properties
				enum(fp["kind"], "message", "digest", "incident", "progress", "checkin", "status")
				enum(fp["routine"], "exclude", "include", "only")
				fp["level"].Items = &jsonschema.Schema{Type: "string", Enum: []any{"passive", "active", "time-sensitive", "critical"}}
				strList(fp["project"], 64, 32)
			}
			if b := p["bundle"]; b != nil {
				b.Types, b.Type = nil, "object"
				between(b.Properties["max_visible"], 1, 20)
			}
		},
		map[string]*jsonschema.Schema{
			"view":    anyObj("save: the view."),
			"deleted": boolean("delete: true when the view was removed."),
		},
		t.viewsWrite)
}

func viewInput(in viewsWriteIn) api.ViewInput {
	vi := api.ViewInput{Name: in.Name, Filter: &api.ViewFilter{}}
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	if in.Icon != "" {
		vi.Icon = &in.Icon
	}
	if in.Position != 0 {
		vi.Position = &in.Position
	}
	routine := "exclude"
	if f := in.Filter; f != nil {
		vi.Filter.Source, vi.Filter.Host, vi.Filter.Tag, vi.Filter.Q = opt(f.Source), opt(f.Host), opt(f.Tag), opt(f.Q)
		if f.Kind != "" {
			k := api.Kind(f.Kind)
			vi.Filter.Kind = &k
		}
		if len(f.Project) > 0 {
			vi.Filter.Project = &f.Project
		}
		if len(f.Level) > 0 {
			ls := make([]api.Level, len(f.Level))
			for i, l := range f.Level {
				ls[i] = api.Level(l)
			}
			vi.Filter.Level = &ls
		}
		if f.Routine != "" {
			routine = f.Routine
		}
	}
	if routine != "include" {
		r := api.ViewFilterRoutine(routine)
		vi.Filter.Routine = &r
	}
	if b := in.Bundle; b != nil {
		vi.Bundle = &api.ViewBundle{Window: b.Window}
		if b.MaxVisible > 0 {
			vi.Bundle.MaxVisible = &b.MaxVisible
		}
	}
	return vi
}

func (t *tools) viewsWrite(ctx context.Context, _ *mcp.CallToolRequest, in viewsWriteIn) (result, error) {
	if in.Name == "" {
		return result{}, toolErr("invalid", "give name", "views_read lists the views")
	}
	switch in.Operation {
	case "delete":
		if err := t.API.DeleteView(ctx, in.Name); err != nil {
			return result{}, t.apiErr(err, call{write: true, what: "view " + in.Name, lister: "views_read"})
		}
		return result{out: map[string]any{"deleted": true}, text: "deleted view " + in.Name}, nil
	case "save":
	default:
		return result{}, toolErr("invalid", "operation must be save or delete", "")
	}
	vi := viewInput(in)
	verb := "saved"
	v, err := t.API.View(ctx, in.Name)
	switch {
	case err == nil:
		verb = "replaced"
		v, err = t.API.PutView(ctx, v.Id, vi)
	case client.IsStatus(err, http.StatusNotFound):
		v, err = t.API.SaveView(ctx, vi)
	}
	if client.IsStatus(err, http.StatusNotFound) || client.IsStatus(err, http.StatusMethodNotAllowed) {
		return result{}, toolErr("server_too_old", "this heraldo server has no saved views", "")
	}
	if err != nil {
		return result{}, t.apiErr(err, call{write: true})
	}
	return result{out: map[string]any{"view": viewOut(v)}, text: verb + " view " + viewLine(v)}, nil
}
