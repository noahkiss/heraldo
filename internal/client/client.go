// Package client calls the heraldo read and write API. The CLI uses it; it
// decodes into the same generated types the server validates against.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// Client talks to one heraldo server.
type Client struct {
	// BaseURL is the server root, such as https://heraldo.example.net.
	BaseURL string
	// Token is a read token, for every call but Post.
	Token string
	// WriteToken is for Post. Empty means Token.
	WriteToken string
	// HTTP is the client for every call but Stream; nil means one with a
	// 15 second timeout.
	HTTP *http.Client
	// UserAgent is sent with every call.
	UserAgent string
}

// Error is an answer from the server that is not a success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is an Error with the given status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// Event is one stored event, decoded, with the JSON the server sent.
type Event struct {
	api.Event
	Raw json.RawMessage
}

func (e *Event) UnmarshalJSON(b []byte) error {
	e.Raw = append(json.RawMessage(nil), b...)
	return json.Unmarshal(b, &e.Event)
}

// Page is one page of a list.
type Page struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor"`
}

// State is the derived state: the latest event per key.
type State struct {
	Incidents []Event `json:"incidents"`
	Progress  []Event `json:"progress"`
	Checkins  []Event `json:"checkins"`
	Statuses  []Event `json:"statuses"`
}

// Query selects events for List. Empty fields do not filter.
type Query struct {
	After, Before string
	// Desc lists newest first.
	Desc                         bool
	Limit                        int
	Source, Kind, Host, Key, Tag string
	Q, Hold                      string
}

func (q Query) values() url.Values {
	v := url.Values{}
	for _, p := range []struct{ k, v string }{
		{"after", q.After}, {"before", q.Before}, {"source", q.Source}, {"kind", q.Kind},
		{"host", q.Host}, {"key", q.Key}, {"tag", q.Tag}, {"q", q.Q}, {"hold", q.Hold},
	} {
		if p.v != "" {
			v.Set(p.k, p.v)
		}
	}
	if q.Desc {
		v.Set("order", "desc")
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, token string, body any) (*http.Request, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// do sends a request and decodes a 2xx JSON answer into out.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, token string, body, out any) error {
	_, err := c.doHeader(ctx, method, path, q, token, body, out)
	return err
}

func (c *Client) doHeader(ctx context.Context, method, path string, q url.Values, token string, body, out any) (http.Header, error) {
	req, err := c.request(ctx, method, path, q, token, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, errorFrom(resp.StatusCode, raw)
	}
	if out == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.Header, fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return resp.Header, nil
}

func errorFrom(status int, raw []byte) error {
	var e api.Error
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return &Error{Status: status, Message: e.Error}
	}
	return &Error{Status: status, Message: strings.TrimSpace(string(raw))}
}

// ErrNoDesc means the server ignored order=desc, before and hold: it is
// older than the API that has them.
var ErrNoDesc = errors.New("the server cannot list newest first")

// List returns one page of events. A newest-first query on a server that
// cannot answer it gives ErrNoDesc.
func (c *Client) List(ctx context.Context, q Query) (Page, error) {
	var p Page
	hdr, err := c.doHeader(ctx, http.MethodGet, "/v1/events", q.values(), c.Token, nil, &p)
	if err == nil && q.Desc && hdr.Get("Heraldo-Order") != "desc" {
		return Page{}, ErrNoDesc
	}
	return p, err
}

// Get returns one event with its full body.
func (c *Client) Get(ctx context.Context, id string) (Event, error) {
	var e Event
	err := c.do(ctx, http.MethodGet, "/v1/events/"+url.PathEscape(id), nil, c.Token, nil, &e)
	return e, err
}

// State returns the derived state.
func (c *Client) State(ctx context.Context) (State, error) {
	var s State
	err := c.do(ctx, http.MethodGet, "/v1/state", nil, c.Token, nil, &s)
	return s, err
}

// Checks returns every check. A server before stage 6 answers 404.
func (c *Client) Checks(ctx context.Context) ([]api.Check, error) {
	var l api.CheckList
	err := c.do(ctx, http.MethodGet, "/v1/checkins", nil, c.Token, nil, &l)
	return l.Checkins, err
}

// Check returns one check.
func (c *Client) Check(ctx context.Context, slug string) (api.Check, error) {
	var ch api.Check
	err := c.do(ctx, http.MethodGet, "/v1/checkins/"+url.PathEscape(slug), nil, c.Token, nil, &ch)
	return ch, err
}

// PutCheck creates a check or replaces its config, with the write token.
func (c *Client) PutCheck(ctx context.Context, slug string, cfg api.CheckConfig) (api.Check, error) {
	var ch api.Check
	err := c.do(ctx, http.MethodPut, "/v1/checkins/"+url.PathEscape(slug), nil, c.writeToken(), cfg, &ch)
	return ch, err
}

// SetPaused pauses or resumes a check, with the write token.
func (c *Client) SetPaused(ctx context.Context, slug string, paused bool) (api.Check, error) {
	op := "/resume"
	if paused {
		op = "/pause"
	}
	var ch api.Check
	err := c.do(ctx, http.MethodPost, "/v1/checkins/"+url.PathEscape(slug)+op, nil, c.writeToken(), nil, &ch)
	return ch, err
}

func (c *Client) writeToken() string {
	if c.WriteToken == "" {
		return c.Token
	}
	return c.WriteToken
}

type cursorBody struct {
	Cursor string `json:"cursor"`
}

// Cursor returns the server's read cursor for this token under name. found
// is false when the server has none.
func (c *Client) Cursor(ctx context.Context, name string) (cursor string, found bool, err error) {
	var b cursorBody
	err = c.do(ctx, http.MethodGet, "/v1/cursors/"+url.PathEscape(name), nil, c.Token, nil, &b)
	if IsStatus(err, http.StatusNotFound) {
		var e *Error
		errors.As(err, &e)
		// A server without the cursor API answers 404 too, with no JSON
		// error naming the cursor.
		if strings.Contains(e.Message, "no cursor named") {
			return "", false, nil
		}
	}
	return b.Cursor, err == nil, err
}

// SetCursor moves the read cursor under name forward to cursor. It returns
// the cursor the server keeps, which is later than cursor when another copy
// of the client moved it further.
func (c *Client) SetCursor(ctx context.Context, name, cursor string) (string, error) {
	var b cursorBody
	err := c.do(ctx, http.MethodPut, "/v1/cursors/"+url.PathEscape(name), nil, c.Token, cursorBody{cursor}, &b)
	return b.Cursor, err
}

// Post stores one event with the write token. input is an EventInput as JSON
// fields.
func (c *Client) Post(ctx context.Context, input map[string]any) (Event, error) {
	var e Event
	err := c.do(ctx, http.MethodPost, "/v1/events", nil, c.writeToken(), input, &e)
	return e, err
}

// ErrNoStream means the server has no /v1/stream; poll instead.
var ErrNoStream = errors.New("the server has no event stream")

// Stream opens GET /v1/stream and calls poke with the cursor of each poke,
// the first one on connect. It returns when ctx ends, the stream breaks, or
// poke returns an error. A server without the stream gives ErrNoStream.
func (c *Client) Stream(ctx context.Context, poke func(cursor string) error) error {
	req, err := c.request(ctx, http.MethodGet, "/v1/stream", nil, c.Token, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// No client timeout: the stream stays open. Heartbeats keep it alive.
	resp, err := (&http.Client{Transport: c.http().Transport}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return ErrNoStream
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errorFrom(resp.StatusCode, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event == "poke" && data != "" {
				var b cursorBody
				if err := json.Unmarshal([]byte(data), &b); err == nil && b.Cursor != "" {
					if err := poke(b.Cursor); err != nil {
						return err
					}
				}
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.ErrUnexpectedEOF
}
