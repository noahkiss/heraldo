package main

import (
	"context"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/mcphttp"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/settings"
)

func newLLMFixture(t *testing.T, f *llm.Fake, vars map[string]string) *fixture {
	t.Helper()
	m, err := settings.New(context.Background(), settings.Options{Getenv: func(k string) string { return vars[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWith(t, func(o *server.Options) {
		o.Settings, o.LLMProvider, o.AskTools = m, f.Factory, mcphttp.NewToolbox("test")
	})
}

func TestGuidanceCLI(t *testing.T) {
	f := newFixture(t)
	if r := f.run("guidance", "add", "global", "Be brief."); r.code != 1 || !strings.Contains(r.stderr, "HERALDO_WRITE_TOKEN") {
		t.Fatalf("no write token: %+v", r)
	}
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	if r := f.run("guidance"); r.code != 0 || !strings.Contains(r.stderr, "no guidance") {
		t.Fatalf("empty: %+v", r)
	}
	r := f.run("guidance", "add", "project:home", "Mention", "the", "host.")
	if r.code != 0 {
		t.Fatalf("add: %+v", r)
	}
	id := strings.TrimSpace(strings.Split(strings.TrimSpace(r.stdout), "\n")[1])
	if !strings.HasPrefix(id, "gd_") {
		t.Fatalf("id %q", id)
	}
	if r := f.run("guidance", "add", "view:x", "nope"); r.code != 1 || !strings.Contains(r.stderr, "scope") {
		t.Fatalf("bad scope: %+v", r)
	}
	if r := f.run("guidance", "ls"); r.code != 0 || !strings.Contains(r.stdout, "Mention the host.") || !strings.Contains(r.stdout, "project:home") {
		t.Fatalf("ls: %+v", r)
	}
	if r := f.run("guidance", "edit", "--disable", id); r.code != 0 || !strings.Contains(r.stdout, "version 2") {
		t.Fatalf("edit: %+v", r)
	}
	if r := f.run("guidance", "rm", id); r.code != 0 {
		t.Fatalf("rm: %+v", r)
	}
	if r := f.run("guidance", "--json"); r.code != 0 || r.stdout != "" {
		t.Fatalf("after rm: %+v", r)
	}
}

func TestLLMAndAskCLI(t *testing.T) {
	fk := &llm.Fake{Reply: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) == 0 && req.Schema == nil {
			return llm.Response{Content: "ok"}, nil
		}
		return llm.Response{Content: `{"answer":"All quiet.","cited_ids":[],"proposal":{"type":"guidance","summary":"Keep it short","guidance":{"scope":"global","text":"Keep it short."}}}`}, nil
	}}
	f := newLLMFixture(t, fk, map[string]string{"HERALDO_LLM_MODE": "shadow", "HERALDO_LLM_CHAT": "true",
		"HERALDO_LLM_BASE_URL": "https://llm.example.net/v1", "HERALDO_LLM_CHAT_MODEL": "m", "HERALDO_LLM_TOKEN": "sk-x-secret"})
	r := f.run("llm", "status")
	if r.code != 0 || !strings.Contains(r.stdout, "mode       shadow") || !strings.Contains(r.stdout, "llm.example.net") ||
		strings.Contains(r.stdout, "sk-x-secret") {
		t.Fatalf("status: %+v", r)
	}
	if r := f.run("llm", "test"); r.code != 1 {
		t.Fatalf("test with a read token: %+v", r)
	}
	f.env["HERALDO_ADMIN_TOKEN"] = adminTok
	if r := f.run("llm", "test"); r.code != 0 || !strings.Contains(r.stdout, `ok: m answered "ok"`) {
		t.Fatalf("test: %+v", r)
	}
	r = f.run("ask", "--project", "home", "anything", "wrong?")
	if r.code != 0 || !strings.Contains(r.stdout, "All quiet.") || !strings.Contains(r.stdout, `heraldo guidance add "global" "Keep it short."`) {
		t.Fatalf("ask: %+v", r)
	}
}

func TestAskOff(t *testing.T) {
	f := newFixture(t)
	if r := f.run("ask", "hi"); r.code != 1 || !strings.Contains(r.stderr, "chat is off") {
		t.Fatalf("ask with llm off: %+v", r)
	}
	if r := f.run("llm"); r.code != 0 || !strings.Contains(r.stdout, "mode       off") {
		t.Fatalf("llm: %+v", r)
	}
}
