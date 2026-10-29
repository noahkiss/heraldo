package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestGuidanceMatching(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for i, g := range []Guidance{
		{ID: "gd_4", Scope: "key:^disk:", Text: "k", Enabled: true},
		{ID: "gd_3", Scope: "project:home", Text: "p", Enabled: true},
		{ID: "gd_2", Scope: "source:nas", Text: "s", Enabled: true},
		{ID: "gd_1", Scope: "global", Text: "g", Enabled: true},
		{ID: "gd_5", Scope: "global", Text: "off", Enabled: false},
		{ID: "gd_6", Scope: "project:work", Text: "w", Enabled: true},
	} {
		g.CreatedBy, g.CreatedAt, g.UpdatedAt = "t", at.Add(time.Duration(i)*time.Second), at
		if err := s.AddGuidance(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.MatchingGuidance(ctx, "Home", "nas", "disk:sda")
	if err != nil {
		t.Fatal(err)
	}
	var order string
	for _, g := range got {
		order += g.Text
	}
	if order != "gspk" {
		t.Fatalf("order %q", order)
	}
	if got, _ := s.MatchingGuidance(ctx, "", "", ""); len(got) != 1 || got[0].ID != "gd_1" {
		t.Fatalf("no event fields: %+v", got)
	}
	g, err := s.PutGuidance(ctx, Guidance{ID: "gd_1", Scope: "global", Text: "g2", Enabled: true, UpdatedAt: at})
	if err != nil || g.Version != 2 || g.Text != "g2" {
		t.Fatalf("put %+v %v", g, err)
	}
}

func TestLLMCallCache(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, c := range []LLMCall{
		{ID: "llm_1", At: at, Status: LLMOK, InputHash: "h", Output: json.RawMessage(`{"a":1}`), TokensIn: 3, TokensOut: 1},
		{ID: "llm_2", At: at.Add(time.Minute), Status: LLMError, InputHash: "h", Error: "x", TokensIn: 2},
	} {
		c.Role, c.Model, c.BaseHost, c.PromptVersion = "decisions", "m", "llm.example.net", "p"
		if err := s.AddLLMCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	hit, err := s.CachedLLMCall(ctx, "h", at.Add(-time.Hour))
	if err != nil || hit.ID != "llm_1" || string(hit.Output) != `{"a":1}` {
		t.Fatalf("hit %+v %v", hit, err)
	}
	if _, err := s.CachedLLMCall(ctx, "h", at.Add(time.Second)); err != ErrNotFound {
		t.Fatalf("stale: %v", err)
	}
	if n, _ := s.LLMTokensSince(ctx, at.Add(-time.Hour)); n != 6 {
		t.Fatalf("tokens %d", n)
	}
	if n, _ := s.PruneLLMCalls(ctx, at.Add(30*time.Second)); n != 1 {
		t.Fatalf("pruned %d", n)
	}
}
