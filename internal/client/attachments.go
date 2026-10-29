package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/noahkiss/heraldo/api"
)

// MaxAttachmentBytes is the server's cap on one attachment.
const MaxAttachmentBytes = 5 << 20

// UploadAttachment stores data with the write token and returns the
// attachment. contentType may be empty: the server then types the bytes.
func (c *Client) UploadAttachment(ctx context.Context, contentType string, data []byte) (api.AttachmentCreated, error) {
	var out api.AttachmentCreated
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/v1/attachments", bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.writeToken())
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, errorFrom(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode POST /v1/attachments: %w", err)
	}
	return out, nil
}

// Attachment returns an attachment's bytes and Content-Type, with the read
// token.
func (c *Client) Attachment(ctx context.Context, id string) ([]byte, string, error) {
	req, err := c.request(ctx, http.MethodGet, "/v1/attachments/"+url.PathEscape(id), nil, c.Token, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", errorFrom(resp.StatusCode, raw)
	}
	return raw, resp.Header.Get("Content-Type"), nil
}
