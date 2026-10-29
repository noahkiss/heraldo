package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/noahkiss/heraldo/api"
)

// ErrNoViews means the server has no saved views, or ignored view=: it is
// older than saved views.
var ErrNoViews = errors.New("the server has no saved views; it needs a newer heraldo-server")

// Views returns the saved views, by position, then name, with the read
// token.
func (c *Client) Views(ctx context.Context) ([]api.View, error) {
	var l api.ViewList
	err := c.do(ctx, http.MethodGet, "/v1/views", nil, c.Token, nil, &l)
	if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusMethodNotAllowed) {
		return nil, ErrNoViews
	}
	return l.Views, err
}

// View returns one saved view by id, or by name without regard to case.
func (c *Client) View(ctx context.Context, ref string) (api.View, error) {
	var v api.View
	err := c.do(ctx, http.MethodGet, "/v1/views/"+url.PathEscape(ref), nil, c.Token, nil, &v)
	return v, err
}

// SaveView makes a view, with the write token.
func (c *Client) SaveView(ctx context.Context, in api.ViewInput) (api.View, error) {
	var v api.View
	err := c.do(ctx, http.MethodPost, "/v1/views", nil, c.writeToken(), in, &v)
	return v, err
}

// PutView replaces the view ref names, with the write token.
func (c *Client) PutView(ctx context.Context, ref string, in api.ViewInput) (api.View, error) {
	var v api.View
	err := c.do(ctx, http.MethodPut, "/v1/views/"+url.PathEscape(ref), nil, c.writeToken(), in, &v)
	return v, err
}

// DeleteView deletes the view ref names, with the write token.
func (c *Client) DeleteView(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/v1/views/"+url.PathEscape(ref), nil, c.writeToken(), nil, nil)
}
