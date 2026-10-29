package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
)

// noTools is a chat toolbox with nothing in it. The real one is
// mcphttp.NewToolbox, which imports this package.
type noTools struct{}

func (noTools) Tools(context.Context) ([]llm.ToolDef, error) { return nil, nil }
func (noTools) Call(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"events":[]}`), nil
}

func TestGuidanceTools(t *testing.T) {
	f := newFixture(t)
	w := f.connect(true)
	w.fail("guidance_write", map[string]any{"operation": "add", "scope": "global"}, "invalid")
	w.fail("guidance_write", map[string]any{"operation": "add", "scope": "nope", "text": "x"}, "invalid")
	out, _ := w.ok("guidance_write", map[string]any{"operation": "add", "scope": "source:backup", "text": "One host is one story."})
	id := out["guidance"].(map[string]any)["id"].(string)
	out, _ = w.ok("guidance_write", map[string]any{"operation": "edit", "id": id, "enabled": false})
	if g := out["guidance"].(map[string]any); g["version"] != float64(2) || g["enabled"] != false {
		t.Fatalf("edit: %v", g)
	}
	r := f.connect(false)
	out, text := r.ok("guidance_read", map[string]any{})
	if gs := out["guidance"].([]any); len(gs) != 1 || !strings.Contains(text, "(off)") {
		t.Fatalf("read: %v %s", out, text)
	}
	w.ok("guidance_write", map[string]any{"operation": "delete", "id": id})
	w.fail("guidance_write", map[string]any{"operation": "delete", "id": id}, "not_found")
}

func TestLLMStatusAndAskTools(t *testing.T) {
	f := newFixture(t)
	r := f.connect(false)
	out, _ := r.ok("llm_status", map[string]any{})
	if out["mode"] != "off" || out["chat"] != false {
		t.Fatalf("status %v", out)
	}
	r.fail("ask", map[string]any{"question": "hi"}, "llm_off")

	m, err := settings.New(context.Background(), settings.Options{Getenv: func(k string) string {
		return map[string]string{"HERALDO_LLM_MODE": "shadow", "HERALDO_LLM_CHAT": "true",
			"HERALDO_LLM_BASE_URL": "https://llm.example.net/v1", "HERALDO_LLM_CHAT_MODEL": "m"}[k]
	}})
	if err != nil {
		t.Fatal(err)
	}
	fk := &llm.Fake{Replies: []llm.Response{{Content: `{"answer":"Quiet night.","cited_ids":[]}`}}}
	f2 := newFixtureWith(t, func(o *server.Options) {
		o.Settings, o.LLMProvider = m, fk.Factory
		o.AskTools = func(http.Handler) llm.Toolbox { return noTools{} }
	})
	out, text := f2.connect(false).ok("ask", map[string]any{"question": "what happened overnight?", "since": "12h"})
	if out["answer"] != "Quiet night." || !strings.Contains(text, "Quiet night.") {
		t.Fatalf("ask: %v %s", out, text)
	}
}
