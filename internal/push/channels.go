package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Apple's broadcast channel management hosts. Each environment has its own
// port; a channel made in one environment is unknown in the other.
const (
	ManageProductionURL = "https://api-manage-broadcast.push.apple.com:2196"
	ManageSandboxURL    = "https://api-manage-broadcast.sandbox.push.apple.com:2195"
)

// Channel storage policies (message-storage-policy at creation).
const (
	// ChannelNoStorage keeps nothing for an offline device; every push on
	// the channel must carry apns-expiration 0.
	ChannelNoStorage = 0
	// ChannelStoreLatest keeps the most recent push, up to 8 hours.
	ChannelStoreLatest = 1
)

// ChannelResult is APNs's answer to a channel management request. ID is the
// new channel's id after a create.
type ChannelResult struct {
	LiveResult
	ID string
}

// ChannelCreated reports whether a create succeeded.
func (r ChannelResult) ChannelCreated() bool {
	return r.Err == nil && r.Status == http.StatusCreated && r.ID != ""
}

// ChannelDeleted reports whether a delete succeeded, or the channel is gone
// already.
func (r ChannelResult) ChannelDeleted() bool {
	return r.Err == nil && (r.Status == http.StatusNoContent || r.Status == http.StatusOK ||
		r.Status == http.StatusNotFound || r.Reason == "ChannelNotRegistered")
}

// ChannelGone reports whether a broadcast failed because the channel does
// not exist (deleted, or made in the other environment).
func (r LiveResult) ChannelGone() bool {
	return r.Status == http.StatusBadRequest && (r.Reason == "ChannelNotRegistered" || r.Reason == "BadChannelId")
}

// FeatureOff reports whether APNs refused because the app's App ID lacks
// the broadcast capability.
func (r LiveResult) FeatureOff() bool {
	return r.Status == http.StatusBadRequest && r.Reason == "FeatureNotEnabled"
}

// CreateChannel makes a Live Activity broadcast channel for bundle id topic
// in environment (sandbox or production) with the storage policy.
func (p *Pusher) CreateChannel(ctx context.Context, environment, topic string, policy int) ChannelResult {
	body, _ := json.Marshal(map[string]any{"message-storage-policy": policy, "push-type": "LiveActivity"})
	return p.manage(ctx, http.MethodPost, environment, topic, "", body)
}

// DeleteChannel deletes channel id of bundle id topic in environment.
func (p *Pusher) DeleteChannel(ctx context.Context, environment, topic, id string) ChannelResult {
	return p.manage(ctx, http.MethodDelete, environment, topic, id, nil)
}

func (p *Pusher) manage(ctx context.Context, method, environment, topic, id string, body []byte) ChannelResult {
	r := p.manageOnce(ctx, method, environment, topic, id, body, time.Time{})
	if r.Err == nil && (answer{status: r.Status, reason: r.Reason}).staleToken() {
		r = p.manageOnce(ctx, method, environment, topic, id, body, p.cfg.Now())
	}
	return r
}

func (p *Pusher) manageOnce(ctx context.Context, method, environment, topic, id string, body []byte, stale time.Time) ChannelResult {
	base := p.cfg.ManageProductionURL
	if environment == "sandbox" {
		base = p.cfg.ManageSandboxURL
	}
	jwt, _, err := p.sign.get(stale)
	if err != nil {
		return ChannelResult{LiveResult: LiveResult{Status: -1, Err: fmt.Errorf("sign provider token: %w", err)}}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+"/1/apps/"+topic+"/channels", rd)
	if err != nil {
		return ChannelResult{LiveResult: LiveResult{Status: -1, Err: err}}
	}
	req.Header.Set("authorization", "bearer "+jwt)
	if id != "" {
		req.Header.Set("apns-channel-id", id)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return ChannelResult{LiveResult: LiveResult{Err: err}}
	}
	defer resp.Body.Close()
	out := ChannelResult{LiveResult: LiveResult{Status: resp.StatusCode, Reason: readReason(resp)}, ID: resp.Header.Get("apns-channel-id")}
	return out
}

// readReason returns the reason of a failed APNs answer.
func readReason(resp *http.Response) string {
	if resp.StatusCode < 300 {
		return ""
	}
	var r struct {
		Reason string `json:"reason"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(raw, &r)
	return r.Reason
}

// sendBroadcast posts one Live Activity push on a channel: POST
// /4/broadcasts/apps/<bundle id> on the push host of the environment.
// apns-expiration is required there; 0 means "try once, keep nothing".
func (p *Pusher) sendBroadcast(ctx context.Context, lp LivePush, stale time.Time) (answer, error) {
	base := p.cfg.ProductionURL
	if lp.Environment == "sandbox" {
		base = p.cfg.SandboxURL
	}
	jwt, _, err := p.sign.get(stale)
	if err != nil {
		return answer{status: -1}, fmt.Errorf("sign provider token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/4/broadcasts/apps/"+lp.Topic, bytes.NewReader(lp.Body))
	if err != nil {
		return answer{status: -1}, err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-channel-id", lp.Channel)
	req.Header.Set("apns-push-type", "liveactivity")
	req.Header.Set("apns-priority", strconv.Itoa(lp.Priority))
	req.Header.Set("apns-expiration", strconv.FormatInt(max(lp.Expiration, 0), 10))
	req.Header.Set("content-type", "application/json")
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	return answer{status: resp.StatusCode, reason: readReason(resp), apnsID: resp.Header.Get("apns-unique-id")}, nil
}
