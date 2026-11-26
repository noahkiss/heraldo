package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/noahkiss/heraldo/api"
)

// Delivery returns the delivery settings, by position, with whether they
// are on, with the read token.
func (c *Client) Delivery(ctx context.Context) (api.DeliverySettingList, error) {
	var l api.DeliverySettingList
	err := c.do(ctx, http.MethodGet, "/v1/delivery", nil, c.Token, nil, &l)
	return l, err
}

// DeliveryRow returns one delivery setting by id.
func (c *Client) DeliveryRow(ctx context.Context, id string) (api.DeliverySetting, error) {
	var d api.DeliverySetting
	err := c.do(ctx, http.MethodGet, "/v1/delivery/"+url.PathEscape(id), nil, c.Token, nil, &d)
	return d, err
}

// AddDelivery adds a delivery setting, with the admin token.
func (c *Client) AddDelivery(ctx context.Context, in api.DeliverySettingInput) (api.DeliverySetting, error) {
	var d api.DeliverySetting
	err := c.do(ctx, http.MethodPost, "/v1/delivery", nil, c.adminToken(), in, &d)
	return d, err
}

// PutDelivery replaces the delivery setting id, with the admin token.
func (c *Client) PutDelivery(ctx context.Context, id string, in api.DeliverySettingInput) (api.DeliverySetting, error) {
	var d api.DeliverySetting
	err := c.do(ctx, http.MethodPut, "/v1/delivery/"+url.PathEscape(id), nil, c.adminToken(), in, &d)
	return d, err
}

// DeleteDelivery deletes the delivery setting id, with the admin token.
func (c *Client) DeleteDelivery(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/delivery/"+url.PathEscape(id), nil, c.adminToken(), nil, nil)
}

// PreviewDelivery runs rows over the stored events, with the read token.
func (c *Client) PreviewDelivery(ctx context.Context, in api.DeliveryPreviewInput) (api.DeliveryPreview, error) {
	var p api.DeliveryPreview
	err := c.do(ctx, http.MethodPost, "/v1/delivery/preview", nil, c.Token, in, &p)
	return p, err
}

// Sources returns every source, new ones first, with the read token.
func (c *Client) Sources(ctx context.Context) ([]api.Source, error) {
	var l api.SourceList
	err := c.do(ctx, http.MethodGet, "/v1/sources", nil, c.Token, nil, &l)
	return l.Sources, err
}

// ReviewSource marks a source reviewed, with the admin token.
func (c *Client) ReviewSource(ctx context.Context, source string) (api.Source, error) {
	var s api.Source
	err := c.do(ctx, http.MethodPost, "/v1/sources/"+url.PathEscape(source)+"/review", nil, c.adminToken(), nil, &s)
	return s, err
}
