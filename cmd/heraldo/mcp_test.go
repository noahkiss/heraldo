package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpRun runs heraldo mcp with args over pipes, sends the JSON-RPC lines in
// reqs, and returns the answers by id, its stderr and the app's stdout.
func (f *fixture) mcpRun(t *testing.T, reqs []string, args ...string) (byID map[int]map[string]any, stderr, stdout string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	old := mcpTransport
	mcpTransport = func() mcp.Transport { return &mcp.IOTransport{Reader: inR, Writer: outW} }
	t.Cleanup(func() { mcpTransport = old })
	var out, errb syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan int)
	go func() {
		done <- f.app(&out, &errb, "", false).run(ctx, append([]string{"mcp"}, args...))
		outW.Close()
	}()
	go func() {
		for _, r := range reqs {
			io.WriteString(inW, r+"\n")
		}
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 1<<20)
	want := 0
	for _, r := range reqs {
		if strings.Contains(r, `"id"`) {
			want++
		}
	}
	byID = map[int]map[string]any{}
	for len(byID) < want && sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC: %q", sc.Text())
		}
		if id, ok := m["id"].(float64); ok {
			byID[int(id)] = m
		}
	}
	inW.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	return byID, errb.String(), out.String()
}

const (
	mcpInit   = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex-mcp-client","version":"1"}}}`
	mcpInited = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	mcpList   = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
)

func toolNames(t *testing.T, m map[string]any) []string {
	t.Helper()
	var names []string
	for _, tl := range m["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	return names
}

func TestMCPStdio(t *testing.T) {
	f := newFixture(t)
	f.env["HERALDO_WRITE_TOKEN"] = writeTok
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send","arguments":{"title":"from codex","dedup":"t-1"}}}`
	lines, stderr, stdout := f.mcpRun(t, []string{mcpInit, mcpInited, mcpList, call})
	if stdout != "" {
		t.Fatalf("the app wrote to stdout: %q", stdout)
	}
	if n := len(toolNames(t, lines[2])); n != 16 {
		t.Fatalf("%d tools with a write token", n)
	}
	res := lines[3]["result"].(map[string]any)
	if res["isError"] == true || res["structuredContent"].(map[string]any)["source"] != "agent:codex" {
		t.Fatalf("send: %v", res)
	}
	for _, l := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if !json.Valid([]byte(l)) {
			t.Fatalf("stderr line is not JSON: %q", l)
		}
	}
	if strings.Contains(stderr, writeTok) || strings.Contains(stderr, readTok) {
		t.Fatal("a token reached the log")
	}

	lines, _, _ = f.mcpRun(t, []string{mcpInit, mcpInited, mcpList}, "--read-only")
	if n := len(toolNames(t, lines[2])); n != 10 {
		t.Fatalf("%d tools with --read-only", n)
	}
	if !strings.Contains(lines[1]["result"].(map[string]any)["instructions"].(string), "read-only") {
		t.Fatal("read-only instructions")
	}

	// An admin token adds settings_write; --read-only still drops it.
	f.env["HERALDO_ADMIN_TOKEN"] = adminTok
	lines, _, _ = f.mcpRun(t, []string{mcpInit, mcpInited, mcpList})
	if names := toolNames(t, lines[2]); len(names) != 17 || !slices.Contains(names, "settings_write") {
		t.Fatalf("tools with an admin token: %v", names)
	}
	lines, _, _ = f.mcpRun(t, []string{mcpInit, mcpInited, mcpList}, "--read-only")
	if n := len(toolNames(t, lines[2])); n != 10 {
		t.Fatalf("%d tools with --read-only and an admin token", n)
	}
}

func TestMCPNotConfigured(t *testing.T) {
	f := newFixture(t)
	delete(f.env, "HERALDO_TOKEN")
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"overview","arguments":{}}}`
	lines, _, _ := f.mcpRun(t, []string{mcpInit, mcpInited, mcpList, call})
	if names := toolNames(t, lines[2]); len(names) != 1 || names[0] != "overview" {
		t.Fatalf("tools: %v", names)
	}
	res := lines[3]["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] != true || !strings.Contains(text, "HERALDO_TOKEN is not set") || !strings.Contains(text, "heraldo/env") {
		t.Fatalf("overview: %v", res)
	}
}
