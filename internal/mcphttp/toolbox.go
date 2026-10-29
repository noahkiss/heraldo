package mcphttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/auth"
	"github.com/noahkiss/heraldo/internal/client"
	"github.com/noahkiss/heraldo/internal/llm"
	"github.com/noahkiss/heraldo/internal/mcptools"
)

// ChatGrant is the token the chat model's tools run under: read scope, so
// every call is checked as a read token's would be, and no write tool is
// registered.
var ChatGrant = auth.Token{Name: "llm-chat", Scope: auth.ScopeRead}

// NewToolbox returns the read-only tools the chat model may call
// (llm.ChatTools): the MCP tools, served in process over an in-memory
// session, calling the REST API under ChatGrant. It is the factory
// server.Options.AskTools takes.
func NewToolbox(version string) func(api http.Handler) llm.Toolbox {
	return func(api http.Handler) llm.Toolbox {
		c := &client.Client{
			BaseURL: inProcessURL, Token: "in-process",
			HTTP:      &http.Client{Transport: &inProcess{api: api, grant: ChatGrant}},
			UserAgent: "heraldo-llm-chat/" + version,
		}
		srv := mcptools.New(mcptools.Options{API: c, URLHost: "in-process", Version: version, WriteOff: "the chat model"})
		return &toolbox{srv: srv, version: version}
	}
}

type toolbox struct {
	srv     *mcp.Server
	version string

	mu sync.Mutex
	cs *mcp.ClientSession
}

func (t *toolbox) session(ctx context.Context) (*mcp.ClientSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cs != nil {
		return t.cs, nil
	}
	st, ct := mcp.NewInMemoryTransports()
	// The sessions outlive one request: they run until the process ends.
	bg := context.WithoutCancel(ctx)
	if _, err := t.srv.Connect(bg, st, nil); err != nil {
		return nil, err
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "heraldo-llm-chat", Version: t.version}, nil)
	cs, err := cl.Connect(bg, ct, nil)
	if err != nil {
		return nil, err
	}
	t.cs = cs
	return cs, nil
}

func (t *toolbox) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cs != nil {
		_ = t.cs.Close()
		t.cs = nil
	}
}

// Tools lists llm.ChatTools with their descriptions and input schemas.
func (t *toolbox) Tools(ctx context.Context) ([]llm.ToolDef, error) {
	cs, err := t.session(ctx)
	if err != nil {
		return nil, err
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.reset()
		return nil, err
	}
	var out []llm.ToolDef
	for _, tool := range res.Tools {
		if !slices.Contains(llm.ChatTools, tool.Name) {
			continue
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, llm.ToolDef{Name: tool.Name, Description: tool.Description, Parameters: schema})
	}
	return out, nil
}

// Call runs one of llm.ChatTools and returns its structured result.
func (t *toolbox) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if !slices.Contains(llm.ChatTools, name) {
		return nil, errors.New("no such tool: " + name)
	}
	cs, err := t.session(ctx)
	if err != nil {
		return nil, err
	}
	var a map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, errors.New("the arguments are not a JSON object")
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		if ctx.Err() == nil {
			t.reset()
		}
		return nil, err
	}
	if res.IsError {
		var parts []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				parts = append(parts, tc.Text)
			}
		}
		return nil, errors.New(strings.Join(parts, " "))
	}
	if res.StructuredContent == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(res.StructuredContent)
}
