package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// sectionCap caps each overview list.
const sectionCap = 20

// allSections lists overview's sections; the first five are the default.
var allSections = []string{"unread", "incidents", "checkins", "activities", "mutes", "statuses", "projects", "devices"}

type overviewIn struct {
	Project  []string `json:"project,omitempty" jsonschema:"Only incidents, checks and statuses of these projects."`
	Sections []string `json:"sections,omitempty" jsonschema:"Default: unread, incidents, checkins, activities, mutes. Ask for statuses, projects or devices by name."`
}

const overviewDesc = `Show what needs attention in heraldo now: unread count, open incidents, checks that are down or late, running Live Activities, mutes in force.

Best for: the first call in a session, and "is anything wrong / what happened".
Returns: counts plus short lists (at most 20 per section, most urgent first). No event bodies.
Ask for more by name in sections: statuses (set status widgets), projects (the project names events use, with counts; use them as project filters), devices (the owner's registered phones and Macs, and whether each gets pushes).
Not for: history or search; use events_search.
Follow up with event_read for an incident's full text, checkin_read for a check, activity_read for an activity, events_search with key=<key> for an incident's history.`

func (t *tools) addOverview(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "overview", Description: overviewDesc, Annotations: readOnly("Overview")},
		func(is *jsonschema.Schema) {
			strList(is.Properties["project"], 128, 16)
			sc := is.Properties["sections"]
			sc.Types, sc.Type = nil, "array"
			enum(sc.Items, allSections...)
		},
		map[string]*jsonschema.Schema{
			"server": obj("", map[string]*jsonschema.Schema{
				"url_host": str(""), "version": str(""), "push": str(""), "write": boolean("Write tools are registered."),
				"write_off": str("Why write is false."), "source": str("The source of events this server posts."),
			}),
			"unread": obj("", map[string]*jsonschema.Schema{"cursor_name": str(""), "count": integer(""), "cursor": str("")}),
			"incidents": obj("Not resolved: open before acked, oldest first.", map[string]*jsonschema.Schema{
				"total": integer(""), "items": arr("", anyObj("key, state, opened_at, level, source, title, project, event_id")),
			}),
			"checkins": obj("items: only down, late or overdue checks.", map[string]*jsonschema.Schema{
				"total": integer(""), "by_state": anyObj("Counts per state."), "items": arr("", anyObj("slug, state, down_reason, state_since, next_due, incident")),
			}),
			"activities": obj("", map[string]*jsonschema.Schema{
				"running": integer(""), "items": arr("", anyObj("key, title, status, progress, updated_at, devices")),
			}),
			"mutes":    obj("", map[string]*jsonschema.Schema{"items": arr("", anyObj("id, source, key, project, until"))}),
			"statuses": obj("", map[string]*jsonschema.Schema{"total": integer(""), "items": arr("", eventSummarySchema)}),
			"projects": obj("Newest first.", map[string]*jsonschema.Schema{
				"total": integer(""), "unfiled": integer("Events with no project."),
				"items": arr("", anyObj("name, events, last_at")),
			}),
			"devices": obj("Active first.", map[string]*jsonschema.Schema{
				"total": integer(""), "active": integer(""),
				"items": arr("", anyObj("id, name, platform, environment, active, token_suffix, permissions, retired_reason, updated_at")),
			}),
			"notes": arr("", str("")),
		},
		t.overview)
}

type incidentItem struct {
	Key      string `json:"key"`
	State    string `json:"state"`
	OpenedAt string `json:"opened_at,omitempty"`
	Level    string `json:"level,omitempty"`
	Source   string `json:"source"`
	Title    string `json:"title,omitempty"`
	Project  string `json:"project,omitempty"`
	EventID  string `json:"event_id"`

	opened time.Time
}

type checkItem struct {
	Slug       string `json:"slug"`
	State      string `json:"state"`
	DownReason string `json:"down_reason,omitempty"`
	StateSince string `json:"state_since"`
	NextDue    string `json:"next_due,omitempty"`
	Incident   string `json:"incident,omitempty"`
}

type activityItem struct {
	Key       string   `json:"key"`
	Title     string   `json:"title,omitempty"`
	Status    string   `json:"status,omitempty"`
	Progress  *float32 `json:"progress,omitempty"`
	UpdatedAt string   `json:"updated_at"`
	Devices   int      `json:"devices"`
}

type muteItem struct {
	ID      string `json:"id"`
	Source  string `json:"source,omitempty"`
	Key     string `json:"key,omitempty"`
	Project string `json:"project,omitempty"`
	Until   string `json:"until"`
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func rfcp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return rfc(*t)
}

func (t *tools) overview(ctx context.Context, req *mcp.CallToolRequest, in overviewIn) (result, error) {
	if t.API == nil {
		return result{}, toolErr("not_configured", t.NotConfigured, t.ConfigHint)
	}
	want := map[string]bool{}
	for _, s := range in.Sections {
		want[s] = true
	}
	if len(in.Sections) == 0 {
		for _, s := range allSections[:5] {
			want[s] = true
		}
	}
	ps := newProjectSet(in.Project)
	out := map[string]any{}
	var notes []string
	srv := map[string]any{"url_host": t.URLHost, "write": t.Write}
	if !t.Write {
		srv["write_off"] = t.WriteOff
	} else {
		srv["source"] = t.source(req)
	}
	out["server"] = srv

	var (
		wg      sync.WaitGroup
		health  api.Health
		herr    error
		cs      client.CursorState
		cfound  bool
		cerr    error
		state   client.State
		serr    error
		checks  []client.CheckEntry
		ckerr   error
		acts    []api.Activity
		aerr    error
		mutes   []api.Mute
		merr    error
		projs   api.ProjectList
		perr    error
		devs    []api.Device
		derr    error
		needSt  = want["incidents"] || want["statuses"]
		goFetch = func(on bool, f func()) {
			if on {
				wg.Add(1)
				go func() { defer wg.Done(); f() }()
			}
		}
	)
	goFetch(true, func() { health, herr = t.API.Health(ctx) })
	goFetch(want["unread"], func() { cs, cfound, cerr = t.API.CursorUnread(ctx, t.Cursor) })
	goFetch(needSt, func() { state, serr = t.API.StateIn(ctx, in.Project) })
	goFetch(want["checkins"], func() { checks, ckerr = t.API.ChecksIn(ctx, in.Project) })
	goFetch(want["activities"], func() { acts, aerr = t.API.Activities(ctx, false) })
	goFetch(want["mutes"] && t.Write, func() { mutes, merr = t.API.Mutes(ctx) })
	goFetch(want["projects"], func() { projs, perr = t.API.Projects(ctx) })
	goFetch(want["devices"], func() { devs, derr = t.API.Devices(ctx) })
	wg.Wait()

	// A failure every call shares (a bad token, a server that is down) is
	// the answer; a failure of one section is a note.
	for _, err := range []error{serr, cerr, ckerr} {
		if err != nil {
			if te := t.apiErr(err, call{}); te.Code == "unauthorized" || te.Code == "unreachable" {
				return result{}, te
			}
		}
	}
	section := func(name string, err error) bool {
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s not shown: %s", name, t.apiErr(err, call{}).Error()))
			return false
		}
		return true
	}
	var lines []string
	if herr == nil {
		srv["version"] = health.Version
		if health.Push != nil {
			srv["push"] = string(*health.Push)
		}
	}
	if want["unread"] && section("unread", cerr) {
		if cfound {
			out["unread"] = map[string]any{"cursor_name": t.Cursor, "count": cs.Unread, "cursor": cs.Cursor}
			lines = append(lines, fmt.Sprintf("unread: %d (cursor %s)", cs.Unread, t.Cursor))
		} else {
			out["unread"] = map[string]any{"cursor_name": t.Cursor, "count": 0}
			notes = append(notes, fmt.Sprintf("the read cursor %q does not exist yet", t.Cursor))
		}
		if len(ps) > 0 {
			notes = append(notes, "unread counts every project")
		}
	}
	if needSt && section("incidents and statuses", serr) {
		var raws []json.RawMessage
		for _, e := range slices.Concat(state.Incidents, state.Statuses) {
			raws = append(raws, e.Raw)
		}
		if ps.ignored(raws) {
			return result{}, unsupportedFilter()
		}
	}
	if want["incidents"] && serr == nil {
		var items []incidentItem
		for _, e := range state.Incidents {
			s := summarize(e)
			it := incidentItem{Key: s.Key, State: incidentState(e), Level: s.Level, Source: s.Source,
				Title: s.headline(), Project: s.Project, EventID: e.Id, opened: e.ReceivedAt}
			if e.Incident != nil {
				it.opened = e.Incident.OpenedAt
				it.OpenedAt = rfc(it.opened)
			}
			items = append(items, it)
		}
		sort.SliceStable(items, func(i, j int) bool {
			if (items[i].State == "open") != (items[j].State == "open") {
				return items[i].State == "open"
			}
			return items[i].opened.Before(items[j].opened)
		})
		total := len(items)
		items = items[:min(total, sectionCap)]
		out["incidents"] = map[string]any{"total": total, "items": nonNil(items)}
		lines = append(lines, fmt.Sprintf("incidents: %d not resolved", total))
		for _, it := range items {
			since := ""
			if it.OpenedAt != "" {
				since = " since " + it.OpenedAt
			}
			lines = append(lines, fmt.Sprintf("  %s [%s]%s, %s: %s (event %s)", it.Key, it.State, since, it.Source, it.Title, it.EventID))
		}
	}
	if want["statuses"] && serr == nil {
		var items []EventSummary
		for _, e := range state.Statuses {
			items = append(items, summarize(e))
		}
		total := len(items)
		items = items[:min(total, sectionCap)]
		out["statuses"] = map[string]any{"total": total, "items": nonNil(items)}
		lines = append(lines, fmt.Sprintf("statuses: %d set", total))
		for _, s := range items {
			lines = append(lines, "  "+s.line())
		}
	}
	if want["checkins"] && section("checkins", ckerr) {
		raws := make([]json.RawMessage, len(checks))
		for i, c := range checks {
			raws[i] = c.Raw
		}
		if ps.ignored(raws) {
			return result{}, unsupportedFilter()
		}
		byState := map[string]int{}
		var items []checkItem
		now := time.Now()
		for _, c := range checks {
			byState[string(c.State)]++
			bad := c.State == "down" || c.State == "late" ||
				(c.State == "started" && c.DownAfter != nil && now.After(*c.DownAfter))
			if !bad {
				continue
			}
			it := checkItem{Slug: c.Key, State: string(c.State), StateSince: rfc(c.StateSince), NextDue: rfcp(c.NextDue)}
			if c.DownReason != nil {
				it.DownReason = string(*c.DownReason)
			}
			if c.Incident != nil {
				it.Incident = string(*c.Incident)
			}
			items = append(items, it)
		}
		sort.SliceStable(items, func(i, j int) bool {
			if (items[i].State == "down") != (items[j].State == "down") {
				return items[i].State == "down"
			}
			return items[i].StateSince < items[j].StateSince
		})
		n := len(items)
		items = items[:min(n, sectionCap)]
		out["checkins"] = map[string]any{"total": len(checks), "by_state": byState, "items": nonNil(items)}
		lines = append(lines, fmt.Sprintf("checkins: %d total, %d need attention %s", len(checks), n, countLine(byState)))
		for _, it := range items {
			l := fmt.Sprintf("  %s [%s] since %s", it.Slug, it.State, it.StateSince)
			if it.DownReason != "" {
				l += " (" + it.DownReason + ")"
			}
			lines = append(lines, l)
		}
	}
	if want["activities"] && section("activities", aerr) {
		var items []activityItem
		for _, a := range acts {
			it := activityItem{Key: a.Key, UpdatedAt: rfc(a.UpdatedAt), Devices: len(a.Instances)}
			it.Title, _ = a.Content.Title.Get()
			it.Status, _ = a.Content.Status.Get()
			if p, err := a.Content.Progress.Get(); err == nil {
				it.Progress = &p
			}
			items = append(items, it)
		}
		total := len(items)
		items = items[:min(total, sectionCap)]
		out["activities"] = map[string]any{"running": total, "items": nonNil(items)}
		lines = append(lines, fmt.Sprintf("activities: %d running", total))
		for _, a := range acts[:len(items)] {
			lines = append(lines, fmt.Sprintf("  %s: %s", a.Key, liveLine(a.Content)))
		}
		if len(ps) > 0 && total > 0 {
			notes = append(notes, "activities are not filtered by project")
		}
	}
	if want["mutes"] {
		if !t.Write {
			notes = append(notes, "mutes not shown: listing them needs a write token")
		} else if section("mutes", merr) {
			var items []muteItem
			for _, m := range mutes {
				items = append(items, muteItem{ID: m.Id, Source: deref(m.Source), Key: deref(m.Key), Project: deref(m.Project), Until: rfc(m.Until)})
			}
			items = items[:min(len(items), sectionCap)]
			out["mutes"] = map[string]any{"items": nonNil(items)}
			lines = append(lines, fmt.Sprintf("mutes: %d in force", len(mutes)))
			for _, m := range items {
				line := fmt.Sprintf("  %s source=%s key=%s", m.ID, m.Source, m.Key)
				if m.Project != "" {
					line += " project=" + m.Project
				}
				lines = append(lines, line+" until "+m.Until)
			}
		}
	}
	if want["projects"] && section("projects", perr) {
		pl := projs.Projects
		sort.SliceStable(pl, func(i, j int) bool { return pl[i].LastAt.After(pl[j].LastAt) })
		items := make([]map[string]any, 0, min(len(pl), sectionCap))
		lines = append(lines, fmt.Sprintf("projects: %d, plus %d events with none", len(pl), projs.Unfiled))
		for _, p := range pl[:min(len(pl), sectionCap)] {
			items = append(items, map[string]any{"name": p.Name, "events": p.Events, "last_at": rfc(p.LastAt)})
			lines = append(lines, fmt.Sprintf("  %s: %d events, last %s", p.Name, p.Events, rfc(p.LastAt)))
		}
		out["projects"] = map[string]any{"total": len(pl), "unfiled": projs.Unfiled, "items": items}
	}
	if want["devices"] && section("devices", derr) {
		sort.SliceStable(devs, func(i, j int) bool { return devs[i].Active && !devs[j].Active })
		active := 0
		for _, d := range devs {
			if d.Active {
				active++
			}
		}
		lines = append(lines, fmt.Sprintf("devices: %d, %d active", len(devs), active))
		items := make([]map[string]any, 0, min(len(devs), sectionCap))
		for _, d := range devs[:min(len(devs), sectionCap)] {
			it := map[string]any{"id": d.Id, "environment": string(d.Environment), "active": d.Active,
				"token_suffix": d.TokenSuffix, "permissions": d.Permissions, "updated_at": rfc(d.UpdatedAt)}
			for k, v := range map[string]*string{"name": d.Name, "platform": d.Platform, "retired_reason": d.RetiredReason} {
				if v != nil {
					it[k] = *v
				}
			}
			items = append(items, it)
			state := "active"
			if !d.Active {
				state = "retired: " + deref(d.RetiredReason)
			}
			lines = append(lines, fmt.Sprintf("  %s %s (%s) %s", d.Id, deref(d.Name), deref(d.Platform), state))
		}
		out["devices"] = map[string]any{"total": len(devs), "active": active, "items": items}
	}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	head := fmt.Sprintf("heraldo %s", t.URLHost)
	if v, ok := srv["version"].(string); ok && v != "" {
		head += " " + v
	}
	if t.Write {
		head += ", write on"
	} else {
		head += ", read-only (" + t.WriteOff + ")"
	}
	lines = append([]string{head}, lines...)
	for _, n := range notes {
		lines = append(lines, "note: "+n)
	}
	return result{out: out, text: strings.Join(lines, "\n")}, nil
}

// incidentState is the state of the incident an event in /v1/state stands
// for. An event stored before incident snapshots has none: its action says.
func incidentState(e client.Event) string {
	if e.Incident != nil {
		return string(e.Incident.State)
	}
	if e.Action != nil && *e.Action == "ack" {
		return "acked"
	}
	return "open"
}

func countLine(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// nonNil keeps an empty list a JSON array.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
