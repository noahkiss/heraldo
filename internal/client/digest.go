package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/noahkiss/heraldo/api"
)

// DigestPreview returns the digest as it would go out now, with the read
// token. It stores and sends nothing.
func (c *Client) DigestPreview(ctx context.Context) (api.DigestPreview, error) {
	var d api.DigestPreview
	err := c.do(ctx, http.MethodGet, "/v1/digest/preview", nil, c.Token, nil, &d)
	return d, err
}

// SendDigest builds the digest and sends it now, with the write token.
func (c *Client) SendDigest(ctx context.Context) (api.DigestPreview, error) {
	var d api.DigestPreview
	err := c.do(ctx, http.MethodPost, "/v1/digest/send", nil, c.writeToken(), nil, &d)
	return d, err
}

// StaleIncidents returns the stale incidents of the state (stale=1), each
// with incident.stale set. projects filters as StateIn does. A server older
// than 0.7 ignores stale=1 and marks nothing, so the answer keeps only the
// incidents the server marked.
func (c *Client) StaleIncidents(ctx context.Context, projects []string) ([]Event, error) {
	q := url.Values{"stale": {"1"}}
	for _, p := range projects {
		q.Add("project", p)
	}
	var s State
	if err := c.do(ctx, http.MethodGet, "/v1/state", q, c.Token, nil, &s); err != nil {
		return nil, err
	}
	out := []Event{}
	for _, e := range s.Incidents {
		if IsStale(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// IsStale reports whether the server marked an incident stale.
func IsStale(e Event) bool {
	return e.Incident != nil && e.Incident.Stale != nil && *e.Incident.Stale
}
