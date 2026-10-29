package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/noahkiss/heraldo/api"
)

// HoldQuery selects the hold's queue.
type HoldQuery struct {
	// Pending lists open entries only.
	Pending bool
	// Poll makes the call a triage poll, which resets the dead-man. A
	// person or a tool that only looks leaves it false.
	Poll bool
}

// Hold returns the alert hold's queue, with the read token.
func (c *Client) Hold(ctx context.Context, q HoldQuery) (api.HoldQueue, error) {
	v := url.Values{}
	if q.Pending {
		v.Set("state", "pending")
	}
	if !q.Poll {
		v.Set("peek", "1")
	}
	var h api.HoldQueue
	err := c.do(ctx, http.MethodGet, "/v1/hold", v, c.Token, nil, &h)
	return h, err
}

// HoldResult is the answer to a release or a suppress.
type HoldResult struct {
	Released        []string `json:"released,omitempty"`
	Suppressed      []string `json:"suppressed,omitempty"`
	UnknownOrClosed []string `json:"unknown_or_closed"`
	Mode            string   `json:"mode"`
	Sent            bool     `json:"sent,omitempty"`
	EventID         string   `json:"event_id,omitempty"`
}

// HoldRelease closes held entries with one message, with the write token.
func (c *Client) HoldRelease(ctx context.Context, in api.HoldReleaseJSONBody) (HoldResult, error) {
	var r HoldResult
	err := c.do(ctx, http.MethodPost, "/v1/hold/release", nil, c.writeToken(), in, &r)
	return r, err
}

// HoldSuppress closes held entries and sends nothing, with the write token.
func (c *Client) HoldSuppress(ctx context.Context, in api.HoldSuppressJSONBody) (HoldResult, error) {
	var r HoldResult
	err := c.do(ctx, http.MethodPost, "/v1/hold/suppress", nil, c.writeToken(), in, &r)
	return r, err
}

// Devices returns every registered device, retired ones too, with the read
// token. Push tokens come as suffixes only.
func (c *Client) Devices(ctx context.Context) ([]api.Device, error) {
	var l api.DeviceList
	err := c.do(ctx, http.MethodGet, "/v1/devices", nil, c.Token, nil, &l)
	return l.Devices, err
}

// RetireDevice stops pushes to a device, with the read token, as the apps
// do. The device's history stays.
func (c *Client) RetireDevice(ctx context.Context, id string) (api.Device, error) {
	var d api.Device
	err := c.do(ctx, http.MethodDelete, "/v1/devices/"+url.PathEscape(id), nil, c.Token, nil, &d)
	return d, err
}
