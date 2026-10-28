package mcptools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/client"
)

type searchIn struct {
	Q       string   `json:"q,omitempty" jsonschema:"Full-text search over title, summary and body. SQLite FTS: words, \"quoted phrases\", prefix*."`
	Kind    string   `json:"kind,omitempty"`
	Source  string   `json:"source,omitempty"`
	Host    string   `json:"host,omitempty"`
	Key     string   `json:"key,omitempty" jsonschema:"One incident's, job's or check's events."`
	Tag     string   `json:"tag,omitempty"`
	Project []string `json:"project,omitempty" jsonschema:"Only events of these projects. - means events with no project."`
	Hold    string   `json:"hold,omitempty" jsonschema:"Only events whose hold.state is this, such as held."`
	Unread  bool     `json:"unread,omitempty" jsonschema:"Events after the shared read cursor, oldest first. Never moves the cursor."`
	Cursor  string   `json:"cursor,omitempty" jsonschema:"next_cursor from an earlier call. Pass it alone: it carries that call's filters and order."`
	Order   string   `json:"order,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

type searchOut struct {
	Events      []EventSummary `json:"events"`
	NextCursor  string         `json:"next_cursor,omitempty"`
	HasMore     bool           `json:"has_more"`
	Order       string         `json:"order"`
	UnreadCount *int           `json:"unread_count,omitempty"`
	Truncated   bool           `json:"truncated,omitempty"`
	Notes       []string       `json:"notes,omitempty"`

	state *pageState
}

// trim drops the last row to fit maxResult. It reports false when one row
// is left.
func (o *searchOut) trim() bool {
	if len(o.Events) <= 1 {
		return false
	}
	o.Events = o.Events[:len(o.Events)-1]
	o.HasMore, o.Truncated = true, true
	o.NextCursor = o.reCursor(o.Events[len(o.Events)-1].Cursor)
	if len(o.Notes) == 0 || !strings.HasPrefix(o.Notes[len(o.Notes)-1], "truncated") {
		o.Notes = append(o.Notes, "truncated to fit 64 KiB: lower limit or narrow the filters")
	}
	return true
}

// pageState is what an MCP cursor carries: the server cursor, the
// direction, and the filters of the first call.
type pageState struct {
	Dir    string       `json:"d"`
	Server string       `json:"c"`
	Query  client.Query `json:"q"`
}

func (o *searchOut) reCursor(server string) string {
	if o.state == nil || server == "" {
		return o.NextCursor
	}
	st := *o.state
	st.Server = server
	return encodeCursor(st)
}

func encodeCursor(st pageState) string {
	st.Query.After, st.Query.Before = "", ""
	b, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (pageState, error) {
	var st pageState
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil || (st.Dir != "newest" && st.Dir != "oldest") {
		return st, toolErr("invalid", "cursor is not a next_cursor from events_search",
			"pass next_cursor exactly as an earlier call returned it, or leave cursor out")
	}
	return st, nil
}

const searchDesc = `List or search heraldo events, with filters and full-text search.

Best for: "what happened with X", unread events, an incident's history (key=), held events (hold=held), events since a cursor.
Returns: up to limit EventSummary rows (default 20, max 100), plus next_cursor and has_more. No bodies.
Query tips: q is full-text over title, summary and body (SQLite FTS: words, "quoted phrases", prefix*). Filters combine with AND. unread=true reads from the shared read cursor and never moves it. To page, pass next_cursor alone: it keeps the filters and order.
Follow up with event_read for the full body of a row.
Not for: open incidents or down checks right now; use overview.`

func (t *tools) addEventsSearch(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "events_search", Description: searchDesc, Annotations: readOnly("Search events")},
		func(is *jsonschema.Schema) {
			p := is.Properties
			enum(p["kind"], "message", "digest", "incident", "progress", "checkin", "status")
			enum(p["order"], "newest", "oldest")
			def(p["order"], "newest")
			p["order"].Description = "Ignored with unread, which lists oldest first."
			between(p["limit"], 1, 100)
			def(p["limit"], 20)
			for k, n := range map[string]int{"q": 256, "source": 128, "host": 128, "key": 255, "tag": 64, "hold": 64, "cursor": 4096} {
				maxLen(p[k], n)
			}
			strList(p["project"], 128, 16)
		},
		map[string]*jsonschema.Schema{
			"events":       arr("Newest first, or oldest first with order=oldest or unread.", eventSummarySchema),
			"next_cursor":  str("Pass as cursor for the next page. With oldest, keep it to poll for new events."),
			"has_more":     boolean(""),
			"order":        str("newest or oldest"),
			"unread_count": integer("With unread: the owner's unread badge count."),
			"truncated":    boolean("Rows were dropped to fit 64 KiB."),
			"notes":        arr("", str("")),
		},
		t.eventsSearch)
}

func (t *tools) eventsSearch(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (result, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	out := &searchOut{Events: []EventSummary{}}
	var st pageState
	switch {
	case in.Cursor != "":
		var err error
		if st, err = decodeCursor(in.Cursor); err != nil {
			return result{}, err
		}
	case in.Unread:
		cs, found, err := t.API.CursorUnread(ctx, t.Cursor)
		if err != nil {
			return result{}, t.apiErr(err, call{})
		}
		st.Dir = "oldest"
		if found {
			st.Server = cs.Cursor
			out.UnreadCount = ptr(cs.Unread)
		} else {
			st.Dir = "newest"
			out.Notes = append(out.Notes, fmt.Sprintf("the read cursor %q does not exist yet; showing the newest events", t.Cursor))
		}
	default:
		st.Dir = "newest"
		if in.Order == "oldest" {
			st.Dir = "oldest"
		}
	}
	if in.Cursor == "" {
		st.Query = client.Query{Q: in.Q, Kind: in.Kind, Source: in.Source, Host: in.Host, Key: in.Key,
			Tag: in.Tag, Hold: in.Hold, Project: in.Project}
	}
	out.state = &st
	q := st.Query
	q.Limit = limit + 1
	if st.Dir == "newest" {
		q.Desc, q.Before = true, st.Server
	} else {
		q.After = st.Server
	}
	page, err := t.API.List(ctx, q)
	if err != nil {
		return result{}, t.apiErr(err, call{})
	}
	ps := newProjectSet(q.Project)
	raws := make([]json.RawMessage, len(page.Events))
	for i, e := range page.Events {
		raws[i] = e.Raw
	}
	if ps.ignored(raws) {
		return result{}, unsupportedFilter()
	}
	evs := page.Events
	if len(evs) > limit {
		evs, out.HasMore = evs[:limit], true
	}
	for _, e := range evs {
		out.Events = append(out.Events, summarize(e))
	}
	out.Order = st.Dir
	switch {
	case len(out.Events) > 0:
		out.NextCursor = out.reCursor(out.Events[len(out.Events)-1].Cursor)
	case st.Dir == "oldest" && st.Server != "":
		// Up to date: keep the cursor to poll with.
		out.NextCursor = encodeCursor(st)
	}
	for size(out) > maxResult && out.trim() {
	}
	return result{out: out, text: out.render()}, nil
}

func (o *searchOut) render() string {
	var b strings.Builder
	if o.UnreadCount != nil {
		fmt.Fprintf(&b, "unread: %d\n", *o.UnreadCount)
	}
	if len(o.Events) == 0 {
		b.WriteString("no events\n")
	}
	for _, e := range o.Events {
		b.WriteString(e.line() + "\n")
	}
	if o.HasMore {
		b.WriteString("more: pass next_cursor\n")
	}
	for _, n := range o.Notes {
		b.WriteString("note: " + n + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- event_read ---------------------------------------------------------

const (
	bodyPage   = 16 << 10
	detailsCap = 4 << 10
	deliveryN  = 10
)

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)

type readIn struct {
	ID         string `json:"id" jsonschema:"The event id, a 26-character ULID."`
	BodyOffset int    `json:"body_offset,omitempty" jsonschema:"Byte offset into the body, from body_page.next_offset."`
}

const readDesc = `Read one heraldo event in full: body, details, delivery outcome per device.

Best for: an id from events_search or overview.
Returns: the stored event. Bodies over 16 KiB come back in pages; pass body_page.next_offset as body_offset to read on. details over 4 KiB is left out with its size.
Not for: finding events; use events_search.`

func (t *tools) addEventRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "event_read", Description: readDesc, Annotations: readOnly("Read event")},
		func(is *jsonschema.Schema) {
			is.Properties["id"].Pattern = ulidRe.String()
			is.Properties["body_offset"].Minimum = ptr(0.0)
		},
		map[string]*jsonschema.Schema{
			"id": str(""), "source": str(""), "kind": str(""), "title": str(""), "body": str("At most 16 KiB of the body."),
			"body_page": obj("Set when the body was cut.", map[string]*jsonschema.Schema{
				"offset": integer(""), "next_offset": integer("Absent on the last page."), "total_bytes": integer(""),
			}),
			"details":  anyObj("The event's details, or {_omitted_bytes: n} over 4 KiB."),
			"delivery": obj("Push outcome per device.", map[string]*jsonschema.Schema{"sent": integer(""), "skipped": integer(""), "failed": integer(""), "items": arr("At most 10.", anyObj(""))}),
		},
		t.eventRead)
}

func (t *tools) eventRead(ctx context.Context, _ *mcp.CallToolRequest, in readIn) (result, error) {
	if !ulidRe.MatchString(in.ID) {
		return result{}, toolErr("invalid", "id must be a 26-character event id", "take it from events_search or overview")
	}
	e, err := t.API.Get(ctx, strings.ToUpper(in.ID))
	if err != nil {
		return result{}, t.apiErr(err, call{what: "event " + in.ID, lister: "events_search"})
	}
	m := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(e.Raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return result{}, err
	}
	var notes []string
	body := ""
	if e.Body != nil {
		body = *e.Body
	}
	if body != "" || in.BodyOffset > 0 {
		off := min(in.BodyOffset, len(body))
		for off < len(body) && !utf8.RuneStart(body[off]) {
			off++
		}
		end := min(off+bodyPage, len(body))
		for end < len(body) && !utf8.RuneStart(body[end]) {
			end--
		}
		m["body"] = body[off:end]
		if off > 0 || end < len(body) {
			pg := map[string]any{"offset": off, "total_bytes": len(body)}
			if end < len(body) {
				pg["next_offset"] = end
				notes = append(notes, fmt.Sprintf("body cut at %d of %d bytes: call again with body_offset=%d", end, len(body), end))
			}
			m["body_page"] = pg
		}
	}
	if d, ok := m["details"]; ok {
		if n := size(d); n > detailsCap {
			m["details"] = map[string]any{"_omitted_bytes": n}
			notes = append(notes, fmt.Sprintf("details left out: %d bytes", n))
		}
	}
	if e.Delivery != nil {
		dl := map[string]any{"sent": 0, "skipped": 0, "failed": 0}
		var items []any
		for i, d := range *e.Delivery {
			k := string(d.Outcome)
			if n, ok := dl[k].(int); ok {
				dl[k] = n + 1
			}
			if i < deliveryN {
				items = append(items, d)
			}
		}
		dl["items"] = items
		m["delivery"] = dl
	}
	if len(notes) > 0 {
		m["notes"] = notes
	}
	s := summarize(e)
	var b strings.Builder
	b.WriteString(s.line() + "\n")
	if body != "" {
		b.WriteString("\n" + m["body"].(string) + "\n")
	}
	for _, n := range notes {
		b.WriteString("\nnote: " + n)
	}
	return result{out: m, text: strings.TrimRight(b.String(), "\n")}, nil
}
