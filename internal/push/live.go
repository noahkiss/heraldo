package push

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/noahkiss/heraldo/internal/store"
)

// LiveTopicSuffix makes the apns-topic of a Live Activity push from the
// app's bundle id.
const LiveTopicSuffix = ".push-type.liveactivity"

// LivePush is one Live Activity push: a push-to-start to a device's
// push-to-start token, or an update or end to one activity's token.
type LivePush struct {
	// Environment is the device's APNs environment, sandbox or production.
	Environment string
	// Token is the push-to-start token or the activity's update token, hex.
	Token string
	// Topic is the app's bundle id; the push goes to Topic plus
	// LiveTopicSuffix.
	Topic      string
	Body       []byte
	Priority   int
	Expiration int64
}

// LiveResult is APNs's answer to a LivePush.
type LiveResult struct {
	Status int
	Reason string
	// Err is set when no answer came.
	Err error
}

// Sent reports whether APNs took the push.
func (r LiveResult) Sent() bool { return r.Err == nil && r.Status == http.StatusOK }

// Gone reports whether the token is dead for good: the activity ended on
// the device, or the push-to-start token was replaced.
func (r LiveResult) Gone() bool {
	return r.Status == http.StatusGone ||
		(r.Status == http.StatusBadRequest && (r.Reason == "BadDeviceToken" || r.Reason == "DeviceTokenNotForTopic" ||
			r.Reason == "ExpiredToken"))
}

// Error describes a push that was not sent.
func (r LiveResult) Error() string {
	switch {
	case r.Err != nil:
		return r.Err.Error()
	case r.Reason != "":
		return r.Reason
	default:
		return strconv.Itoa(r.Status)
	}
}

// SendLive sends one Live Activity push now. It signs a new provider token
// and tries again once when APNs refuses the old one. It never waits to
// retry: the caller holds its own backoff, and a later update replaces a
// lost one.
func (p *Pusher) SendLive(ctx context.Context, lp LivePush) LiveResult {
	dev := store.Device{Token: lp.Token, Environment: lp.Environment, Topic: lp.Topic}
	h := Headers{Priority: lp.Priority, Expiration: lp.Expiration, PushType: "liveactivity", Topic: lp.Topic + LiveTopicSuffix}
	a, err := p.send(ctx, dev, lp.Body, h, time.Time{})
	if err == nil && a.staleToken() {
		a, err = p.send(ctx, dev, lp.Body, h, p.cfg.Now())
	}
	return LiveResult{Status: a.status, Reason: a.reason, Err: err}
}
