package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/noahkiss/heraldo/internal/client"
)

// maxResult caps the JSON of any one result.
const maxResult = 64 << 10

// ToolError is the structured payload of a failed call.
type ToolError struct {
	Code    string `json:"code"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *ToolError) Error() string {
	s := strings.TrimSuffix(e.Message, ".") + "."
	if h := strings.TrimSuffix(e.Hint, "."); h != "" {
		s += " " + strings.ToUpper(h[:1]) + h[1:] + "."
	}
	return s
}

func toolErr(code, msg, hint string) *ToolError {
	return &ToolError{Code: code, Message: msg, Hint: hint}
}

// call says what a failed API call needed, for the error's hint.
type call struct {
	// write is true when the call used the write token.
	write bool
	// what names the thing, for a 404, such as "event 01J...".
	what string
	// lister names the tool that lists valid ids, for a 404.
	lister string
}

// apiErr turns an API failure into a ToolError. It never carries a token.
func (t *tools) apiErr(err error, c call) *ToolError {
	var te *ToolError
	if errors.As(err, &te) {
		return te
	}
	tok, env := "read", "HERALDO_TOKEN"
	if c.write {
		tok, env = "write", "HERALDO_WRITE_TOKEN"
	}
	var ce *client.Error
	if errors.As(err, &ce) {
		e := &ToolError{Status: ce.Status, Message: ce.Message}
		switch ce.Status {
		case http.StatusUnauthorized:
			e.Code, e.Message = "unauthorized", "the server rejected the "+tok+" token"
			e.Hint = "set " + env + " to a valid " + tok + " token and restart the MCP server"
		case http.StatusForbidden:
			e.Code, e.Message = "forbidden", "the "+tok+" token has no "+tok+" scope"
			e.Hint = "set " + env + " to a token with " + tok + " scope and restart the MCP server"
		case http.StatusNotFound:
			e.Code = "not_found"
			if c.what != "" {
				e.Message = "no " + c.what
			}
			if c.lister != "" {
				e.Hint = c.lister + " lists valid ones"
			}
		case http.StatusBadRequest:
			e.Code, e.Hint = "invalid", "fix the named fields and call again"
		case http.StatusRequestEntityTooLarge:
			e.Code, e.Hint = "too_large", "shorten body"
		default:
			e.Code, e.Hint = "server_error", "try again later"
			if e.Message == "" {
				e.Message = fmt.Sprintf("the server answered HTTP %d", ce.Status)
			}
		}
		return e
	}
	if errors.Is(err, client.ErrNoDesc) {
		return toolErr("server_too_old", "this heraldo server is too old for newest-first", "pass order=oldest")
	}
	if errors.Is(err, context.Canceled) {
		return toolErr("canceled", "the call was canceled", "")
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		// The URL holds no token, but its query may hold a search; keep the
		// cause only.
		msg = ue.Err.Error()
	}
	return toolErr("unreachable", "cannot reach the heraldo server at "+t.URLHost+": "+msg,
		"check HERALDO_URL and that the server is up")
}

// result is what a handler returns: the structured payload and the text
// block for the model.
type result struct {
	out  any
	text string
}

// addTool registers a tool. The input schema is inferred from In, then
// adjusted by in; the output schema is out with an error property added.
func addTool[In any](t *tools, s *mcp.Server, tool *mcp.Tool, in func(*jsonschema.Schema), out map[string]*jsonschema.Schema,
	h func(ctx context.Context, req *mcp.CallToolRequest, input In) (result, error)) {
	is, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	if in != nil {
		in(is)
	}
	tool.InputSchema = is
	tool.OutputSchema = outSchema(out)
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		r, err := h(ctx, req, input)
		if err != nil {
			te := t.apiErr(err, call{})
			t.Logger.Info("tool", "tool", tool.Name, "error", te.Code)
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: te.Error()}}},
				map[string]any{"error": te}, nil
		}
		t.Logger.Info("tool", "tool", tool.Name)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.text}}}, r.out, nil
	})
}

func size(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func outSchema(props map[string]*jsonschema.Schema) *jsonschema.Schema {
	p := map[string]*jsonschema.Schema{
		"error": obj("Set when isError is true: code, status, message, hint.", map[string]*jsonschema.Schema{
			"code": str(""), "status": integer(""), "message": str(""), "hint": str(""),
		}),
	}
	for k, v := range props {
		p[k] = v
	}
	return &jsonschema.Schema{Type: "object", Properties: p}
}

// Schema builders for output schemas. They describe; they do not restrict
// fields the server may add.
func obj(desc string, props map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Description: desc, Properties: props}
}

func arr(desc string, items *jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Description: desc, Items: items}
}

func str(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

func integer(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: desc}
}

func boolean(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc}
}

func anyObj(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Description: desc}
}

// Input schema adjusters.
func enum(s *jsonschema.Schema, vals ...string) {
	s.Enum = nil
	for _, v := range vals {
		s.Enum = append(s.Enum, v)
	}
}

func maxLen(s *jsonschema.Schema, n int) { s.MaxLength = ptr(n) }

func between(s *jsonschema.Schema, lo, hi float64) {
	s.Minimum, s.Maximum = ptr(lo), ptr(hi)
}

func def(s *jsonschema.Schema, v any) {
	b, _ := json.Marshal(v)
	s.Default = b
}

// strList makes s a list of strings of at most n bytes each, without null.
func strList(s *jsonschema.Schema, n, items int) {
	s.Types, s.Type = nil, "array"
	maxLen(s.Items, n)
	s.MaxItems = ptr(items)
}

var noAnnot = false

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true,
		DestructiveHint: &noAnnot, OpenWorldHint: &noAnnot}
}
