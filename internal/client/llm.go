package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// ErrNoLLM means the server has no LLM endpoints: it is older than 0.8.
var ErrNoLLM = errors.New("the server has no LLM endpoints; it needs a newer heraldo-server")

// AskTimeout is how long Ask and LLMTest wait when the client has no HTTP
// client of its own: the model may take several rounds.
const AskTimeout = 6 * time.Minute

func noLLM(err error) error {
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusMethodNotAllowed) {
		return ErrNoLLM
	}
	return err
}

// slow is c with a long timeout, for calls that wait on the model.
func (c *Client) slow() *Client {
	if c.HTTP != nil {
		return c
	}
	cc := *c
	cc.HTTP = &http.Client{Timeout: AskTimeout}
	return &cc
}

// Guidance returns every guidance line, oldest first; with scope, only
// lines of that scope.
func (c *Client) Guidance(ctx context.Context, scope string) ([]api.Guidance, error) {
	var q url.Values
	if scope != "" {
		q = url.Values{"scope": {scope}}
	}
	var l api.GuidanceList
	err := c.do(ctx, http.MethodGet, "/v1/guidance", q, c.Token, nil, &l)
	return l.Guidance, noLLM(err)
}

// GuidanceLine returns one guidance line.
func (c *Client) GuidanceLine(ctx context.Context, id string) (api.Guidance, error) {
	var g api.Guidance
	err := c.do(ctx, http.MethodGet, "/v1/guidance/"+url.PathEscape(id), nil, c.Token, nil, &g)
	return g, err
}

// AddGuidance adds a guidance line, with the write token.
func (c *Client) AddGuidance(ctx context.Context, in api.GuidanceInput) (api.Guidance, error) {
	var g api.Guidance
	err := c.do(ctx, http.MethodPost, "/v1/guidance", nil, c.writeToken(), in, &g)
	return g, noLLM(err)
}

// PutGuidance changes the fields given of a guidance line, with the write
// token.
func (c *Client) PutGuidance(ctx context.Context, id string, in api.GuidancePatch) (api.Guidance, error) {
	var g api.Guidance
	err := c.do(ctx, http.MethodPut, "/v1/guidance/"+url.PathEscape(id), nil, c.writeToken(), in, &g)
	return g, err
}

// DeleteGuidance deletes a guidance line, with the write token.
func (c *Client) DeleteGuidance(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/guidance/"+url.PathEscape(id), nil, c.writeToken(), nil, nil)
}

// LLMStatus returns the LLM's mode, circuit and budget.
func (c *Client) LLMStatus(ctx context.Context) (api.LlmStatus, error) {
	var s api.LlmStatus
	err := c.do(ctx, http.MethodGet, "/v1/llm/status", nil, c.Token, nil, &s)
	return s, noLLM(err)
}

// LLMTest asks the provider a trivial question, with the admin token.
func (c *Client) LLMTest(ctx context.Context) (api.LlmTest, error) {
	var t api.LlmTest
	err := c.slow().do(ctx, http.MethodPost, "/v1/llm/test", nil, c.adminToken(), nil, &t)
	return t, noLLM(err)
}

// Ask asks the chat model a question, with the read token.
func (c *Client) Ask(ctx context.Context, in api.AskInput) (api.AskAnswer, error) {
	var a api.AskAnswer
	err := c.slow().do(ctx, http.MethodPost, "/v1/ask", nil, c.Token, in, &a)
	return a, noLLM(err)
}
