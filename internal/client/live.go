package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/noahkiss/heraldo/api"
)

// Health is GET /healthz. It needs no token.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var h api.Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, "", nil, &h)
	return h, err
}

// CheckEntry is one check, decoded, with the JSON the server sent.
type CheckEntry struct {
	api.Check
	Raw json.RawMessage
}

func (e *CheckEntry) UnmarshalJSON(b []byte) error {
	e.Raw = append(json.RawMessage(nil), b...)
	return json.Unmarshal(b, &e.Check)
}

// ChecksIn returns every check, or with projects, the checks the server
// keeps under them. A server without projects ignores the filter.
func (c *Client) ChecksIn(ctx context.Context, projects []string) ([]CheckEntry, error) {
	q := url.Values{}
	for _, p := range projects {
		q.Add("project", p)
	}
	var l struct {
		Checkins []CheckEntry `json:"checkins"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/checkins", q, c.Token, nil, &l)
	return l.Checkins, err
}

// CursorState is a read cursor with its unread count.
type CursorState struct {
	Cursor string `json:"cursor"`
	Unread int    `json:"unread"`
}

// CursorUnread returns the read cursor under name with its unread count.
// found is false when the server has none.
func (c *Client) CursorUnread(ctx context.Context, name string) (cs CursorState, found bool, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/cursors/"+url.PathEscape(name), nil, c.Token, nil, &cs)
	if IsStatus(err, http.StatusNotFound) {
		var e *Error
		if errors.As(err, &e) && strings.Contains(e.Message, "no cursor named") {
			return CursorState{}, false, nil
		}
	}
	return cs, err == nil, err
}

// Activities returns the running Live Activities, or every one with all.
func (c *Client) Activities(ctx context.Context, all bool) ([]api.Activity, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var l api.ActivityList
	err := c.do(ctx, http.MethodGet, "/v1/activities", q, c.Token, nil, &l)
	return l.Activities, err
}

// Activity returns one activity with its timeline. The key may hold slashes.
func (c *Client) Activity(ctx context.Context, key string) (api.Activity, error) {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	var a api.Activity
	err := c.do(ctx, http.MethodGet, "/v1/activities/"+strings.Join(parts, "/"), nil, c.Token, nil, &a)
	return a, err
}

// Mutes returns the mutes in force, with the write token.
func (c *Client) Mutes(ctx context.Context) ([]api.Mute, error) {
	var l api.MuteList
	err := c.do(ctx, http.MethodGet, "/v1/mutes", nil, c.writeToken(), nil, &l)
	return l.Mutes, err
}

// Mute makes a mute, with the write token.
func (c *Client) Mute(ctx context.Context, in api.MuteInput) (api.Mute, error) {
	var m api.Mute
	err := c.do(ctx, http.MethodPost, "/v1/mutes", nil, c.writeToken(), in, &m)
	return m, err
}

// EndMute ends a mute now, with the write token.
func (c *Client) EndMute(ctx context.Context, id string) (api.Mute, error) {
	var m api.Mute
	err := c.do(ctx, http.MethodDelete, "/v1/mutes/"+url.PathEscape(id), nil, c.writeToken(), nil, &m)
	return m, err
}

// StateIn is State, or with projects, the state of those projects. A server
// without projects ignores the filter.
func (c *Client) StateIn(ctx context.Context, projects []string) (State, error) {
	q := url.Values{}
	for _, p := range projects {
		q.Add("project", p)
	}
	var s State
	err := c.do(ctx, http.MethodGet, "/v1/state", q, c.Token, nil, &s)
	return s, err
}

// PostNew is Post, and reports whether the server stored a new event (201)
// rather than answering a repeated dedup with the first one (200).
func (c *Client) PostNew(ctx context.Context, input map[string]any) (Event, bool, error) {
	req, err := c.request(ctx, http.MethodPost, "/v1/events", nil, c.writeToken(), input)
	if err != nil {
		return Event{}, false, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return Event{}, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return Event{}, false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Event{}, false, errorFrom(resp.StatusCode, raw)
	}
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return Event{}, false, fmt.Errorf("decode POST /v1/events: %w", err)
	}
	return e, resp.StatusCode == http.StatusCreated, nil
}
