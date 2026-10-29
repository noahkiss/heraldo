package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/noahkiss/heraldo/api"
)

func (c *Client) adminToken() string {
	if c.AdminToken != "" {
		return c.AdminToken
	}
	return c.writeToken()
}

// Settings returns every server setting. A server before settings answers
// 404.
func (c *Client) Settings(ctx context.Context) ([]api.Setting, error) {
	var l api.SettingList
	err := c.do(ctx, http.MethodGet, "/v1/settings", nil, c.Token, nil, &l)
	return l.Settings, err
}

// Setting returns one server setting.
func (c *Client) Setting(ctx context.Context, key string) (api.Setting, error) {
	var s api.Setting
	err := c.do(ctx, http.MethodGet, "/v1/settings/"+url.PathEscape(key), nil, c.Token, nil, &s)
	return s, err
}

// PutSetting stores a setting's value, with the admin token. value has the
// key's JSON type.
func (c *Client) PutSetting(ctx context.Context, key string, value any) (api.Setting, error) {
	var s api.Setting
	err := c.do(ctx, http.MethodPut, "/v1/settings/"+url.PathEscape(key), nil, c.adminToken(),
		api.SettingInput{Value: value}, &s)
	return s, err
}

// DeleteSetting removes a setting's stored value, with the admin token.
func (c *Client) DeleteSetting(ctx context.Context, key string) (api.Setting, error) {
	var s api.Setting
	err := c.do(ctx, http.MethodDelete, "/v1/settings/"+url.PathEscape(key), nil, c.adminToken(), nil, &s)
	return s, err
}
