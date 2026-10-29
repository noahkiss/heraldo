package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/mcphttp"
	"github.com/noahkiss/heraldo/internal/settings"
)

const llmTestToken = "sk-test-llm-token-0000"

// newLLMEnv is an env whose LLM provider is f and whose settings come from
// vars.
func newLLMEnv(t *testing.T, f *llm.Fake, vars map[string]string, set func(*Options)) *env {
	t.Helper()
	e := newEnvWith(t, func(o *Options) {
		o.LLMProvider = f.Factory
		o.AskTools = mcphttp.NewToolbox("v-test")
		if set != nil {
			set(o)
		}
	})
	e.withSettings(t, settings.Options{Getenv: func(k string) string { return vars[k] }})
	return e
}

func llmOn(extra map[string]string) map[string]string {
	v := map[string]string{"HERALDO_LLM_MODE": "shadow", "HERALDO_LLM_CHAT": "true",
		"HERALDO_LLM_BASE_URL": "https://llm.example.net/v1", "HERALDO_LLM_CHAT_MODEL": "m", "HERALDO_LLM_TOKEN": llmTestToken}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

func TestGuidanceEndpoints(t *testing.T) {
	e := newEnv(t)
	if rec := e.do(t, http.MethodPost, "/v1/guidance", readTok, []byte(`{"scope":"global","text":"x"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("read token writes: %d", rec.Code)
	}
	for _, bad := range []string{`{}`, `{"scope":"global","text":""}`, `{"scope":"view:1","text":"x"}`,
		`{"scope":"key:(","text":"x"}`, `{"scope":"project:","text":"x"}`, `{"scope":"global","text":"x","level":"critical"}`,
		`{"scope":"global","text":"` + strings.Repeat("a", 1001) + `"}`} {
		if rec := e.do(t, http.MethodPost, "/v1/guidance", writeTok, []byte(bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%.60s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	rec := e.do(t, http.MethodPost, "/v1/guidance", writeTok, []byte(`{"scope":"key:^backup:","text":"Group backup alerts by host."}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	g := decode[api.Guidance](t, rec)
	if !strings.HasPrefix(g.Id, "gd_") || !g.Enabled || g.Version != 1 || g.CreatedBy != "sender" {
		t.Fatalf("guidance %+v", g)
	}
	e.do(t, http.MethodPost, "/v1/guidance", writeTok, []byte(`{"scope":"global","text":"Be brief.","enabled":false}`))
	list := decode[api.GuidanceList](t, e.do(t, http.MethodGet, "/v1/guidance", readTok, nil))
	if len(list.Guidance) != 2 {
		t.Fatalf("list %+v", list)
	}
	if l := decode[api.GuidanceList](t, e.do(t, http.MethodGet, "/v1/guidance?scope=global", readTok, nil)); len(l.Guidance) != 1 {
		t.Fatalf("by scope %+v", l)
	}
	rec = e.do(t, http.MethodPut, "/v1/guidance/"+g.Id, writeTok, []byte(`{"text":"Group backups by host and day."}`))
	put := decode[api.Guidance](t, rec)
	if rec.Code != http.StatusOK || put.Version != 2 || put.Scope != "key:^backup:" || put.Text != "Group backups by host and day." {
		t.Fatalf("put: %d %+v", rec.Code, put)
	}
	if rec := e.do(t, http.MethodPut, "/v1/guidance/"+g.Id, writeTok, []byte(`{"scope":"bad"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad put: %d", rec.Code)
	}
	match, err := e.st.MatchingGuidance(context.Background(), "", "", "backup:nas")
	if err != nil || len(match) != 1 || match[0].ID != g.Id {
		t.Fatalf("matching %+v %v", match, err)
	}
	if rec := e.do(t, http.MethodDelete, "/v1/guidance/"+g.Id, writeTok, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/guidance/"+g.Id, readTok, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
}

func TestLLMOffByDefault(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{{Content: "ok"}}}
	e := newLLMEnv(t, f, nil, nil)
	h := decode[api.Health](t, e.do(t, http.MethodGet, "/healthz", "", nil))
	if h.Llm == nil || *h.Llm != api.HealthLlmOff {
		t.Fatalf("health llm %v", h.Llm)
	}
	st := decode[api.LlmStatus](t, e.do(t, http.MethodGet, "/v1/llm/status", readTok, nil))
	if st.Mode != "off" || st.EffectiveMode != "off" || st.Chat || st.Configured {
		t.Fatalf("status %+v", st)
	}
	if rec := e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(`{"question":"hi"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("ask with llm off: %d %s", rec.Code, rec.Body)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("a request left the server")
	}
}

func TestLLMStatusAndTest(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{{Content: " ok ", TokensIn: 5, TokensOut: 1}}}
	e := newLLMEnv(t, f, llmOn(map[string]string{"HERALDO_LLM_MODE": "off"}), nil)
	if rec := e.do(t, http.MethodPost, "/v1/llm/test", writeTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("write token tests: %d", rec.Code)
	}
	rec := e.do(t, http.MethodPost, "/v1/llm/test", adminTok, nil)
	res := decode[api.LlmTest](t, rec)
	if !res.Ok || res.Answer == nil || *res.Answer != "ok" || res.CallId == nil || res.Model == nil || *res.Model != "m" {
		t.Fatalf("test: %s", rec.Body)
	}
	st := decode[api.LlmStatus](t, e.do(t, http.MethodGet, "/v1/llm/status", readTok, nil))
	if !st.Configured || !st.TokenSet || st.TokensToday != 6 || st.BaseHost == nil || *st.BaseHost != "llm.example.net" {
		t.Fatalf("status %+v", st)
	}
	raw := e.do(t, http.MethodGet, "/v1/llm/status", readTok, nil).Body.String()
	if strings.Contains(raw, llmTestToken) {
		t.Fatal("status returns the token")
	}
	// A failing provider: ok false, the error without the token.
	f2 := &llm.Fake{Err: errors.New("refused for " + llmTestToken)}
	e2 := newLLMEnv(t, f2, llmOn(nil), nil)
	rec = e2.do(t, http.MethodPost, "/v1/llm/test", adminTok, nil)
	if res := decode[api.LlmTest](t, rec); res.Ok || res.Error == nil || strings.Contains(rec.Body.String(), llmTestToken) {
		t.Fatalf("failing test: %s", rec.Body)
	}
}

func TestLLMOutageNotice(t *testing.T) {
	f := &llm.Fake{Err: errors.New("down")}
	e := newLLMEnv(t, f, llmOn(nil), nil)
	for i := 0; i < 4; i++ {
		e.do(t, http.MethodPost, "/v1/llm/test", adminTok, nil)
	}
	h := decode[api.Health](t, e.do(t, http.MethodGet, "/healthz", "", nil))
	if h.Llm == nil || *h.Llm != api.HealthLlmDegraded {
		t.Fatalf("health llm %v", h.Llm)
	}
	page := decode[api.EventPage](t, e.do(t, http.MethodGet, "/v1/events?source=heraldo", readTok, nil))
	if len(page.Events) != 1 || page.Events[0].Level == nil || *page.Events[0].Level != api.LevelPassive {
		t.Fatalf("notices: %+v", page.Events)
	}
}

func TestLLMStandbyIsOff(t *testing.T) {
	f := &llm.Fake{Replies: []llm.Response{{Content: "ok"}}}
	e := newLLMEnv(t, f, llmOn(map[string]string{"HERALDO_LLM_MODE": "enforce"}), func(o *Options) { o.Standby = true })
	st := decode[api.LlmStatus](t, e.do(t, http.MethodGet, "/v1/llm/status", readTok, nil))
	if st.EffectiveMode != "off" || st.Chat || st.Standby == nil || !*st.Standby {
		t.Fatalf("standby status %+v", st)
	}
	if rec := e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(`{"question":"hi"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("standby ask: %d", rec.Code)
	}
	if res := decode[api.LlmTest](t, e.do(t, http.MethodPost, "/v1/llm/test", adminTok, nil)); res.Ok {
		t.Fatal("standby test called the provider")
	}
	if len(f.Requests()) != 0 {
		t.Fatal("a standby called the LLM")
	}
}

func TestAsk(t *testing.T) {
	f := &llm.Fake{}
	e := newLLMEnv(t, f, llmOn(map[string]string{"HERALDO_LLM_EXCLUDE_PROJECTS": "private"}), nil)
	disk := decode[api.Stored](t, e.post(t, `{"source":"nas","kind":"incident","key":"disk:nas","action":"open","title":"Disk 91% full","project":"home","body":"token Bearer abcdefghijklmnop"}`))
	e.post(t, `{"source":"vault","title":"private thing","project":"private"}`)
	e.do(t, http.MethodPost, "/v1/guidance", writeTok, []byte(`{"scope":"project:home","text":"Mention the host."}`))
	f.Reply = func(req llm.Request) (llm.Response, error) {
		if len(req.Messages) == 2 {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "t1", Name: "event_read", Arguments: fmt.Sprintf(`{"id":%q}`, disk.Id)}}}, nil
		}
		ans := map[string]any{"answer": "The NAS disk is 91% full.", "cited_ids": []string{disk.Id, "01JINVENTEDINVENTEDINVENTE"},
			"proposal": map[string]any{"type": "mute", "summary": "Mute the disk alert for an hour", "mute": map[string]any{"key": "disk:nas", "for": "1h"}}}
		b, _ := json.Marshal(ans)
		return llm.Response{Content: string(b)}, nil
	}
	if rec := e.do(t, http.MethodPost, "/v1/ask", "", []byte(`{"question":"x"}`)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	for _, bad := range []string{`{}`, `{"question":""}`, `{"question":"x","scope":{"since":"soon"}}`, `{"question":"x","write":true}`} {
		if rec := e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	rec := e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(`{"question":"What is wrong at home?","scope":{"project":"home","since":"24h"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	a := decode[api.AskAnswer](t, rec)
	if a.Answer != "The NAS disk is 91% full." || len(a.CitedIds) != 1 || a.CitedIds[0] != disk.Id {
		t.Fatalf("answer %+v", a)
	}
	if a.Proposal == nil || a.Proposal.Type != api.AskProposalTypeMute || a.Proposal.Mute == nil || a.CallIds == nil || len(*a.CallIds) != 2 {
		t.Fatalf("proposal %s", rec.Body)
	}
	// Nothing was written: no mute.
	if ms := decode[api.MuteList](t, e.do(t, http.MethodGet, "/v1/mutes", writeTok, nil)); len(ms.Mutes) != 0 {
		t.Fatalf("chat wrote a mute: %+v", ms)
	}
	reqs := f.Requests()
	var sent strings.Builder
	for _, m := range reqs[len(reqs)-1].Messages {
		sent.WriteString(m.Content)
	}
	for _, bad := range []string{"private thing", "abcdefghijklmnop", llmTestToken} {
		if strings.Contains(sent.String(), bad) {
			t.Fatalf("%q reached the model", bad)
		}
	}
	if !strings.Contains(sent.String(), "Disk 91% full") || !strings.Contains(sent.String(), "Mention the host.") {
		t.Fatalf("context: %s", sent.String())
	}
	// A proposal that fails its write call's checks is dropped.
	f.Reply = func(llm.Request) (llm.Response, error) {
		return llm.Response{Content: `{"answer":"ok","cited_ids":[],"proposal":{"type":"mute","mute":{"for":"1h"}}}`}, nil
	}
	if a := decode[api.AskAnswer](t, e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(`{"question":"y"}`))); a.Proposal != nil {
		t.Fatalf("bad proposal kept: %+v", a.Proposal)
	}
	// A provider failure is a 503 with the cause, never the token.
	f.Reply = func(llm.Request) (llm.Response, error) { return llm.Response{}, errors.New("down " + llmTestToken) }
	rec = e.do(t, http.MethodPost, "/v1/ask", readTok, []byte(`{"question":"z"}`))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), llmTestToken) {
		t.Fatalf("failure: %d %s", rec.Code, rec.Body)
	}
}
