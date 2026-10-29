package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Message is one chat message. Role is system, user, assistant or tool. A
// tool message answers the assistant's tool call ToolCallID.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is the model asking to run one tool. Arguments is JSON.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolDef is a tool the model may call. Parameters is a JSON Schema.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Schema asks for JSON output that fits a JSON Schema.
type Schema struct {
	Name   string
	Schema json.RawMessage
}

// Structured modes: how a Request with a Schema asks for JSON.
const (
	// StructuredSchema sends the schema as response_format json_schema.
	StructuredSchema = "json_schema"
	// StructuredObject sends response_format json_object and puts the
	// schema in the system prompt.
	StructuredObject = "json_object"
	// StructuredPrompt sends no response_format and no tools: the schema
	// goes in the system prompt, for endpoints without JSON mode or tool
	// calls.
	StructuredPrompt = "prompt"
)

// Request is one completion request.
type Request struct {
	Model    string
	Messages []Message
	// Tools the model may call. Dropped in StructuredPrompt mode.
	Tools []ToolDef
	// Schema, when set, asks for JSON that fits it.
	Schema *Schema
	// Structured is one of the Structured modes; empty is StructuredSchema.
	Structured  string
	Temperature *float64
	MaxTokens   int
}

// Response is the model's answer.
type Response struct {
	Content   string
	ToolCalls []ToolCall
	TokensIn  int64
	TokensOut int64
}

// Provider sends one completion request. A Provider never logs a token.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// HTTPError is a provider's non-2xx answer. Message is the start of its
// body, without the request.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("the endpoint answered HTTP %d: %s", e.Status, e.Message)
}

// maxProviderBody caps a provider's response body.
const maxProviderBody = 4 << 20

// schemaPrompt is the system prompt line for a schema in the modes that
// do not send it as response_format.
func schemaPrompt(s *Schema) string {
	return "Answer with one JSON object and nothing else. It must fit this JSON Schema:\n" + string(s.Schema)
}

// withSchemaPrompt returns msgs with the schema line added to the first
// system message, or a new system message in front.
func withSchemaPrompt(msgs []Message, s *Schema) []Message {
	out := append([]Message{}, msgs...)
	line := schemaPrompt(s)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content += "\n\n" + line
		return out
	}
	return append([]Message{{Role: "system", Content: line}}, out...)
}

func post(ctx context.Context, hc *http.Client, endpoint, token string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL has no token in it, but drop it anyway: the cause is
		// enough.
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &HTTPError{Status: resp.StatusCode, Message: msg}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the endpoint's answer is not JSON: %v", err)
	}
	return nil
}

// OpenAIChat is an OpenAI-compatible /chat/completions endpoint. BaseURL
// is the API root, such as https://llm.example.net/v1.
type OpenAIChat struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Complete is POST <BaseURL>/chat/completions.
func (p *OpenAIChat) Complete(ctx context.Context, req Request) (Response, error) {
	msgs, body := req.Messages, map[string]any{"model": req.Model}
	mode := req.Structured
	if mode == "" {
		mode = StructuredSchema
	}
	if req.Schema != nil {
		switch mode {
		case StructuredSchema:
			body["response_format"] = map[string]any{"type": "json_schema",
				"json_schema": map[string]any{"name": req.Schema.Name, "schema": req.Schema.Schema}}
		case StructuredObject:
			body["response_format"] = map[string]any{"type": "json_object"}
			msgs = withSchemaPrompt(msgs, req.Schema)
		default:
			msgs = withSchemaPrompt(msgs, req.Schema)
		}
	}
	if len(req.Tools) > 0 && mode != StructuredPrompt {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = tools
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	var out []chatMessage
	for _, m := range msgs {
		if mode == StructuredPrompt && (m.Role == "tool" || len(m.ToolCalls) > 0) {
			continue
		}
		c := m.Content
		cm := chatMessage{Role: m.Role, Content: &c, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var t chatToolCall
			t.ID, t.Type, t.Function.Name, t.Function.Arguments = tc.ID, "function", tc.Name, tc.Arguments
			cm.ToolCalls = append(cm.ToolCalls, t)
		}
		if len(cm.ToolCalls) > 0 && c == "" {
			cm.Content = nil
		}
		out = append(out, cm)
	}
	body["messages"] = out
	var resp struct {
		Choices []struct {
			Message struct {
				Content   *string        `json:"content"`
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := post(ctx, p.HTTP, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", p.Token, body, &resp); err != nil {
		return Response{}, err
	}
	if len(resp.Choices) == 0 {
		return Response{}, errors.New("the endpoint answered with no choices")
	}
	m := resp.Choices[0].Message
	r := Response{TokensIn: resp.Usage.PromptTokens, TokensOut: resp.Usage.CompletionTokens}
	if m.Content != nil {
		r.Content = *m.Content
	}
	for _, tc := range m.ToolCalls {
		r.ToolCalls = append(r.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return r, nil
}

// OpenAIResponses is an OpenAI-compatible /responses endpoint.
type OpenAIResponses struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Complete is POST <BaseURL>/responses. The system messages become
// instructions; the rest become input items.
func (p *OpenAIResponses) Complete(ctx context.Context, req Request) (Response, error) {
	msgs, body := req.Messages, map[string]any{"model": req.Model, "store": false}
	mode := req.Structured
	if mode == "" {
		mode = StructuredSchema
	}
	if req.Schema != nil {
		switch mode {
		case StructuredSchema:
			body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": req.Schema.Name,
				"schema": req.Schema.Schema}}
		case StructuredObject:
			body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
			msgs = withSchemaPrompt(msgs, req.Schema)
		default:
			msgs = withSchemaPrompt(msgs, req.Schema)
		}
	}
	if len(req.Tools) > 0 && mode != StructuredPrompt {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description,
				"parameters": t.Parameters})
		}
		body["tools"] = tools
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	var instructions []string
	var input []map[string]any
	for _, m := range msgs {
		switch {
		case m.Role == "system":
			instructions = append(instructions, m.Content)
		case m.Role == "tool":
			if mode != StructuredPrompt {
				input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			}
		case len(m.ToolCalls) > 0:
			if mode == StructuredPrompt {
				continue
			}
			if m.Content != "" {
				input = append(input, map[string]any{"role": m.Role, "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Name,
					"arguments": tc.Arguments})
			}
		default:
			input = append(input, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	if len(instructions) > 0 {
		body["instructions"] = strings.Join(instructions, "\n\n")
	}
	body["input"] = input
	var resp struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := post(ctx, p.HTTP, strings.TrimRight(p.BaseURL, "/")+"/responses", p.Token, body, &resp); err != nil {
		return Response{}, err
	}
	r := Response{TokensIn: resp.Usage.InputTokens, TokensOut: resp.Usage.OutputTokens}
	var text []string
	for _, o := range resp.Output {
		switch o.Type {
		case "function_call":
			r.ToolCalls = append(r.ToolCalls, ToolCall{ID: o.CallID, Name: o.Name, Arguments: o.Arguments})
		case "message":
			for _, c := range o.Content {
				if c.Type == "output_text" {
					text = append(text, c.Text)
				}
			}
		}
	}
	r.Content = strings.Join(text, "")
	return r, nil
}

// ParseJSON finds the JSON object in a model's text and decodes it into
// out. It allows a ``` fence and text around the object, for endpoints in
// prompt mode.
func ParseJSON(text string, out any) error {
	t := strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(t), out); err == nil {
		return nil
	}
	i, j := strings.Index(t, "{"), strings.LastIndex(t, "}")
	if i < 0 || j <= i {
		return errors.New("no JSON object in the answer")
	}
	return json.Unmarshal([]byte(t[i:j+1]), out)
}
