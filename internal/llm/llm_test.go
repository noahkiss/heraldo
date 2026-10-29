package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/settings"
	"github.com/noahkiss/heraldo/internal/store"
)

const testToken = "sk-test-0000-secret"

type env map[string]string

func (e env) get(k string) string { return e[k] }

func manager(t *testing.T, e env) *settings.Manager {
	t.Helper()
	m, err := settings.New(context.Background(), settings.Options{Getenv: e.get})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// on is an env with the LLM on and configured.
func on(extra env) env {
	e := env{"HERALDO_LLM_MODE": "enforce", "HERALDO_LLM_BASE_URL": "https://llm.example.net/v1",
		"HERALDO_LLM_CHAT_MODEL": "chat-m", "HERALDO_LLM_DECISIONS_MODEL": "dec-m", "HERALDO_LLM_TOKEN": testToken,
		"HERALDO_LLM_CHAT": "true"}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type notices struct {
	mu sync.Mutex
	f  []map[string]any
}

func (n *notices) notify(_ context.Context, f map[string]any) error {
	n.mu.Lock()
	n.f = append(n.f, f)
	n.mu.Unlock()
	return nil
}

func (n *notices) len() int { n.mu.Lock(); defer n.mu.Unlock(); return len(n.f) }

func newClient(t *testing.T, e env, f *Fake, log CallLog) (*Client, *clock, *notices) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	n := &notices{}
	c := New(Config{Settings: manager(t, e), Log: log, Notify: n.notify, NewProvider: f.Factory, Now: clk.now})
	return c, clk, n
}

func simple(content string) Call {
	return Call{Role: RoleDecisions, Prompt: Prompt{Version: "p-1"}, Input: content,
		Request: Request{Messages: []Message{{Role: "user", Content: content}}}}
}

func TestOffAndStandbyCallNothing(t *testing.T) {
	f := &Fake{Replies: []Response{{Content: "x"}}}
	c, _, _ := newClient(t, env{}, f, nil)
	if _, err := c.Call(context.Background(), simple("a")); !errors.Is(err, ErrOff) {
		t.Fatalf("off: %v", err)
	}
	if c.Health() != "off" || c.EffectiveMode() != ModeOff || c.ChatAllowed() {
		t.Fatalf("off: health %s mode %s", c.Health(), c.EffectiveMode())
	}
	sb := New(Config{Settings: manager(t, on(nil)), Standby: true, NewProvider: f.Factory})
	if _, err := sb.Call(context.Background(), Call{Force: true}); !errors.Is(err, ErrStandby) {
		t.Fatalf("standby: %v", err)
	}
	if sb.EffectiveMode() != ModeOff || sb.Health() != "off" || sb.ChatAllowed() {
		t.Fatal("standby must report off")
	}
	if len(f.Requests()) != 0 {
		t.Fatal("a request left the server")
	}
}

func TestRedactsAndLogsWithoutToken(t *testing.T) {
	st := openStore(t)
	f := &Fake{Replies: []Response{{Content: `{"ok":true}`, TokensIn: 10, TokensOut: 2}}}
	c, _, _ := newClient(t, on(nil), f, st)
	res, err := c.Call(context.Background(), simple("look: Bearer abcdefghijklmnop and op://vault/item"))
	if err != nil {
		t.Fatal(err)
	}
	got := f.Requests()[0]
	if strings.Contains(got.Messages[0].Content, "abcdefghijklmnop") || strings.Contains(got.Messages[0].Content, "op://") {
		t.Fatalf("not redacted: %q", got.Messages[0].Content)
	}
	if got.Model != "dec-m" || got.Structured != StructuredSchema {
		t.Fatalf("request: model %q structured %q", got.Model, got.Structured)
	}
	row, err := st.GetLLMCall(context.Background(), res.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.LLMOK || row.BaseHost != "llm.example.net" || row.TokensIn != 10 || string(row.Output) != `{"ok":true}` {
		t.Fatalf("row: %+v", row)
	}
	if n, _ := st.LLMTokensSince(context.Background(), time.Unix(0, 0)); n != 12 {
		t.Fatalf("tokens = %d", n)
	}
}

func TestCache(t *testing.T) {
	st := openStore(t)
	f := &Fake{Replies: []Response{{Content: `{"v":1}`}, {Content: `{"v":2}`}}}
	c, clk, _ := newClient(t, on(nil), f, st)
	call := simple("same")
	call.Cache = true
	a, _ := c.Call(context.Background(), call)
	b, err := c.Call(context.Background(), call)
	if err != nil || !b.Cached || b.Content != `{"v":1}` || len(f.Requests()) != 1 {
		t.Fatalf("hit: %+v %v, %d requests", b, err, len(f.Requests()))
	}
	if a.CallID == b.CallID {
		t.Fatal("a cache hit gets its own row")
	}
	// Another guidance version is another key.
	call.GuidanceVersions = []string{"gd_1:2"}
	if r, _ := c.Call(context.Background(), call); r.Cached {
		t.Fatal("new guidance version hit the cache")
	}
	clk.add(25 * time.Hour)
	call.GuidanceVersions = nil
	if r, _ := c.Call(context.Background(), call); r.Cached {
		t.Fatal("hit after 24 h")
	}
}

func TestCircuitOpensAfterThreeAndNoticesOnce(t *testing.T) {
	f := &Fake{Err: errors.New("boom " + testToken)}
	c, clk, n := newClient(t, on(nil), f, nil)
	for i := 0; i < 3; i++ {
		_, err := c.Call(context.Background(), simple("a"))
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := c.Call(context.Background(), simple("a")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("4th: %v", err)
	}
	if len(f.Requests()) != 3 {
		t.Fatalf("%d requests", len(f.Requests()))
	}
	st := c.Status(context.Background())
	if st.Circuit != "open" || st.EffectiveMode != "shadow" || c.Health() != "degraded" {
		t.Fatalf("status: %+v", st)
	}
	if st.LastError == nil || strings.Contains(*st.LastError, testToken) {
		t.Fatalf("last error: %v", st.LastError)
	}
	if n.len() != 1 || n.f[0]["level"] != "passive" || n.f[0]["source"] != "heraldo" {
		t.Fatalf("notices: %v", n.f)
	}
	// Half open after 10 minutes: one more failure opens it again, with
	// no second notice.
	clk.add(CircuitOpenFor + time.Second)
	_, _ = c.Call(context.Background(), simple("a"))
	if _, err := c.Call(context.Background(), simple("a")); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("reopen: %v", err)
	}
	if n.len() != 1 {
		t.Fatalf("second notice in one outage")
	}
	// A success closes it and ends the outage.
	clk.add(CircuitOpenFor + time.Second)
	f.mu.Lock()
	f.Err, f.Replies = nil, []Response{{Content: "ok"}}
	f.mu.Unlock()
	if _, err := c.Call(context.Background(), simple("a")); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(context.Background()); st.Circuit != "closed" || st.EffectiveMode != "enforce" {
		t.Fatalf("after success: %+v", st)
	}
}

func TestBudgets(t *testing.T) {
	f := &Fake{Replies: []Response{{Content: "ok", TokensIn: 60, TokensOut: 50}}}
	c, clk, n := newClient(t, on(env{"HERALDO_LLM_TOKENS_PER_DAY": "100", "HERALDO_LLM_CALLS_PER_MINUTE": "5"}), f, nil)
	if _, err := c.Call(context.Background(), simple("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), simple("b")); !errors.Is(err, ErrBudget) {
		t.Fatalf("over tokens: %v", err)
	}
	st := c.Status(context.Background())
	if st.EffectiveMode != "shadow" || st.ShadowUntil == nil || !st.ShadowUntil.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("status: %+v", st)
	}
	if n.len() != 1 {
		t.Fatalf("notices: %d", n.len())
	}
	// Midnight resets the day.
	clk.add(13 * time.Hour)
	if _, err := c.Call(context.Background(), simple("c")); err != nil {
		t.Fatalf("next day: %v", err)
	}
	if c.EffectiveMode() != "enforce" {
		t.Fatalf("next day mode %s", c.EffectiveMode())
	}

	// The per-minute cap.
	f2 := &Fake{Replies: []Response{{Content: "ok"}}}
	c2, clk2, _ := newClient(t, on(env{"HERALDO_LLM_CALLS_PER_MINUTE": "2"}), f2, nil)
	for i := 0; i < 2; i++ {
		if _, err := c2.Call(context.Background(), simple("a")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c2.Call(context.Background(), simple("a")); !errors.Is(err, ErrBudget) {
		t.Fatalf("per minute: %v", err)
	}
	clk2.add(61 * time.Second)
	if _, err := c2.Call(context.Background(), simple("a")); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
	if c2.EffectiveMode() != "shadow" {
		t.Fatal("a passed budget keeps the mode at shadow until midnight")
	}
}

func TestInvalidCountsAsFailure(t *testing.T) {
	f := &Fake{Replies: []Response{{Content: "not json"}}}
	c, _, _ := newClient(t, on(nil), f, nil)
	call := simple("a")
	call.Validate = func(r Response) error {
		var v map[string]any
		return ParseJSON(r.Content, &v)
	}
	if _, err := c.Call(context.Background(), call); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	if f := c.Status(context.Background()).Failures; f == nil || *f != 1 {
		t.Fatalf("failures %v", f)
	}
}

func TestNotConfigured(t *testing.T) {
	c, _, _ := newClient(t, env{"HERALDO_LLM_MODE": "shadow"}, &Fake{}, nil)
	if _, err := c.Call(context.Background(), simple("a")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
	if c.Health() != "degraded" {
		t.Fatalf("health %s", c.Health())
	}
}

func TestPolicySanitize(t *testing.T) {
	p := NewPolicy(5, settings.DefaultRedact, []string{"Secret"})
	raw := `{"events":[
		{"id":"01JAAAAAAAAAAAAAAAAAAAAAAA","project":"web","title":"t op://v/i","body":"0123456789","details":{"x":1},"url":"https://x"},
		{"id":"01JBBBBBBBBBBBBBBBBBBBBBBB","project":"secret","title":"hidden"},
		{"id":"01JCCCCCCCCCCCCCCCCCCCCCCC","summary":"first body line","summary_from_body":true}
	]}`
	out, ids, err := p.SanitizeJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"hidden", "op://", "details", "https://x", "56789", "summary_from_body", "ody line"} {
		if strings.Contains(s, bad) {
			t.Fatalf("%q left in %s", bad, s)
		}
	}
	if !strings.Contains(s, `"body":"01234…"`) || !strings.Contains(s, `"summary":"first…"`) {
		t.Fatalf("body cut: %s", s)
	}
	if len(ids) != 2 || ids[0] != "01JAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("ids %v", ids)
	}
	// Body chars 0 sends no body at all.
	out, _, _ = NewPolicy(0, nil, nil).SanitizeJSON([]byte(`{"id":"x","body":"abc","summary":"abc","summary_from_body":true}`))
	if strings.Contains(string(out), "abc") {
		t.Fatalf("body sent: %s", out)
	}
	// An excluded event read on its own.
	out, ids, _ = p.SanitizeJSON([]byte(`{"id":"01JBBBBBBBBBBBBBBBBBBBBBBB","project":"SECRET","title":"x"}`))
	if string(out) != `{"excluded":true}` || len(ids) != 0 {
		t.Fatalf("excluded: %s %v", out, ids)
	}
}

func TestPromptVersion(t *testing.T) {
	p := LoadPrompt("chat")
	if !strings.HasPrefix(p.Version, "chat-") || len(p.Version) != len("chat-")+12 || p.Text == "" {
		t.Fatalf("prompt %+v", p.Version)
	}
}

// openAIStub is a fake OpenAI-compatible endpoint. It never reaches a
// real API.
func openAIStub(t *testing.T, handle func(path string, body map[string]any) any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(handle(r.URL.Path, body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIChat(t *testing.T) {
	var seen map[string]any
	srv := openAIStub(t, func(path string, body map[string]any) any {
		if path != "/v1/chat/completions" {
			t.Errorf("path %s", path)
		}
		seen = body
		return map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "overview", "arguments": "{}"}}}}}},
			"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
		}
	})
	p := &OpenAIChat{BaseURL: srv.URL + "/v1/", Token: testToken}
	r, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:  []ToolDef{{Name: "overview", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Schema: &Schema{Name: "s", Schema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "overview" || r.TokensIn != 7 || r.TokensOut != 3 {
		t.Fatalf("response %+v", r)
	}
	if rf, _ := seen["response_format"].(map[string]any); rf["type"] != "json_schema" {
		t.Fatalf("response_format %v", seen["response_format"])
	}
	if seen["tools"] == nil {
		t.Fatal("no tools sent")
	}
	// Prompt mode: no response_format, no tools, the schema in the system
	// prompt.
	_, _ = p.Complete(context.Background(), Request{Model: "m", Structured: StructuredPrompt,
		Messages: []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
		Tools:    []ToolDef{{Name: "overview"}}, Schema: &Schema{Name: "s", Schema: json.RawMessage(`{"type":"object"}`)}})
	if seen["response_format"] != nil || seen["tools"] != nil {
		t.Fatalf("prompt mode sent %v %v", seen["response_format"], seen["tools"])
	}
	msgs, _ := seen["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if c, _ := first["content"].(string); !strings.Contains(c, "JSON Schema") {
		t.Fatalf("schema not in the system prompt: %v", first)
	}
	// A bad token: the error names the status, never the token.
	bad := &OpenAIChat{BaseURL: srv.URL + "/v1", Token: "wrong"}
	_, err = bad.Complete(context.Background(), Request{Model: "m"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenAIResponses(t *testing.T) {
	var seen map[string]any
	srv := openAIStub(t, func(path string, body map[string]any) any {
		if path != "/v1/responses" {
			t.Errorf("path %s", path)
		}
		seen = body
		return map[string]any{
			"output": []any{map[string]any{"type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": `{"answer":"hi"}`}}}},
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 2},
		}
	})
	p := &OpenAIResponses{BaseURL: srv.URL + "/v1", Token: testToken}
	r, err := p.Complete(context.Background(), Request{Model: "m",
		Messages: []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "overview", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "c1", Content: "{}"}},
		Schema: &Schema{Name: "s", Schema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != `{"answer":"hi"}` || r.TokensIn != 4 {
		t.Fatalf("response %+v", r)
	}
	if seen["instructions"] != "sys" {
		t.Fatalf("instructions %v", seen["instructions"])
	}
	if in, _ := seen["input"].([]any); len(in) != 3 {
		t.Fatalf("input %v", seen["input"])
	}
}

func TestParseJSON(t *testing.T) {
	var v struct{ A int }
	if err := ParseJSON("Sure:\n```json\n{\"A\": 3}\n```", &v); err != nil || v.A != 3 {
		t.Fatalf("%v %v", v, err)
	}
	if err := ParseJSON("no json", &v); err == nil {
		t.Fatal("want an error")
	}
}
