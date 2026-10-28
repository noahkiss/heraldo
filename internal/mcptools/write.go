package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- incident_act -------------------------------------------------------

type incidentIn struct {
	Key    string `json:"key" jsonschema:"The incident's key, from overview."`
	Action string `json:"action"`
	Note   string `json:"note,omitempty" jsonschema:"Stored as the event body. Say who acted and why."`
}

const incidentDesc = `Acknowledge or resolve an open heraldo incident by its key.

Best for: the owner asks to ack or close an incident, or you fixed what it reports.
ack stops reminders and re-alerts; the incident stays open until resolved. resolve closes it and replaces its phone notification with "Resolved after N min".
Returns: the incident after the action, the event that was posted, and changed=false when it was already in that state.
Not for: check-in incidents (key checkin:<slug>) you want gone: they resolve on the check's next success; to silence one, ack it or pause the check with checkin_update.`

func (t *tools) addIncidentAct(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "incident_act", Description: incidentDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Ack or resolve incident", IdempotentHint: true,
			DestructiveHint: &noAnnot, OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			maxLen(is.Properties["key"], 255)
			is.Properties["key"].MinLength = ptr(1)
			enum(is.Properties["action"], "ack", "resolve")
			maxLen(is.Properties["note"], 4096)
		},
		map[string]*jsonschema.Schema{
			"changed": boolean("false: the incident was already in that state; nothing was posted."),
			"incident": obj("", map[string]*jsonschema.Schema{
				"key": str(""), "state": str(""), "opened_at": str(""), "resolved_at": str(""),
			}),
			"event_id": str("The posted event, when changed."),
		},
		t.incidentAct)
}

func incidentOut(key string, e client.Event) map[string]any {
	m := map[string]any{"key": key, "state": incidentState(e)}
	if e.Incident != nil {
		m["state"] = string(e.Incident.State)
		m["opened_at"] = rfc(e.Incident.OpenedAt)
		if r := rfcp(e.Incident.ResolvedAt); r != "" {
			m["resolved_at"] = r
		}
	}
	return m
}

// dedupID is a dedup id of at most 128 bytes.
func dedupID(parts ...string) string {
	s := "mcp:" + strings.Join(parts, ":")
	if len(s) <= 128 {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return "mcp:" + parts[0] + ":" + hex.EncodeToString(h[:16])
}

func (t *tools) incidentAct(ctx context.Context, req *mcp.CallToolRequest, in incidentIn) (result, error) {
	if in.Action != "ack" && in.Action != "resolve" {
		return result{}, toolErr("invalid", "action must be ack or resolve", "")
	}
	st, err := t.API.StateIn(ctx, nil)
	if err != nil {
		return result{}, t.apiErr(err, call{})
	}
	var open *client.Event
	for i, e := range st.Incidents {
		if deref(e.Key) == in.Key {
			open = &st.Incidents[i]
		}
	}
	if open == nil {
		// A resolve that already went through is not an error: the call
		// is idempotent.
		if in.Action == "resolve" {
			p, err := t.API.List(ctx, client.Query{Kind: "incident", Key: in.Key, Desc: true, Limit: 1})
			if err == nil && len(p.Events) == 1 && p.Events[0].Incident != nil &&
				p.Events[0].Incident.State == "resolved" {
				last := p.Events[0]
				return result{out: map[string]any{"changed": false, "incident": incidentOut(in.Key, last)},
					text: fmt.Sprintf("%s is already resolved (event %s)", in.Key, last.Id)}, nil
			}
		}
		return result{}, toolErr("no_open_incident", fmt.Sprintf("no open incident has key %q", in.Key),
			"overview lists the open incidents")
	}
	if in.Action == "ack" && incidentState(*open) == "acked" {
		return result{out: map[string]any{"changed": false, "incident": incidentOut(in.Key, *open)},
			text: in.Key + " is already acknowledged"}, nil
	}
	verb := map[string]string{"ack": "Acknowledged", "resolve": "Resolved"}[in.Action]
	s := summarize(*open)
	title := verb + ": " + s.headline()
	if len(title) > 256 {
		title = cutRunes(title, 250)
	}
	post := map[string]any{
		"source": t.source(req), "kind": "incident", "key": in.Key, "action": in.Action,
		"level": "passive", "title": title,
	}
	if open.Host != nil {
		post["host"] = *open.Host
	}
	if s.Project != "" {
		post["project"] = s.Project
	}
	if in.Note != "" {
		post["body"] = in.Note
	}
	opened := ""
	if open.Incident != nil {
		opened = fmt.Sprint(open.Incident.OpenedAt.Unix())
	}
	post["dedup"] = dedupID(in.Action, in.Key, opened)
	e, _, err := t.API.PostNew(ctx, post)
	if err != nil {
		return result{}, t.apiErr(err, call{write: true})
	}
	return result{out: map[string]any{"changed": true, "incident": incidentOut(in.Key, e), "event_id": e.Id},
		text: fmt.Sprintf("%s %s (event %s)", strings.ToLower(verb), in.Key, e.Id)}, nil
}

// ---- mute ---------------------------------------------------------------

type muteIn struct {
	Operation string `json:"operation"`
	Source    string `json:"source,omitempty" jsonschema:"create: mute events from this source."`
	Key       string `json:"key,omitempty" jsonschema:"create: mute events with this key. checkin:<slug> mutes one check both ways."`
	For       string `json:"for,omitempty" jsonschema:"create: Go duration, 1m to 8784h. Default 1h."`
	MuteID    string `json:"mute_id,omitempty" jsonschema:"end: the id from overview."`
}

const muteDesc = `Mute a heraldo source or key for a while, or end a mute early.

Best for: the owner asks to silence a noisy source, one incident, or one check for a time.
A mute holds back pushes and re-alerts only. Events are still stored and listed.
create needs source, key or both (both: an event must match both). To mute one check both ways, use key checkin:<slug>.
Returns: the mute, and created=false when a mute in force already covered it. overview lists the mutes in force.`

func (t *tools) addMute(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "mute", Description: muteDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Mute", DestructiveHint: &noAnnot, OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			enum(is.Properties["operation"], "create", "end")
			maxLen(is.Properties["source"], 128)
			maxLen(is.Properties["key"], 255)
			maxLen(is.Properties["for"], 32)
			def(is.Properties["for"], "1h")
			maxLen(is.Properties["mute_id"], 64)
		},
		map[string]*jsonschema.Schema{
			"created": boolean("create: false when a mute in force already covered this."),
			"mute": obj("", map[string]*jsonschema.Schema{
				"id": str(""), "source": str(""), "key": str(""), "until": str(""), "active": boolean(""), "ended_at": str(""),
			}),
		},
		t.mute)
}

func muteOut(m api.Mute) map[string]any {
	o := map[string]any{"id": m.Id, "until": rfc(m.Until), "active": m.Active}
	if m.Source != nil {
		o["source"] = *m.Source
	}
	if m.Key != nil {
		o["key"] = *m.Key
	}
	if m.EndedAt != nil {
		o["ended_at"] = rfc(*m.EndedAt)
	}
	return o
}

func muteText(verb string, m api.Mute) string {
	s := verb + " " + m.Id
	if m.Source != nil {
		s += " source=" + *m.Source
	}
	if m.Key != nil {
		s += " key=" + *m.Key
	}
	return s + " until " + rfc(m.Until)
}

func (t *tools) mute(ctx context.Context, _ *mcp.CallToolRequest, in muteIn) (result, error) {
	w := call{write: true}
	switch in.Operation {
	case "end":
		if in.MuteID == "" {
			return result{}, toolErr("invalid", "end needs mute_id", "overview lists the mutes in force with their ids")
		}
		m, err := t.API.EndMute(ctx, in.MuteID)
		if err != nil {
			return result{}, t.apiErr(err, call{write: true, what: "mute " + in.MuteID, lister: "overview"})
		}
		return result{out: map[string]any{"mute": muteOut(m)}, text: muteText("ended", m)}, nil
	case "create":
	default:
		return result{}, toolErr("invalid", "operation must be create or end", "")
	}
	if in.Source == "" && in.Key == "" {
		return result{}, toolErr("invalid", "create needs source, key or both", "")
	}
	f := in.For
	if f == "" {
		f = "1h"
	}
	d, err := time.ParseDuration(f)
	if err != nil || d < time.Minute || d > 8784*time.Hour {
		return result{}, toolErr("invalid", fmt.Sprintf("for: %q is not a Go duration from 1m to 8784h", f), "use a form such as 30m, 2h or 72h")
	}
	// A retry must not stack mutes: a mute in force on the same source and
	// key that lasts at least as long answers instead.
	mutes, err := t.API.Mutes(ctx)
	if err != nil {
		return result{}, t.apiErr(err, w)
	}
	end := time.Now().Add(d)
	for _, m := range mutes {
		if m.Active && deref(m.Source) == in.Source && deref(m.Key) == in.Key && !m.Until.Before(end.Add(-time.Minute)) {
			return result{out: map[string]any{"created": false, "mute": muteOut(m)}, text: muteText("already muted:", m)}, nil
		}
	}
	mi := api.MuteInput{For: &f}
	if in.Source != "" {
		mi.Source = &in.Source
	}
	if in.Key != "" {
		mi.Key = &in.Key
	}
	m, err := t.API.Mute(ctx, mi)
	if err != nil {
		return result{}, t.apiErr(err, w)
	}
	return result{out: map[string]any{"created": true, "mute": muteOut(m)}, text: muteText("muted", m)}, nil
}

// ---- send ---------------------------------------------------------------

type sendIn struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary,omitempty" jsonschema:"The one-line push text."`
	Body    string   `json:"body,omitempty" jsonschema:"Markdown. Stored in full; the push carries the summary or the start."`
	Level   string   `json:"level,omitempty"`
	Key     string   `json:"key,omitempty" jsonschema:"Groups related messages, such as one job's."`
	URL     string   `json:"url,omitempty" jsonschema:"Opened when the notification is tapped."`
	Tags    []string `json:"tags,omitempty"`
	Project string   `json:"project,omitempty" jsonschema:"The project the message is about. Default: HERALDO_PROJECT, else none."`
	Dedup   string   `json:"dedup,omitempty" jsonschema:"Idempotency id. A repeat within 24 hours stores nothing and returns the first event."`
}

const sendDesc = `Send the owner a plain message through heraldo. It is stored and, unless passive, pushed to their phone and Mac.

Best for: telling the owner that a long task finished or failed, or that it needs them.
Level: passive is inbox-only; active (default) is a normal notification; time-sensitive breaks through Focus, use it only when the owner must act now.
Returns: the stored event id and cursor, and duplicate=true when dedup matched an earlier send.
Query tips: set dedup to make retries safe, such as <task>-<step>.
Not for: incidents (incident_act acks and resolves them; agents do not open them), check-in pings (the job pings itself), Live Activities.`

func (t *tools) addSend(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "send", Description: sendDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Send message", DestructiveHint: &noAnnot, OpenWorldHint: ptr(true)}},
		func(is *jsonschema.Schema) {
			p := is.Properties
			p["title"].MinLength = ptr(1)
			for k, n := range map[string]int{"title": 256, "summary": 178, "body": 65536, "key": 255, "url": 2048, "project": 128, "dedup": 128} {
				maxLen(p[k], n)
			}
			enum(p["level"], "passive", "active", "time-sensitive")
			def(p["level"], "active")
			p["url"].Format = "uri"
			strList(p["tags"], 64, 16)
		},
		map[string]*jsonschema.Schema{
			"id": str("The stored event id."), "cursor": str(""), "duplicate": boolean(""),
			"pushed": str("will push, or passive (inbox only). A hint: delivery is async."),
			"source": str("The source the event was posted as."),
		},
		t.send)
}

func (t *tools) send(ctx context.Context, req *mcp.CallToolRequest, in sendIn) (result, error) {
	if strings.TrimSpace(in.Title) == "" {
		return result{}, toolErr("invalid", "title is required", "")
	}
	level := in.Level
	if level == "" {
		level = "active"
	}
	src := t.source(req)
	post := map[string]any{"source": src, "kind": "message", "title": in.Title, "level": level}
	for k, v := range map[string]string{"summary": in.Summary, "body": in.Body, "key": in.Key, "url": in.URL, "dedup": in.Dedup, "host": t.Host} {
		if v != "" {
			post[k] = v
		}
	}
	if len(in.Tags) > 0 {
		post["tags"] = in.Tags
	}
	project := in.Project
	if project == "" {
		project = t.Project
	}
	if project != "" {
		post["project"] = project
	}
	e, created, err := t.API.PostNew(ctx, post)
	if err != nil {
		var ce *client.Error
		// A server from before the project field refuses it as an unknown
		// field. Any other complaint about project is a bad name, and the
		// server's message says so.
		if project != "" && asStatus(err, &ce, http.StatusBadRequest) && strings.Contains(ce.Message, "/project: not allowed here") {
			return result{}, toolErr("unsupported_filter", "this heraldo server has no project field yet: it refused project",
				"send again without project, or upgrade the server")
		}
		return result{}, t.apiErr(err, call{write: true})
	}
	if project != "" {
		if _, ok := rawProject(e.Raw); !ok {
			return result{}, toolErr("unsupported_filter",
				"this heraldo server stored the message but dropped project "+e.Id,
				"do not send it again; leave project out until the server is upgraded")
		}
	}
	dup := !created
	pushed := "will push"
	if level == "passive" {
		pushed = "passive"
	}
	out := map[string]any{"id": e.Id, "cursor": deref(e.Cursor), "duplicate": dup, "pushed": pushed, "source": src}
	text := fmt.Sprintf("sent %s as %s (%s)", e.Id, src, pushed)
	if dup {
		text = fmt.Sprintf("duplicate: dedup %q matched event %s; nothing new was stored", in.Dedup, e.Id)
	}
	return result{out: out, text: text}, nil
}

func asStatus(err error, ce **client.Error, status int) bool {
	if !client.IsStatus(err, status) {
		return false
	}
	return errors.As(err, ce)
}
