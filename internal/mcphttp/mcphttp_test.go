package mcphttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/mcphttp"
	"github.com/noahkiss/heraldo/internal/mcptools"
	"github.com/noahkiss/heraldo/internal/server"
	"github.com/noahkiss/heraldo/internal/store"
	"github.com/noahkiss/heraldo/internal/validate"
)

const (
	readTok  = "http-mcp-read-token-for-tests"
	writeTok = "http-mcp-write-token-for-tests"
	adminTok = "http-mcp-admin-token-for-tests"
	keyTok   = "http-mcp-notify-key-token-for-tests"
	teeTok   = "http-mcp-bridge-token-for-tests"
)

func newServer(t *testing.T, on bool) string {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokens, err := auth.Parse(strings.NewReader(fmt.Sprintf(
		"reader read %s\ncodex write %s\nops admin %s\nsender write %s notify-key\nbridge write %s trusted-ingest\n",
		auth.Hash(readTok), auth.Hash(writeTok), auth.Hash(adminTok), auth.Hash(keyTok), auth.Hash(teeTok))), '\n')
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	o := server.Options{Store: st, Tokens: tokens, Validator: v, Done: done, Version: "test",
		Logger: slog.New(slog.DiscardHandler)}
	if on {
		o.MCP = mcphttp.New(mcphttp.Options{Version: "test"})
	}
	srv := httptest.NewServer(server.New(o))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv.URL
}

// bearer adds a bearer token to every request.
type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, base, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: base + "/v1/mcp", HTTPClient: &http.Client{Transport: bearer{tok}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func names(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, tl := range lt.Tools {
		n = append(n, tl.Name)
	}
	slices.Sort(n)
	return n
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) (map[string]any, string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	b, _ := json.Marshal(res.StructuredContent)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	for _, tok := range []string{readTok, writeTok, adminTok, keyTok, teeTok} {
		if strings.Contains(string(b)+text, tok) {
			t.Fatalf("%s: a token leaked", name)
		}
	}
	return out, text, res.IsError
}

func status(t *testing.T, method, url, tok, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`

func TestOffByDefault(t *testing.T) {
	base := newServer(t, false)
	if code := status(t, http.MethodPost, base+"/v1/mcp", readTok, initBody); code != http.StatusNotFound {
		t.Fatalf("off: %d", code)
	}
}

func TestAuth(t *testing.T) {
	base := newServer(t, true)
	for _, tok := range []string{"", "not-a-token"} {
		if code := status(t, http.MethodPost, base+"/v1/mcp", tok, initBody); code != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, code)
		}
	}
	if code := status(t, http.MethodPost, base+"/v1/mcp", readTok, initBody); code != http.StatusOK {
		t.Fatalf("read token: %d", code)
	}
	// A sender's credential is refused: a notify-key token must never gain
	// read access, and a bridge token is no agent's.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"events_search","arguments":{}}}`
	for _, tok := range []string{keyTok, teeTok} {
		for _, body := range []string{initBody, call} {
			if code := status(t, http.MethodPost, base+"/v1/mcp", tok, body); code != http.StatusForbidden {
				t.Fatalf("sender token %s: %d", tok, code)
			}
		}
	}
	// Through REST the notify-key token still cannot read.
	if code := status(t, http.MethodGet, base+"/v1/events", keyTok, ""); code != http.StatusForbidden {
		t.Fatalf("notify-key token on GET /v1/events: %d", code)
	}
	// The MCP grant does not reach the REST API: a write token still cannot
	// read there.
	if code := status(t, http.MethodGet, base+"/v1/events", writeTok, ""); code != http.StatusForbidden {
		t.Fatalf("write token on GET /v1/events: %d", code)
	}
}

func TestToolsByScope(t *testing.T) {
	base := newServer(t, true)
	read := slices.Sorted(slices.Values(mcptools.ReadTools))
	all := slices.Sorted(slices.Values(slices.Concat(mcptools.ReadTools, mcptools.WriteTools)))

	ro := connect(t, base, readTok)
	if got := names(t, ro); !slices.Equal(got, read) {
		t.Fatalf("read token: %v", got)
	}
	out, text, isErr := call(t, ro, "overview", map[string]any{})
	if isErr || out["server"].(map[string]any)["write"] != false || !strings.Contains(text, "read-only") {
		t.Fatalf("overview, read token: %v %s", out, text)
	}

	if got := names(t, connect(t, base, writeTok)); !slices.Equal(got, all) {
		t.Fatalf("write token: %v", got)
	}
	admin := slices.Sorted(slices.Values(slices.Concat(mcptools.ReadTools, mcptools.WriteTools, mcptools.AdminTools)))
	if got := names(t, connect(t, base, adminTok)); !slices.Equal(got, admin) {
		t.Fatalf("admin token: %v", got)
	}
	rw := connect(t, base, writeTok)
	// A write token reads through MCP: overview, events_search.
	out, text, isErr = call(t, rw, "send", map[string]any{"title": "from http", "dedup": "http-1", "level": "passive"})
	if isErr || out["source"] != "agent:codex" {
		t.Fatalf("send: %v %s", out, text)
	}
	id := out["id"].(string)
	out, text, isErr = call(t, rw, "events_search", map[string]any{"q": "from http"})
	if isErr || !strings.Contains(text, id) {
		t.Fatalf("events_search, write token: %v %s", out, text)
	}
	out, text, isErr = call(t, rw, "overview", map[string]any{})
	if isErr || out["mutes"] == nil {
		t.Fatalf("overview, write token: %v %s", out, text)
	}
	out, text, isErr = call(t, rw, "event_read", map[string]any{"id": id})
	if isErr || !strings.Contains(text, "from http") {
		t.Fatalf("event_read: %v %s", out, text)
	}
}
