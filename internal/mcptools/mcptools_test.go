package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	readTok  = "mcp-read-token-for-tests"
	writeTok = "mcp-write-token-for-tests"
)

// fixture is a real heraldo server on a temporary database.
type fixture struct {
	t   *testing.T
	url string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf("agent read %s\nagent-w write %s\n",
		auth.Hash(readTok), auth.Hash(writeTok))), '\n')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv := httptest.NewServer(server.New(server.Options{
		Store: st, Tokens: tokens, Validator: v, Done: done, Version: "test",
		Logger: slog.New(slog.DiscardHandler),
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return &fixture{t: t, url: srv.URL}
}

// do sends one API request and returns the decoded answer.
func (f *fixture) do(method, path, tok, body string) map[string]any {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode > 299 {
		f.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (f *fixture) post(body string) map[string]any {
	return f.do(http.MethodPost, "/v1/events", writeTok, body)
}

func (f *fixture) client(read, write string) *client.Client {
	return &client.Client{BaseURL: f.url, Token: read, WriteToken: write}
}

// session is one MCP client connected to a tools server in memory.
type session struct {
	t     *testing.T
	cs    *mcp.ClientSession
	tools map[string]*mcp.Tool
}

func connect(t *testing.T, o Options, clientName string) *session {
	t.Helper()
	if o.URLHost == "" {
		o.URLHost = "heraldo.test"
	}
	srv := New(o)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	c := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{t: t, cs: cs, tools: map[string]*mcp.Tool{}}
	for _, tl := range lt.Tools {
		s.tools[tl.Name] = tl
	}
	return s
}

func (f *fixture) connect(write bool) *session {
	o := Options{API: f.client(readTok, ""), WriteOff: "no HERALDO_WRITE_TOKEN", Host: "box"}
	if write {
		o = Options{API: f.client(readTok, writeTok), Write: true, Host: "box"}
	}
	return connect(f.t, o, "claude-code")
}

func (s *session) names() []string {
	var n []string
	for k := range s.tools {
		n = append(n, k)
	}
	slices.Sort(n)
	return n
}

// call calls a tool and checks the result against its output schema.
// It returns the structured content and the text.
func (s *session) call(name string, args any) (map[string]any, string, bool) {
	s.t.Helper()
	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	b, _ := json.Marshal(res.StructuredContent)
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		s.t.Fatalf("%s: structured content %s: %v", name, b, err)
	}
	if strings.Contains(string(b), readTok) || strings.Contains(string(b), writeTok) ||
		strings.Contains(text, readTok) || strings.Contains(text, writeTok) {
		s.t.Fatalf("%s: a token leaked into the result", name)
	}
	tool := s.tools[name]
	if tool == nil {
		s.t.Fatalf("%s is not listed", name)
	}
	if res.StructuredContent == nil && res.IsError {
		// The SDK refused the arguments before the handler ran.
		return out, text, true
	}
	sb, _ := json.Marshal(tool.OutputSchema)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(sb))
	if err != nil {
		s.t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("out.json", doc); err != nil {
		s.t.Fatal(err)
	}
	sch, err := c.Compile("out.json")
	if err != nil {
		s.t.Fatal(err)
	}
	inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err := sch.Validate(inst); err != nil {
		s.t.Fatalf("%s: output does not match its schema: %v\n%s", name, err, b)
	}
	return out, text, res.IsError
}

// ok calls a tool that must succeed.
func (s *session) ok(name string, args any) (map[string]any, string) {
	s.t.Helper()
	out, text, isErr := s.call(name, args)
	if isErr {
		s.t.Fatalf("%s: %s", name, text)
	}
	return out, text
}

// fail calls a tool that must fail with code.
func (s *session) fail(name string, args any, code string) string {
	s.t.Helper()
	out, text, isErr := s.call(name, args)
	e, _ := out["error"].(map[string]any)
	if !isErr || e == nil || e["code"] != code {
		s.t.Fatalf("%s: want error %s, got isError=%v %v: %s", name, code, isErr, out, text)
	}
	return text
}

func TestToolLists(t *testing.T) {
	f := newFixture(t)
	if got := connect(t, Options{NotConfigured: "HERALDO_URL is not set"}, "x").names(); !slices.Equal(got, []string{"overview"}) {
		t.Fatalf("not configured: %v", got)
	}
	read := slices.Sorted(slices.Values(ReadTools))
	if got := f.connect(false).names(); !slices.Equal(got, read) {
		t.Fatalf("read only: %v", got)
	}
	all := slices.Sorted(slices.Values(slices.Concat(ReadTools, WriteTools)))
	s := f.connect(true)
	if got := s.names(); !slices.Equal(got, all) {
		t.Fatalf("write: %v", got)
	}
	for _, n := range ReadTools {
		if a := s.tools[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s: want readOnlyHint", n)
		}
	}
	for _, n := range WriteTools {
		if a := s.tools[n].Annotations; a == nil || a.ReadOnlyHint {
			t.Errorf("%s: want no readOnlyHint", n)
		}
	}
	if a := s.tools["send"].Annotations; a.OpenWorldHint == nil || !*a.OpenWorldHint || a.IdempotentHint {
		t.Errorf("send annotations: %+v", a)
	}
	if a := s.tools["checkin_update"].Annotations; a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("checkin_update annotations: %+v", a)
	}
}

func TestInstructions(t *testing.T) {
	f := newFixture(t)
	ro := connect(t, Options{API: f.client(readTok, "")}, "x").cs.InitializeResult().Instructions
	rw := connect(t, Options{API: f.client(readTok, writeTok), Write: true}, "x").cs.InitializeResult().Instructions
	if !strings.Contains(ro, "read-only") || strings.Contains(ro, "dedup") {
		t.Fatalf("read-only instructions: %s", ro)
	}
	if strings.Contains(rw, "read-only") || !strings.Contains(rw, "dedup") || strings.Contains(rw, "mark_read") {
		t.Fatalf("write instructions: %s", rw)
	}
}

func TestNotConfigured(t *testing.T) {
	s := connect(t, Options{NotConfigured: "HERALDO_TOKEN is not set", ConfigHint: "set it in /x/env"}, "x")
	text := s.fail("overview", map[string]any{}, "not_configured")
	if !strings.Contains(text, "HERALDO_TOKEN") || !strings.Contains(text, "/x/env") {
		t.Fatal(text)
	}
}

func TestOverview(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"web","kind":"incident","action":"open","key":"web:down","title":"Web is down","level":"time-sensitive"}`)
	f.post(`{"source":"db","kind":"incident","action":"open","key":"db:slow","title":"DB slow"}`)
	f.post(`{"source":"db","kind":"incident","action":"ack","key":"db:slow"}`)
	f.post(`{"source":"web","kind":"incident","action":"open","key":"gone","title":"Gone"}`)
	f.post(`{"source":"web","kind":"incident","action":"resolve","key":"gone"}`)
	f.do(http.MethodPut, "/v1/checkins/nightly", writeTok, `{"period_s":86400}`)
	f.do(http.MethodPut, "/v1/checkins/hourly", writeTok, `{"period_s":3600}`)
	f.do(http.MethodPost, "/ping/nightly/fail", writeTok, "disk full")
	ev := f.post(`{"source":"s","kind":"message","title":"first"}`)
	f.do(http.MethodPut, "/v1/cursors/inbox", readTok, `{"cursor":"`+ev["cursor"].(string)+`"}`)
	f.post(`{"source":"s","kind":"message","title":"unread one"}`)
	f.do(http.MethodPost, "/v1/mutes", writeTok, `{"source":"noisy","for":"1h"}`)

	out, text := f.connect(true).ok("overview", map[string]any{})
	inc := out["incidents"].(map[string]any)
	items := inc["items"].([]any)
	// The failed ping opened checkin:nightly too; acked comes last.
	if inc["total"].(float64) != 3 || items[0].(map[string]any)["key"] != "web:down" || items[2].(map[string]any)["state"] != "acked" {
		t.Fatalf("incidents: %v", inc)
	}
	ck := out["checkins"].(map[string]any)
	if ck["total"].(float64) != 2 || len(ck["items"].([]any)) != 1 || ck["items"].([]any)[0].(map[string]any)["slug"] != "nightly" {
		t.Fatalf("checkins: %v", ck)
	}
	if u := out["unread"].(map[string]any); u["count"].(float64) != 1 {
		t.Fatalf("unread: %v", u)
	}
	if m := out["mutes"].(map[string]any)["items"].([]any); len(m) != 1 {
		t.Fatalf("mutes: %v", m)
	}
	srv := out["server"].(map[string]any)
	if srv["write"] != true || srv["source"] != "agent:claude" || srv["version"] != "test" {
		t.Fatalf("server: %v", srv)
	}
	if !strings.Contains(text, "web:down [open]") || !strings.Contains(text, "nightly [down]") {
		t.Fatalf("text:\n%s", text)
	}

	// Without a write token, mutes are not listed and the answer says why.
	out, _ = f.connect(false).ok("overview", map[string]any{"sections": []string{"mutes", "unread"}})
	if out["mutes"] != nil || !strings.Contains(fmt.Sprint(out["notes"]), "write token") || out["incidents"] != nil {
		t.Fatalf("read-only overview: %v", out)
	}
	if out["server"].(map[string]any)["write_off"] != "no HERALDO_WRITE_TOKEN" {
		t.Fatalf("write_off: %v", out["server"])
	}
}

func TestSearchPaging(t *testing.T) {
	f := newFixture(t)
	for i := range 45 {
		f.post(fmt.Sprintf(`{"source":"job","kind":"message","title":"run %02d","body":"line one\nline two"}`, i))
		f.post(fmt.Sprintf(`{"source":"other","kind":"message","title":"noise %02d"}`, i))
	}
	s := f.connect(false)
	for _, order := range []string{"newest", "oldest"} {
		var seen []string
		args := map[string]any{"source": "job", "order": order, "limit": 20}
		pages := 0
		for {
			out, _ := s.ok("events_search", args)
			pages++
			for _, e := range out["events"].([]any) {
				m := e.(map[string]any)
				if m["source"] != "job" || m["summary"] != "line one" || m["has_body"] != true {
					t.Fatalf("%s: row %v", order, m)
				}
				seen = append(seen, m["title"].(string))
			}
			if out["has_more"] != true {
				break
			}
			args = map[string]any{"cursor": out["next_cursor"]}
		}
		if pages != 3 || len(seen) != 45 {
			t.Fatalf("%s: %d pages, %d rows", order, pages, len(seen))
		}
		want := slices.Clone(seen)
		slices.Sort(want)
		if order == "newest" {
			slices.Reverse(want)
		}
		if !slices.Equal(seen, want) || len(slices.Compact(slices.Clone(want))) != 45 {
			t.Fatalf("%s: order or repeats wrong: %v", order, seen)
		}
	}
	s.fail("events_search", map[string]any{"cursor": "not-a-cursor"}, "invalid")
}

func TestSearchUnreadAndPoll(t *testing.T) {
	f := newFixture(t)
	s := f.connect(false)
	out, _ := s.ok("events_search", map[string]any{"unread": true})
	if !strings.Contains(fmt.Sprint(out["notes"]), "does not exist") {
		t.Fatalf("no cursor: %v", out)
	}
	first := f.post(`{"source":"s","kind":"message","title":"read"}`)
	f.do(http.MethodPut, "/v1/cursors/inbox", readTok, `{"cursor":"`+first["cursor"].(string)+`"}`)
	f.post(`{"source":"s","kind":"message","title":"new 1"}`)
	f.post(`{"source":"s","kind":"message","title":"new 2"}`)
	out, _ = s.ok("events_search", map[string]any{"unread": true})
	ev := out["events"].([]any)
	if len(ev) != 2 || ev[0].(map[string]any)["title"] != "new 1" || out["unread_count"].(float64) != 2 || out["order"] != "oldest" {
		t.Fatalf("unread: %v", out)
	}
	// An empty page keeps a cursor to poll with; the cursor never moved.
	out, _ = s.ok("events_search", map[string]any{"cursor": out["next_cursor"]})
	if len(out["events"].([]any)) != 0 || out["next_cursor"] == "" {
		t.Fatalf("poll: %v", out)
	}
	next := out["next_cursor"]
	f.post(`{"source":"s","kind":"message","title":"new 3"}`)
	out, _ = s.ok("events_search", map[string]any{"cursor": next})
	if ev := out["events"].([]any); len(ev) != 1 || ev[0].(map[string]any)["title"] != "new 3" {
		t.Fatalf("poll after post: %v", out)
	}
	if c := f.do(http.MethodGet, "/v1/cursors/inbox", readTok, ""); c["cursor"] != first["cursor"] {
		t.Fatalf("the cursor moved: %v", c)
	}
}

func TestSearchTruncates(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("x", 250)
	tags, _ := json.Marshal(slices.Repeat([]string{strings.Repeat("t", 64)}, 16))
	for range 100 {
		f.post(`{"source":"s","kind":"message","title":"` + long + `","summary":"` + long[:178] + `","tags":` + string(tags) + `}`)
	}
	out, _ := f.connect(false).ok("events_search", map[string]any{"limit": 100})
	b, _ := json.Marshal(out)
	if len(b) > maxResult || out["truncated"] != true || out["has_more"] != true || len(out["events"].([]any)) == 100 {
		t.Fatalf("size %d, truncated %v, rows %d", len(b), out["truncated"], len(out["events"].([]any)))
	}
}

func TestEventReadPages(t *testing.T) {
	f := newFixture(t)
	// 200 KiB with a three-byte character across every 16 KiB boundary.
	var body strings.Builder
	for body.Len() < 200<<10 {
		body.WriteString(strings.Repeat("a", bodyPage-1) + "€")
	}
	full := strings.ToValidUTF8(body.String()[:200<<10], "")
	body.Reset()
	body.WriteString(full)
	b, _ := json.Marshal(map[string]any{"source": "s", "kind": "message", "title": "big", "body": body.String(),
		"details": map[string]any{"blob": strings.Repeat("d", 5000)}})
	id := f.post(string(b))["id"].(string)
	s := f.connect(false)
	var got strings.Builder
	off, calls := 0, 0
	for {
		out, _ := s.ok("event_read", map[string]any{"id": id, "body_offset": off})
		calls++
		got.WriteString(out["body"].(string))
		if d := out["details"].(map[string]any); d["_omitted_bytes"] == nil {
			t.Fatalf("details not capped: %v", d)
		}
		pg := out["body_page"].(map[string]any)
		n, ok := pg["next_offset"]
		if !ok {
			break
		}
		off = int(n.(float64))
	}
	if got.String() != body.String() {
		t.Fatalf("reassembled body differs: %d of %d bytes", got.Len(), body.Len())
	}
	if calls != 13 {
		t.Fatalf("%d calls, want 13", calls)
	}
	s.fail("event_read", map[string]any{"id": "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9"}, "not_found")
}

func TestCheckins(t *testing.T) {
	f := newFixture(t)
	f.do(http.MethodPut, "/v1/checkins/backup", writeTok, `{"period_s":86400,"grace_s":600}`)
	f.do(http.MethodPost, "/ping/backup/fail", writeTok, strings.Repeat("ok\n", 5000)+"ERROR: disk full")
	s := f.connect(true)
	out, text := s.ok("checkin_read", map[string]any{"slug": "backup"})
	log := out["last_log"].(string)
	if out["last_log_truncated"] != true || len(log) > logCap || !strings.HasSuffix(log, "ERROR: disk full") {
		t.Fatalf("log: truncated=%v len=%d", out["last_log_truncated"], len(log))
	}
	if !strings.Contains(text, "backup: down") {
		t.Fatal(text)
	}
	s.fail("checkin_read", map[string]any{"slug": "nope"}, "not_found")

	out, _ = s.ok("checkin_update", map[string]any{"slug": "backup", "operation": "pause"})
	if out["state"] != "paused" || out["previous_state"] != "down" {
		t.Fatalf("pause: %v", out)
	}
	out, _ = s.ok("checkin_update", map[string]any{"slug": "backup", "operation": "resume"})
	if out["state"] != "new" || out["previous_state"] != "paused" {
		t.Fatalf("resume: %v", out)
	}
	out, text = s.ok("checkin_update", map[string]any{"slug": "fresh", "operation": "configure",
		"schedule": map[string]any{"period": "6h", "grace": "15m"}})
	sch := out["schedule"].(map[string]any)
	if sch["period_s"].(float64) != 21600 || sch["grace_s"].(float64) != 900 || !strings.HasPrefix(text, "created") {
		t.Fatalf("configure: %v %s", out, text)
	}
	s.fail("checkin_update", map[string]any{"slug": "fresh", "operation": "configure"}, "invalid")
	s.fail("checkin_update", map[string]any{"slug": "fresh", "operation": "pause", "schedule": map[string]any{"period": "1h"}}, "invalid")
	s.fail("checkin_update", map[string]any{"slug": "fresh", "operation": "configure", "schedule": map[string]any{"period": "soon"}}, "invalid")
	s.fail("checkin_update", map[string]any{"slug": "nope", "operation": "pause"}, "not_found")
}

func TestIncidentAct(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"web","kind":"incident","action":"open","key":"web:down","host":"web-1","title":"Web is down"}`)
	s := f.connect(true)
	out, _ := s.ok("incident_act", map[string]any{"key": "web:down", "action": "ack", "note": "looking"})
	if out["changed"] != true || out["incident"].(map[string]any)["state"] != "acked" {
		t.Fatalf("ack: %v", out)
	}
	out, _ = s.ok("incident_act", map[string]any{"key": "web:down", "action": "ack"})
	if out["changed"] != false {
		t.Fatalf("second ack: %v", out)
	}
	out, _ = s.ok("incident_act", map[string]any{"key": "web:down", "action": "resolve"})
	if out["changed"] != true || out["incident"].(map[string]any)["state"] != "resolved" {
		t.Fatalf("resolve: %v", out)
	}
	out, _ = s.ok("incident_act", map[string]any{"key": "web:down", "action": "resolve"})
	if out["changed"] != false {
		t.Fatalf("second resolve: %v", out)
	}
	page := f.do(http.MethodGet, "/v1/events?key=web:down", readTok, "")
	evs := page["events"].([]any)
	if len(evs) != 3 {
		t.Fatalf("stored %d events, want open, ack, resolve", len(evs))
	}
	ack := evs[1].(map[string]any)
	if ack["source"] != "agent:claude" || ack["host"] != "web-1" || ack["body"] != "looking" || ack["level"] != "passive" {
		t.Fatalf("ack event: %v", ack)
	}
	s.fail("incident_act", map[string]any{"key": "nope", "action": "ack"}, "no_open_incident")
}

func TestMute(t *testing.T) {
	f := newFixture(t)
	s := f.connect(true)
	out, _ := s.ok("mute", map[string]any{"operation": "create", "source": "noisy", "for": "2h"})
	id := out["mute"].(map[string]any)["id"].(string)
	if out["created"] != true {
		t.Fatalf("create: %v", out)
	}
	out, _ = s.ok("mute", map[string]any{"operation": "create", "source": "noisy", "for": "2h"})
	if out["created"] != false || out["mute"].(map[string]any)["id"] != id {
		t.Fatalf("retry stacked a mute: %v", out)
	}
	if l := f.do(http.MethodGet, "/v1/mutes", writeTok, "")["mutes"].([]any); len(l) != 1 {
		t.Fatalf("%d mutes", len(l))
	}
	s.fail("mute", map[string]any{"operation": "create"}, "invalid")
	s.fail("mute", map[string]any{"operation": "create", "key": "k", "for": "1s"}, "invalid")
	out, _ = s.ok("mute", map[string]any{"operation": "end", "mute_id": id})
	if out["mute"].(map[string]any)["active"] != false {
		t.Fatalf("end: %v", out)
	}
	s.fail("mute", map[string]any{"operation": "end", "mute_id": "mute_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9"}, "not_found")
}

func TestSend(t *testing.T) {
	f := newFixture(t)
	s := f.connect(true)
	out, _ := s.ok("send", map[string]any{"title": "Build done", "body": "all green", "level": "passive", "dedup": "build-1"})
	id := out["id"].(string)
	if out["duplicate"] != false || out["pushed"] != "passive" || out["source"] != "agent:claude" {
		t.Fatalf("send: %v", out)
	}
	out, _ = s.ok("send", map[string]any{"title": "Build done", "dedup": "build-1"})
	if out["duplicate"] != true || out["id"] != id {
		t.Fatalf("repeat: %v", out)
	}
	e := f.do(http.MethodGet, "/v1/events/"+id, readTok, "")
	if e["kind"] != "message" || e["host"] != "box" || e["source"] != "agent:claude" {
		t.Fatalf("stored: %v", e)
	}
	// Plain messages only: the schema has no kind, action or details.
	for _, field := range []string{"kind", "action", "content", "details", "source"} {
		s.call("send", map[string]any{"title": "x", field: "incident"})
		if _, ok := s.tools["send"].InputSchema.(map[string]any)["properties"].(map[string]any)[field]; ok {
			t.Fatalf("send takes %s", field)
		}
	}
	if _, _, isErr := s.call("send", map[string]any{"title": "x", "kind": "incident"}); !isErr {
		t.Fatal("send took kind")
	}
	// This server has no project field: it refuses one.
	s.fail("send", map[string]any{"title": "x", "project": "heraldo"}, "unsupported_filter")
	// HERALDO_SOURCE wins over the client's name.
	o := connect(t, Options{API: f.client(readTok, writeTok), Write: true, Source: "my-agent"}, "claude-code")
	out, _ = o.ok("send", map[string]any{"title": "x"})
	if out["source"] != "my-agent" {
		t.Fatalf("source: %v", out)
	}
}

func TestSource(t *testing.T) {
	for name, want := range map[string]string{
		"claude-code": "claude", "codex-mcp-client": "codex", "opencode": "opencode", "Some Tool/2": "some-tool-2",
	} {
		if got := harnessFromClient(name); got != want {
			t.Errorf("%q: %s, want %s", name, got, want)
		}
	}
	if got := (&tools{Options{Harness: "claude"}}).source(nil); got != "agent:claude" {
		t.Errorf("harness from env: %s", got)
	}
	if got := (&tools{}).source(nil); got != "heraldo-mcp" {
		t.Errorf("no harness: %s", got)
	}
	if HarnessFromEnv(func(k string) string { return map[string]string{"CLAUDECODE": "1"}[k] }) != "claude" {
		t.Error("CLAUDECODE")
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	f.post(`{"source":"web","kind":"incident","action":"open","key":"k","title":"t"}`)
	bad := connect(t, Options{API: f.client("wrong-token", "")}, "x")
	text := bad.fail("events_search", map[string]any{}, "unauthorized")
	if !strings.Contains(text, "HERALDO_TOKEN") {
		t.Fatal(text)
	}
	bad.fail("overview", map[string]any{}, "unauthorized")
	// A read token in the write slot: the write calls get 403.
	ro := connect(t, Options{API: f.client(readTok, readTok), Write: true}, "x")
	text = ro.fail("incident_act", map[string]any{"key": "k", "action": "ack"}, "forbidden")
	if !strings.Contains(text, "HERALDO_WRITE_TOKEN") {
		t.Fatal(text)
	}
	ro.fail("send", map[string]any{"title": "x"}, "forbidden")
	ro.fail("mute", map[string]any{"operation": "create", "key": "k"}, "forbidden")
	down := connect(t, Options{API: &client.Client{BaseURL: "http://127.0.0.1:1", Token: readTok}}, "x")
	text = down.fail("events_search", map[string]any{"q": "secret"}, "unreachable")
	if strings.Contains(text, "secret") {
		t.Fatalf("the query leaked: %s", text)
	}
	down.fail("overview", map[string]any{}, "unreachable")
}

// fakeServer answers like a heraldo server from before the project field
// and, with noOrder, before newest-first lists.
func fakeServer(t *testing.T, noOrder bool) string {
	t.Helper()
	ev := `{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","cursor":"c1","source":"s","kind":"message","title":"t","ts":"2026-10-27T21:04:05Z","received_at":"2026-10-27T21:04:05Z"}`
	act := `{"key":"print/42","id":"act_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","state":"active","source":"printer",
		"content":{"title":"bracket.3mf","status":"Printing","progress":0.42},
		"started_at":"2026-10-27T20:00:00Z","updated_at":"2026-10-27T21:00:00Z",
		"instances":[{"id":"lai_1","device":"dev_1","chain":0,"state":"active","requested_at":"2026-10-27T20:00:00Z"}],
		"timeline":[` + strings.TrimSuffix(strings.Repeat(`{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","received_at":"2026-10-27T20:00:00Z","action":"update","content":{"progress":0.1}},`, 80), ",") + `]}`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		if !noOrder && r.URL.Query().Get("order") == "desc" {
			w.Header().Set("Heraldo-Order", "desc")
		}
		fmt.Fprintf(w, `{"events":[%s],"next_cursor":"c1"}`, ev)
	})
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"incidents":[{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","source":"s","kind":"incident","key":"k","ts":"2026-10-27T21:04:05Z","incident":{"state":"open","opened_at":"2026-10-27T21:04:05Z"}},`+
			`{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z8","source":"s","kind":"incident","action":"ack","key":"old","ts":"2026-10-27T21:04:05Z","received_at":"2026-10-27T21:04:05Z"}],"progress":[],"checkins":[],"statuses":[]}`)
	})
	mux.HandleFunc("GET /v1/checkins", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"checkins":[]}`) })
	mux.HandleFunc("GET /v1/activities", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"activities":[]}`) })
	mux.HandleFunc("GET /v1/activities/{key...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("key") != "print/42" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no activity on that key"}`)
			return
		}
		fmt.Fprint(w, act)
	})
	mux.HandleFunc("GET /v1/cursors/{name}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"cursor":"c0","unread":0}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","db":"ok","version":"old"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestOldServer(t *testing.T) {
	u := fakeServer(t, true)
	s := connect(t, Options{API: &client.Client{BaseURL: u, Token: readTok}}, "x")
	s.fail("events_search", map[string]any{}, "server_too_old")
	if out, _ := s.ok("events_search", map[string]any{"order": "oldest"}); len(out["events"].([]any)) != 1 {
		t.Fatalf("oldest: %v", out)
	}
	u = fakeServer(t, false)
	s = connect(t, Options{API: &client.Client{BaseURL: u, Token: readTok}}, "x")
	s.fail("events_search", map[string]any{"project": []string{"heraldo"}}, "unsupported_filter")
	s.fail("overview", map[string]any{"project": []string{"heraldo"}}, "unsupported_filter")
	if out, _ := s.ok("events_search", map[string]any{}); out["events"].([]any)[0].(map[string]any)["project"] != nil {
		t.Fatalf("project: %v", out)
	}
	// An incident event stored before snapshots: its action gives the state.
	out, text := s.ok("overview", map[string]any{"sections": []string{"incidents"}})
	items := out["incidents"].(map[string]any)["items"].([]any)
	if items[0].(map[string]any)["state"] != "open" || items[1].(map[string]any)["state"] != "acked" || strings.Contains(text, "since ,") {
		t.Fatalf("incidents: %v\n%s", items, text)
	}
}

func TestProjectFilter(t *testing.T) {
	// A server with projects: every answer to a filtered list carries the
	// field, so the filter is honoured. The MCP sends one project
	// parameter per name.
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Heraldo-Order", "desc")
		fmt.Fprint(w, `{"events":[{"id":"01JBA6Y8Q2K9M3T4V5W6X7Y8Z9","cursor":"c1","source":"s","kind":"message","project":"heraldo","ts":"2026-10-27T21:04:05Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := connect(t, Options{API: &client.Client{BaseURL: srv.URL, Token: readTok}}, "x")
	out, _ := s.ok("events_search", map[string]any{"project": []string{"heraldo", "servers"}})
	if !slices.Equal(got["project"], []string{"heraldo", "servers"}) {
		t.Fatalf("sent %v", got)
	}
	if out["events"].([]any)[0].(map[string]any)["project"] != "heraldo" {
		t.Fatalf("%v", out)
	}
	// The filter rides in the cursor.
	s.ok("events_search", map[string]any{"cursor": out["next_cursor"]})
	if !slices.Equal(got["project"], []string{"heraldo", "servers"}) || got.Get("before") != "c1" {
		t.Fatalf("page 2 sent %v", got)
	}
}

func TestActivityRead(t *testing.T) {
	u := fakeServer(t, false)
	s := connect(t, Options{API: &client.Client{BaseURL: u, Token: readTok}}, "x")
	out, text := s.ok("activity_read", map[string]any{"key": "print/42"})
	if len(out["timeline"].([]any)) != 50 || out["timeline_total"].(float64) != 80 {
		t.Fatalf("timeline: %d of %v", len(out["timeline"].([]any)), out["timeline_total"])
	}
	if !strings.Contains(text, "bracket.3mf · Printing · 42%") || !strings.Contains(text, "device dev_1 link 0: active") {
		t.Fatal(text)
	}
	out, _ = s.ok("activity_read", map[string]any{"key": "print/42", "timeline_limit": 0})
	if len(out["timeline"].([]any)) != 0 {
		t.Fatalf("limit 0: %v", out["timeline"])
	}
	s.fail("activity_read", map[string]any{"key": "nope"}, "not_found")
}
