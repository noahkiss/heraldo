package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/ids"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/store"
)

// MaxGuidanceText caps one guidance line.
const MaxGuidanceText = 1000

func (s *server) llmRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/guidance", s.auth(auth.ScopeRead, false, s.listGuidance))
	mux.HandleFunc("POST /v1/guidance", s.auth(auth.ScopeWrite, false, s.createGuidance))
	mux.HandleFunc("GET /v1/guidance/{id}", s.auth(auth.ScopeRead, false, s.getGuidance))
	mux.HandleFunc("PUT /v1/guidance/{id}", s.auth(auth.ScopeWrite, false, s.putGuidance))
	mux.HandleFunc("DELETE /v1/guidance/{id}", s.auth(auth.ScopeWrite, false, s.deleteGuidance))
	mux.HandleFunc("GET /v1/llm/status", s.auth(auth.ScopeRead, false, s.llmStatus))
	mux.HandleFunc("POST /v1/llm/test", s.auth(auth.ScopeAdmin, false, s.llmTest))
	mux.HandleFunc("POST /v1/ask", s.auth(auth.ScopeRead, false, s.ask))
}

// newLLM builds the LLM client over the settings and the call log. Its
// outage notice is a heraldo event, stored as the server's own.
func (s *server) newLLM() *llm.Client {
	cfg := llm.Config{
		Settings: s.Settings, Standby: s.Standby, Now: s.Now, Logger: s.Logger.With("component", "llm"),
		NewProvider: s.LLMProvider, Notify: s.emit,
	}
	if s.Store != nil {
		cfg.Log = s.Store
	}
	return llm.New(cfg)
}

// LLM is the server's LLM client, for the decisions role and tests.
func (srv *Server) LLM() *llm.Client { return srv.s.llm }

// GuidanceJSON is a guidance line as the API serves it.
func GuidanceJSON(g store.Guidance) api.Guidance {
	return api.Guidance{Id: g.ID, Scope: g.Scope, Text: g.Text, Enabled: g.Enabled, Version: g.Version,
		CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
}

// guidanceProblems checks a scope and a text.
func guidanceProblems(scope, text string) []string {
	var problems []string
	if _, _, err := store.ParseGuidanceScope(scope); err != nil {
		problems = append(problems, err.Error())
	}
	switch n := utf8.RuneCountInString(strings.TrimSpace(text)); {
	case n == 0:
		problems = append(problems, "text: must not be empty")
	case n > MaxGuidanceText:
		problems = append(problems, fmt.Sprintf("text: at most %d characters", MaxGuidanceText))
	}
	return problems
}

func decodeStrict(w http.ResponseWriter, r *http.Request, what string, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "want a %s: %v", what, err)
		return false
	}
	return true
}

func (s *server) guidanceErr(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no guidance line with that id")
		return
	}
	s.Logger.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// listGuidance is GET /v1/guidance.
func (s *server) listGuidance(w http.ResponseWriter, r *http.Request) {
	gs, err := s.Store.AllGuidance(r.Context(), r.URL.Query().Get("scope"))
	if err != nil {
		s.guidanceErr(w, "guidance", err)
		return
	}
	out := api.GuidanceList{Guidance: make([]api.Guidance, 0, len(gs))}
	for _, g := range gs {
		out.Guidance = append(out.Guidance, GuidanceJSON(g))
	}
	writeJSON(w, http.StatusOK, out)
}

// createGuidance is POST /v1/guidance.
func (s *server) createGuidance(w http.ResponseWriter, r *http.Request) {
	var in api.GuidanceInput
	if !decodeStrict(w, r, "GuidanceInput", &in) {
		return
	}
	in.Scope, in.Text = strings.TrimSpace(in.Scope), strings.TrimSpace(in.Text)
	if ps := guidanceProblems(in.Scope, in.Text); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, "%s", strings.Join(ps, "; "))
		return
	}
	now := s.Now().UTC()
	g := store.Guidance{ID: "gd_" + ids.New(now), Scope: in.Scope, Text: in.Text, Enabled: in.Enabled == nil || *in.Enabled,
		Version: 1, CreatedBy: tokenFrom(r).Name, CreatedAt: now, UpdatedAt: now}
	if err := s.Store.AddGuidance(r.Context(), g); err != nil {
		s.guidanceErr(w, "add guidance", err)
		return
	}
	s.Logger.Info("guidance", "id", g.ID, "scope", g.Scope, "token", g.CreatedBy)
	writeJSON(w, http.StatusCreated, GuidanceJSON(g))
}

// getGuidance is GET /v1/guidance/{id}.
func (s *server) getGuidance(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGuidance(r.Context(), r.PathValue("id"))
	if err != nil {
		s.guidanceErr(w, "guidance", err)
		return
	}
	writeJSON(w, http.StatusOK, GuidanceJSON(g))
}

// putGuidance is PUT /v1/guidance/{id}: the fields given replace the
// stored ones, and the version goes up.
func (s *server) putGuidance(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GetGuidance(r.Context(), r.PathValue("id"))
	if err != nil {
		s.guidanceErr(w, "guidance", err)
		return
	}
	var in api.GuidancePatch
	if !decodeStrict(w, r, "GuidancePatch", &in) {
		return
	}
	if in.Scope != nil {
		g.Scope = strings.TrimSpace(*in.Scope)
	}
	if in.Text != nil {
		g.Text = strings.TrimSpace(*in.Text)
	}
	if in.Enabled != nil {
		g.Enabled = *in.Enabled
	}
	if ps := guidanceProblems(g.Scope, g.Text); len(ps) > 0 {
		writeError(w, http.StatusBadRequest, "%s", strings.Join(ps, "; "))
		return
	}
	g.UpdatedAt = s.Now().UTC()
	g, err = s.Store.PutGuidance(r.Context(), g)
	if err != nil {
		s.guidanceErr(w, "put guidance", err)
		return
	}
	s.Logger.Info("guidance updated", "id", g.ID, "scope", g.Scope, "version", g.Version, "token", tokenFrom(r).Name)
	writeJSON(w, http.StatusOK, GuidanceJSON(g))
}

// deleteGuidance is DELETE /v1/guidance/{id}.
func (s *server) deleteGuidance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteGuidance(r.Context(), id); err != nil {
		s.guidanceErr(w, "delete guidance", err)
		return
	}
	s.Logger.Info("guidance deleted", "id", id, "token", tokenFrom(r).Name)
	w.WriteHeader(http.StatusNoContent)
}

// llmStatus is GET /v1/llm/status.
func (s *server) llmStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.llm.Status(r.Context()))
}

// llmTest is POST /v1/llm/test: one trivial question, with llm.mode off
// too. It answers 200 with ok false when the provider fails.
func (s *server) llmTest(w http.ResponseWriter, r *http.Request) {
	p := llm.LoadPrompt("test")
	temp := 0.0
	res, err := s.llm.Call(r.Context(), llm.Call{Role: llm.RoleTest, Prompt: p, Force: true, Input: "test",
		Request: llm.Request{Messages: []llm.Message{{Role: "user", Content: p.Text}}, Temperature: &temp, MaxTokens: 16}})
	out := api.LlmTest{Ok: err == nil}
	if res.Model != "" {
		out.Model = &res.Model
	}
	if res.CallID != "" {
		out.CallId = &res.CallID
	}
	if res.Latency > 0 {
		ms := res.Latency.Milliseconds()
		out.LatencyMs = &ms
	}
	if err != nil {
		msg := err.Error()
		out.Error = &msg
	} else {
		a := strings.TrimSpace(res.Content)
		if utf8.RuneCountInString(a) > 200 {
			a = string([]rune(a)[:200])
		}
		out.Answer = &a
	}
	s.Logger.Info("llm test", "ok", out.Ok, "host", s.llm.BaseHost(), "token", tokenFrom(r).Name)
	writeJSON(w, http.StatusOK, out)
}

// parseSince reads an RFC 3339 time or a duration back from now.
func parseSince(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Time{}, errors.New("scope.since: want an RFC 3339 time or a duration such as 12h")
	}
	return now.Add(-d), nil
}

// ask is POST /v1/ask: a question to the chat model, which reads through
// the read-only tools. It writes nothing.
func (s *server) ask(w http.ResponseWriter, r *http.Request) {
	var in api.AskInput
	if !decodeStrict(w, r, "AskInput", &in) {
		return
	}
	q := llm.Question{Text: strings.TrimSpace(in.Question)}
	if q.Text == "" || utf8.RuneCountInString(q.Text) > 2000 {
		writeError(w, http.StatusBadRequest, "question: want 1 to 2000 characters")
		return
	}
	if sc := in.Scope; sc != nil {
		q.Scope.View, q.Scope.Project, q.Scope.Key = trimDeref(sc.View), trimDeref(sc.Project), trimDeref(sc.Key)
		t, err := parseSince(trimDeref(sc.Since), s.Now())
		if err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		q.Scope.Since = t
	}
	if !s.llm.ChatAllowed() {
		writeError(w, http.StatusConflict, "%v", llm.ErrChatOff)
		return
	}
	if s.askTools == nil {
		writeError(w, http.StatusServiceUnavailable, "chat has no tools on this server")
		return
	}
	gs, err := s.Store.MatchingGuidance(r.Context(), q.Scope.Project, "", q.Scope.Key)
	if err != nil {
		s.Logger.Error("ask: guidance", "err", err)
	}
	q.Guidance = gs
	// The model may take several rounds, each up to llm.chat_timeout:
	// lift the server's write timeout for this response.
	_ = http.NewResponseController(w).SetWriteDeadline(s.Now().Add(
		s.llm.Timeout(llm.RoleChat)*(llm.MaxRounds+1) + 30*time.Second))
	a, err := s.llm.Ask(r.Context(), s.askTools, q)
	if err != nil {
		switch {
		case errors.Is(err, llm.ErrChatOff), errors.Is(err, llm.ErrStandby), errors.Is(err, llm.ErrNotConfigured):
			writeError(w, http.StatusConflict, "%v", err)
		default:
			writeError(w, http.StatusServiceUnavailable, "the assistant did not answer: %v", err)
		}
		return
	}
	out := api.AskAnswer{Answer: a.Text, CitedIds: a.CitedIDs}
	if out.CitedIds == nil {
		out.CitedIds = []string{}
	}
	if len(a.CallIDs) > 0 {
		out.CallIds = &a.CallIDs
	}
	if a.Model != "" {
		out.Model = &a.Model
	}
	if p, why := s.checkProposal(a.Proposal); p != nil {
		out.Proposal = p
	} else if why != "" {
		s.Logger.Info("ask: proposal dropped", "why", why)
	}
	s.Logger.Info("ask", "token", tokenFrom(r).Name, "cited", len(out.CitedIds), "proposal", out.Proposal != nil,
		"calls", len(a.CallIDs))
	writeJSON(w, http.StatusOK, out)
}

func trimDeref(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// checkProposal returns the model's proposal when it passes the checks of
// the write call that would apply it, with only the field its type names.
// Otherwise nil, and why.
func (s *server) checkProposal(raw json.RawMessage) (*api.AskProposal, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var p api.AskProposal
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, "not an AskProposal: " + err.Error()
	}
	out := &api.AskProposal{Type: p.Type, Summary: p.Summary}
	var problems []string
	switch p.Type {
	case api.AskProposalTypeFilter:
		if p.Filter == nil {
			return nil, "filter missing"
		}
		_, problems = viewFilterFromInput(*p.Filter)
		out.Filter = p.Filter
	case api.AskProposalTypeView:
		if p.View == nil {
			return nil, "view missing"
		}
		_, problems = viewFromInput(*p.View)
		out.View = p.View
	case api.AskProposalTypeGuidance:
		if p.Guidance == nil {
			return nil, "guidance missing"
		}
		problems = guidanceProblems(p.Guidance.Scope, p.Guidance.Text)
		out.Guidance = p.Guidance
	case api.AskProposalTypeMute:
		if p.Mute == nil {
			return nil, "mute missing"
		}
		_, problems = s.muteFromInput(*p.Mute, s.Now().UTC())
		out.Mute = p.Mute
	default:
		return nil, fmt.Sprintf("type %q is not filter, view, guidance or mute", p.Type)
	}
	if len(problems) > 0 {
		return nil, strings.Join(problems, "; ")
	}
	return out, ""
}
