package mcptools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/client"
)

// ---- guidance_read ------------------------------------------------------

type guidanceReadIn struct {
	Scope string `json:"scope,omitempty" jsonschema:"Only lines with exactly this scope, such as global or project:home."`
}

const guidanceReadDesc = `List the guidance lines: the owner's free-text notes that inform the LLM's wording and grouping.

Best for: checking what the assistant has been told before adding a line, or finding a line's id to edit.
Returns: each line's id, scope (global, project:<p>, source:<s> or key:<regex>), text, enabled and version.
Not for: rules or floors. Guidance never raises a level or overrides an ingest rule.`

func (t *tools) addGuidanceRead(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "guidance_read", Description: guidanceReadDesc, Annotations: readOnly("Read guidance")},
		func(is *jsonschema.Schema) { maxLen(is.Properties["scope"], 300) },
		map[string]*jsonschema.Schema{
			"guidance": arr("Oldest first.", obj("", map[string]*jsonschema.Schema{
				"id": str("gd_ plus a ULID."), "scope": str(""), "text": str(""), "enabled": boolean(""), "version": integer(""),
			})),
		},
		t.guidanceRead)
}

func guidanceOut(g api.Guidance) map[string]any {
	return map[string]any{"id": g.Id, "scope": g.Scope, "text": g.Text, "enabled": g.Enabled, "version": g.Version}
}

func guidanceLine(g api.Guidance) string {
	off := ""
	if !g.Enabled {
		off = " (off)"
	}
	return fmt.Sprintf("%s [%s]%s: %s", g.Id, g.Scope, off, g.Text)
}

func noLLMErr(err error) error {
	if errors.Is(err, client.ErrNoLLM) {
		return toolErr("server_too_old", "this heraldo server has no LLM or guidance", "")
	}
	return err
}

func (t *tools) guidanceRead(ctx context.Context, _ *mcp.CallToolRequest, in guidanceReadIn) (result, error) {
	gs, err := t.API.Guidance(ctx, in.Scope)
	if err != nil {
		return result{}, t.apiErr(noLLMErr(err), call{})
	}
	out := make([]map[string]any, 0, len(gs))
	lines := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, guidanceOut(g))
		lines = append(lines, guidanceLine(g))
	}
	if len(lines) == 0 {
		lines = append(lines, "no guidance")
	}
	return result{out: map[string]any{"guidance": out}, text: strings.Join(lines, "\n")}, nil
}

// ---- guidance_write -----------------------------------------------------

type guidanceWriteIn struct {
	Operation string `json:"operation" jsonschema:"add a line, edit one by id, or delete one by id."`
	ID        string `json:"id,omitempty" jsonschema:"edit and delete: the line's id, from guidance_read."`
	Scope     string `json:"scope,omitempty" jsonschema:"add (required) and edit: global, project:<p>, source:<s> or key:<regex>."`
	Text      string `json:"text,omitempty" jsonschema:"add (required) and edit: the guidance, at most 1000 characters."`
	Enabled   *bool  `json:"enabled,omitempty" jsonschema:"add and edit: false turns the line off. Default true on add."`
}

const guidanceWriteDesc = `Add, edit or delete a guidance line for the LLM: how to word and group alerts, such as "backup alerts from one host are one story".

Best for: the owner says how the assistant should describe or group something.
Guidance informs wording and grouping only. It never raises or lowers a level, never touches time-sensitive or critical, and never overrides an ingest rule or a mute.
Returns: the line after the change; edit raises its version.
Not for: muting or routing; use mute, or the owner's rules file.`

func (t *tools) addGuidanceWrite(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "guidance_write", Description: guidanceWriteDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Change guidance", DestructiveHint: ptr(true), OpenWorldHint: &noAnnot}},
		func(is *jsonschema.Schema) {
			p := is.Properties
			enum(p["operation"], "add", "edit", "delete")
			maxLen(p["scope"], 300)
			maxLen(p["text"], 1000)
			maxLen(p["id"], 64)
		},
		map[string]*jsonschema.Schema{
			"guidance": anyObj("add and edit: the line."),
			"deleted":  boolean("delete: true when the line was removed."),
		},
		t.guidanceWrite)
}

func (t *tools) guidanceWrite(ctx context.Context, _ *mcp.CallToolRequest, in guidanceWriteIn) (result, error) {
	var g api.Guidance
	var err error
	switch in.Operation {
	case "add":
		if in.Scope == "" || in.Text == "" {
			return result{}, toolErr("invalid", "add needs scope and text", "")
		}
		g, err = t.API.AddGuidance(ctx, api.GuidanceInput{Scope: in.Scope, Text: in.Text, Enabled: in.Enabled})
	case "edit":
		if in.ID == "" {
			return result{}, toolErr("invalid", "edit needs id", "guidance_read lists the lines")
		}
		p := api.GuidancePatch{Enabled: in.Enabled}
		if in.Scope != "" {
			p.Scope = &in.Scope
		}
		if in.Text != "" {
			p.Text = &in.Text
		}
		g, err = t.API.PutGuidance(ctx, in.ID, p)
	case "delete":
		if in.ID == "" {
			return result{}, toolErr("invalid", "delete needs id", "guidance_read lists the lines")
		}
		if err := t.API.DeleteGuidance(ctx, in.ID); err != nil {
			return result{}, t.apiErr(err, call{write: true, what: "guidance line " + in.ID, lister: "guidance_read"})
		}
		return result{out: map[string]any{"deleted": true}, text: "deleted guidance " + in.ID}, nil
	default:
		return result{}, toolErr("invalid", "operation must be add, edit or delete", "")
	}
	if err != nil {
		return result{}, t.apiErr(noLLMErr(err), call{write: true, what: "guidance line " + in.ID, lister: "guidance_read"})
	}
	return result{out: map[string]any{"guidance": guidanceOut(g)}, text: in.Operation + ": " + guidanceLine(g)}, nil
}

// ---- llm_status ---------------------------------------------------------

type llmStatusIn struct{}

const llmStatusDesc = `Show the LLM assistant's state: the mode as set and in force, whether chat is allowed, the endpoint host and models, the circuit breaker, the budget used today, and the last error.

Best for: why ask failed, or whether the LLM is on at all. It is optional and off by default.
Not for: changing it; use settings_write with llm.* keys (admin).`

func (t *tools) addLLMStatus(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "llm_status", Description: llmStatusDesc, Annotations: readOnly("LLM status")}, nil,
		map[string]*jsonschema.Schema{
			"mode": str("off, shadow, advise or enforce, as set."), "effective_mode": str("The mode in force."),
			"chat": boolean("ask works now."), "configured": boolean(""), "token_set": boolean(""),
			"circuit": str("closed or open"), "tokens_today": integer(""), "tokens_per_day": integer(""),
			"last_error": str(""),
		},
		t.llmStatus)
}

func (t *tools) llmStatus(ctx context.Context, _ *mcp.CallToolRequest, _ llmStatusIn) (result, error) {
	st, err := t.API.LLMStatus(ctx)
	if err != nil {
		return result{}, t.apiErr(noLLMErr(err), call{})
	}
	text := fmt.Sprintf("mode %s (in force: %s), chat %v, circuit %s, %d of %d tokens today",
		st.Mode, st.EffectiveMode, st.Chat, st.Circuit, st.TokensToday, st.TokensPerDay)
	if st.LastError != nil {
		text += "; last error: " + *st.LastError
	}
	return result{out: st, text: text}, nil
}

// ---- ask ------------------------------------------------------------------

type askIn struct {
	Question string `json:"question" jsonschema:"The question, in plain words."`
	Project  string `json:"project,omitempty" jsonschema:"Ask about this project."`
	Key      string `json:"key,omitempty" jsonschema:"Ask about one incident or job."`
	View     string `json:"view,omitempty" jsonschema:"Ask about a saved view, by name or id."`
	Since    string `json:"since,omitempty" jsonschema:"Ask about events since this time: RFC 3339, or a duration back from now such as 12h."`
}

const askDesc = `Ask heraldo's own LLM assistant a question about the stored events, such as "what happened overnight?" or "why does the backup check keep failing?".

Best for: a summary in the owner's words when the server's assistant is on (llm_status chat=true). It reads only, cites event ids, and may suggest one change (a filter, a view, a guidance line or a mute) that it never applies.
Returns: answer, cited_ids (read them with event_read) and an optional proposal.
Not for: anything you can read yourself; events_search and event_read are faster and cost nothing. Not for: changes; apply a proposal with views_write, guidance_write or mute only when the owner agrees.`

func (t *tools) addAsk(s *mcp.Server) {
	addTool(t, s, &mcp.Tool{Name: "ask", Description: askDesc,
		Annotations: &mcp.ToolAnnotations{Title: "Ask the assistant", ReadOnlyHint: true, DestructiveHint: &noAnnot,
			OpenWorldHint: ptr(true)}},
		func(is *jsonschema.Schema) {
			p := is.Properties
			maxLen(p["question"], 2000)
			p["question"].MinLength = ptr(1)
			maxLen(p["project"], 64)
			maxLen(p["key"], 255)
			maxLen(p["view"], 64)
		},
		map[string]*jsonschema.Schema{
			"answer": str(""), "cited_ids": arr("Event ids the answer rests on.", str("")),
			"proposal": anyObj("A suggested change: type filter, view, guidance or mute, and the body of its write call."),
		},
		t.ask)
}

func (t *tools) ask(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (result, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return result{}, toolErr("invalid", "give question", "")
	}
	ai := api.AskInput{Question: q}
	opt := func(s string) *string {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return &s
	}
	if in.Project != "" || in.Key != "" || in.View != "" || in.Since != "" {
		ai.Scope = &api.AskScope{Project: opt(in.Project), Key: opt(in.Key), View: opt(in.View), Since: opt(in.Since)}
	}
	a, err := t.API.Ask(ctx, ai)
	if client.IsStatus(err, http.StatusConflict) {
		msg := "the assistant is off"
		var ce *client.Error
		if errors.As(err, &ce) && ce.Message != "" {
			msg = ce.Message
		}
		return result{}, toolErr("llm_off", msg, "llm_status shows why; use events_search instead")
	}
	if err != nil {
		return result{}, t.apiErr(noLLMErr(err), call{})
	}
	text := a.Answer
	if len(a.CitedIds) > 0 {
		text += "\n\ncited: " + strings.Join(a.CitedIds, ", ")
	}
	out := map[string]any{"answer": a.Answer, "cited_ids": a.CitedIds}
	if a.Proposal != nil {
		out["proposal"] = a.Proposal
		s := ""
		if a.Proposal.Summary != nil {
			s = ": " + *a.Proposal.Summary
		}
		text += fmt.Sprintf("\n\nproposal (%s)%s. Apply it only when the owner agrees.", a.Proposal.Type, s)
	}
	return result{out: out, text: text}, nil
}
