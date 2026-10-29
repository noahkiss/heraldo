package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

type fakeTools struct {
	calls []string
}

func (f *fakeTools) Tools(context.Context) ([]ToolDef, error) {
	return []ToolDef{{Name: "events_search"}, {Name: "event_read"}, {Name: "send"}}, nil
}

func (f *fakeTools) Call(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	f.calls = append(f.calls, name+" "+string(args))
	switch name {
	case "events_search":
		return json.RawMessage(`{"events":[
			{"id":"01JAAAAAAAAAAAAAAAAAAAAAAA","project":"web","title":"disk full","received_at":"2026-10-29T11:00:00Z"},
			{"id":"01JOLDOLDOLDOLDOLDOLDOLDOL","project":"web","title":"old","received_at":"2026-10-01T00:00:00Z"},
			{"id":"01JBBBBBBBBBBBBBBBBBBBBBBB","project":"private","title":"secret thing"}]}`), nil
	case "event_read":
		return json.RawMessage(`{"id":"01JCCCCCCCCCCCCCCCCCCCCCCC","title":"backup","body":"Bearer abcdefghijklmnopq"}`), nil
	}
	return nil, errors.New("unexpected")
}

func TestAsk(t *testing.T) {
	f := &Fake{}
	f.Replies = []Response{
		{ToolCalls: []ToolCall{{ID: "1", Name: "event_read", Arguments: `{"id":"01JCCCCCCCCCCCCCCCCCCCCCCC"}`},
			{ID: "2", Name: "send", Arguments: `{}`}}},
		{Content: `{"answer":"The disk filled.","cited_ids":["01JAAAAAAAAAAAAAAAAAAAAAAA","01JCCCCCCCCCCCCCCCCCCCCCCC","01JINVENTEDINVENTEDINVENTE","01JBBBBBBBBBBBBBBBBBBBBBBB"],` +
			`"proposal":{"type":"mute","mute":{"key":"disk","for":"1h"}}}`},
	}
	c, _, _ := newClient(t, on(env{"HERALDO_LLM_EXCLUDE_PROJECTS": "private", "HERALDO_LLM_SEND_BODY_CHARS": "100"}), f, openStore(t))
	tb := &fakeTools{}
	ans, err := c.Ask(context.Background(), tb, Question{Text: "what broke?",
		Scope:    Scope{Project: "web", Since: mustTime("2026-10-28T00:00:00Z")},
		Guidance: []store.Guidance{{ID: "gd_1", Version: 2, Scope: "global", Text: "Be brief."}}})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "The disk filled." {
		t.Fatalf("answer %q", ans.Text)
	}
	if strings.Join(ans.CitedIDs, ",") != "01JAAAAAAAAAAAAAAAAAAAAAAA,01JCCCCCCCCCCCCCCCCCCCCCCC" {
		t.Fatalf("cited %v", ans.CitedIDs)
	}
	if !strings.Contains(string(ans.Proposal), `"mute"`) || len(ans.CallIDs) != 2 {
		t.Fatalf("proposal %s calls %v", ans.Proposal, ans.CallIDs)
	}
	reqs := f.Requests()
	first := reqs[0]
	if len(first.Tools) != 2 {
		t.Fatalf("tools offered: %v", first.Tools)
	}
	sent := ""
	for _, m := range reqs[1].Messages {
		sent += m.Content + "\n"
	}
	for _, bad := range []string{"secret thing", "abcdefghijklmnopq", "01JOLDOLDOLDOLDOLDOLDOLDOL"} {
		if strings.Contains(sent, bad) {
			t.Fatalf("%q reached the model", bad)
		}
	}
	if !strings.Contains(sent, "Be brief.") || !strings.Contains(sent, "no such tool") {
		t.Fatalf("prompt: %s", sent)
	}
	if !strings.Contains(tb.calls[0], `"project":["web"]`) {
		t.Fatalf("preload %v", tb.calls)
	}
	for _, c := range tb.calls {
		if strings.HasPrefix(c, "send") {
			t.Fatal("a write tool ran")
		}
	}
}

func TestAskPlainTextAndGates(t *testing.T) {
	f := &Fake{Replies: []Response{{Content: "Nothing failed; see 01JAAAAAAAAAAAAAAAAAAAAAAA."}}}
	c, _, _ := newClient(t, on(nil), f, nil)
	ans, err := c.Ask(context.Background(), &fakeTools{}, Question{Text: "anything?"})
	if err != nil || !strings.HasPrefix(ans.Text, "Nothing failed") || len(ans.CitedIDs) != 1 {
		t.Fatalf("%+v %v", ans, err)
	}
	off, _, _ := newClient(t, on(env{"HERALDO_LLM_CHAT": "false"}), f, nil)
	if _, err := off.Ask(context.Background(), &fakeTools{}, Question{Text: "x"}); !errors.Is(err, ErrChatOff) {
		t.Fatalf("chat off: %v", err)
	}
	ex, _, _ := newClient(t, on(env{"HERALDO_LLM_EXCLUDE_PROJECTS": "private"}), f, nil)
	n := len(f.Requests())
	if a, err := ex.Ask(context.Background(), &fakeTools{}, Question{Text: "x", Scope: Scope{Project: "Private"}}); err != nil ||
		!strings.Contains(a.Text, "excluded") || len(f.Requests()) != n {
		t.Fatalf("excluded project: %+v %v", a, err)
	}
}

func TestAskStopsAfterMaxRounds(t *testing.T) {
	f := &Fake{Reply: func(req Request) (Response, error) {
		if len(req.Tools) == 0 {
			return Response{Content: `{"answer":"done","cited_ids":[]}`}, nil
		}
		return Response{ToolCalls: []ToolCall{{ID: "x", Name: "events_search", Arguments: `{}`}}}, nil
	}}
	c, _, _ := newClient(t, on(nil), f, nil)
	ans, err := c.Ask(context.Background(), &fakeTools{}, Question{Text: "loop"})
	if err != nil || ans.Text != "done" || len(f.Requests()) != MaxRounds+1 {
		t.Fatalf("%+v %v, %d requests", ans, err, len(f.Requests()))
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
