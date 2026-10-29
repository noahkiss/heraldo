package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

// ErrChatOff means llm.chat is off, llm.mode is off, or this is a standby.
var ErrChatOff = errors.New("chat is off: it needs llm.chat on and llm.mode not off, on the primary")

// Toolbox runs the read-only tools the chat model may call. Call returns
// the tool's structured result as JSON; the chat role applies the Policy
// to it before the model sees it.
type Toolbox interface {
	Tools(ctx context.Context) ([]ToolDef, error)
	Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// ChatTools are the only tools the chat model may call. Each one reads.
var ChatTools = []string{"events_search", "event_read", "overview", "checkin_read"}

// Chat limits.
const (
	// MaxRounds caps the model's tool-calling rounds per question.
	MaxRounds = 4
	// MaxToolResult caps one tool result, MaxContext all of them.
	MaxToolResult = 16 << 10
	MaxContext    = 64 << 10
	// PreloadLimit is how many events the server searches before the
	// first call.
	PreloadLimit = 30
)

// Scope is what a question is about.
type Scope struct {
	View, Project, Key string
	Since              time.Time
}

// Question is one question to the chat model.
type Question struct {
	Text  string
	Scope Scope
	// Guidance lines go in the system prompt, least specific first.
	Guidance []store.Guidance
}

// Answer is the chat model's answer. Proposal is the model's suggestion as
// JSON, unchecked: the caller checks it as the matching write call would.
type Answer struct {
	Text     string
	CitedIDs []string
	Proposal json.RawMessage
	CallIDs  []string
	Model    string
}

var answerSchema = &Schema{Name: "heraldo_answer", Schema: json.RawMessage(`{"type":"object",` +
	`"properties":{"answer":{"type":"string"},"cited_ids":{"type":"array","items":{"type":"string"}},` +
	`"proposal":{"type":"object","properties":{"type":{"type":"string","enum":["filter","view","guidance","mute"]},` +
	`"summary":{"type":"string"},"filter":{"type":"object"},"view":{"type":"object"},"guidance":{"type":"object"},` +
	`"mute":{"type":"object"}},"required":["type"]}},"required":["answer","cited_ids"]}`)}

var ulidIn = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)

// Ask answers q. It searches the scope first, then lets the model call the
// ChatTools for up to MaxRounds rounds. Every tool result passes the
// Policy before the model sees it, and an answer may cite only ids a tool
// returned. It writes nothing.
func (c *Client) Ask(ctx context.Context, tb Toolbox, q Question) (Answer, error) {
	if !c.ChatAllowed() {
		return Answer{}, ErrChatOff
	}
	pol := c.Policy()
	if pol.Excluded(q.Scope.Project) {
		return Answer{Text: "The project " + q.Scope.Project + " is excluded from the assistant (llm.exclude_projects).",
			CitedIDs: []string{}}, nil
	}
	prompt := LoadPrompt("chat")
	all, err := tb.Tools(ctx)
	if err != nil {
		return Answer{}, fmt.Errorf("chat tools: %w", err)
	}
	var tools []ToolDef
	for _, t := range all {
		if slices.Contains(ChatTools, t.Name) {
			tools = append(tools, t)
		}
	}
	seen := map[string]bool{}
	used := 0
	run := func(name string, args json.RawMessage) string {
		if !slices.Contains(ChatTools, name) {
			return `{"error":"no such tool"}`
		}
		if used >= MaxContext {
			return `{"error":"the context is full; answer with what you have"}`
		}
		raw, err := tb.Call(ctx, name, args)
		if err != nil {
			b, _ := json.Marshal(map[string]string{"error": pol.RedactText(err.Error())})
			return string(b)
		}
		if name == "events_search" && !q.Scope.Since.IsZero() {
			raw = since(raw, q.Scope.Since)
		}
		out, ids, err := pol.SanitizeJSON(raw)
		if err != nil {
			return `{"error":"the tool result is not JSON"}`
		}
		if len(out) > MaxToolResult {
			out = append(out[:MaxToolResult:MaxToolResult], []byte("…(cut)")...)
		}
		used += len(out)
		for _, id := range ids {
			seen[id] = true
		}
		return string(out)
	}

	sys := prompt.Text + "\n\nNow: " + c.cfg.Now().UTC().Format(time.RFC3339)
	var gv []string
	if len(q.Guidance) > 0 {
		sys += "\n\nGuidance from the owner:"
		for _, g := range q.Guidance {
			sys += fmt.Sprintf("\n- (%s) %s", g.Scope, pol.RedactText(g.Text))
			gv = append(gv, fmt.Sprintf("%s:%d", g.ID, g.Version))
		}
	}
	preArgs := map[string]any{"limit": PreloadLimit, "order": "newest"}
	var scope []string
	if q.Scope.View != "" {
		preArgs["view"], scope = q.Scope.View, append(scope, "view "+q.Scope.View)
	}
	if q.Scope.Project != "" {
		preArgs["project"], scope = []string{q.Scope.Project}, append(scope, "project "+q.Scope.Project)
	}
	if q.Scope.Key != "" {
		preArgs["key"], scope = q.Scope.Key, append(scope, "key "+q.Scope.Key)
	}
	if !q.Scope.Since.IsZero() {
		scope = append(scope, "since "+q.Scope.Since.UTC().Format(time.RFC3339))
	}
	if len(scope) > 0 {
		sys += "\n\nThe question is about: " + strings.Join(scope, ", ") + "."
	}
	pre, _ := json.Marshal(preArgs)
	user := q.Text + "\n\nThe server ran events_search " + string(pre) + " first. It returned:\n" + run("events_search", pre)
	msgs := []Message{{Role: "system", Content: sys}, {Role: "user", Content: user}}
	temp := 0.2

	var ans Answer
	for round := 0; ; round++ {
		req := Request{Messages: msgs, Schema: answerSchema, Temperature: &temp}
		last := round >= MaxRounds
		if !last {
			req.Tools = tools
		}
		res, err := c.Call(ctx, Call{Role: RoleChat, Prompt: prompt, GuidanceVersions: gv,
			Input: map[string]any{"q": q.Text, "round": round}, Request: req})
		if res.CallID != "" {
			ans.CallIDs = append(ans.CallIDs, res.CallID)
		}
		if err != nil {
			return ans, err
		}
		ans.Model = res.Model
		if len(res.ToolCalls) == 0 || last {
			ans.Text, ans.CitedIDs, ans.Proposal = parseAnswer(res.Content, seen)
			return ans, nil
		}
		msgs = append(msgs, Message{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
		for _, tc := range res.ToolCalls {
			args := json.RawMessage(tc.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage(`{}`)
			}
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: run(tc.Name, args)})
		}
	}
}

// parseAnswer reads the model's answer. When it is not the JSON asked for,
// the whole text is the answer. Cited ids are kept only when a tool
// returned them, each once.
func parseAnswer(content string, seen map[string]bool) (string, []string, json.RawMessage) {
	var a struct {
		Answer   string          `json:"answer"`
		CitedIDs []string        `json:"cited_ids"`
		Proposal json.RawMessage `json:"proposal"`
	}
	text := strings.TrimSpace(content)
	var cited []string
	var proposal json.RawMessage
	if ParseJSON(content, &a) == nil && strings.TrimSpace(a.Answer) != "" {
		text, cited = strings.TrimSpace(a.Answer), a.CitedIDs
		if len(a.Proposal) > 0 && string(a.Proposal) != "null" {
			proposal = a.Proposal
		}
	} else {
		cited = ulidIn.FindAllString(text, -1)
	}
	out := []string{}
	for _, id := range cited {
		if seen[id] && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return text, out, proposal
}

// since drops the events received before t from an events_search result.
func since(raw json.RawMessage, t time.Time) json.RawMessage {
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	evs, ok := v["events"].([]any)
	if !ok {
		return raw
	}
	kept := make([]any, 0, len(evs))
	for _, e := range evs {
		m, _ := e.(map[string]any)
		at, _ := m["received_at"].(string)
		if ts, err := time.Parse(time.RFC3339, at); err == nil && ts.Before(t) {
			continue
		}
		kept = append(kept, e)
	}
	v["events"] = kept
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}
