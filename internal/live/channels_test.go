package live_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/internal/live"
	"github.com/noahkiss/heraldo/internal/push"
)

// fakeChannels is the APNs channel management API. create picks the answer
// to a create; nil makes a channel.
type fakeChannels struct {
	mu      sync.Mutex
	n       int
	created []string
	deleted []string
	creates int
	create  func() push.ChannelResult
	del     func(id string) push.ChannelResult
}

func (f *fakeChannels) CreateChannel(_ context.Context, env, topic string, policy int) push.ChannelResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.create != nil {
		return f.create()
	}
	if env != "production" || topic != "org.example.heraldo" || policy != push.ChannelStoreLatest {
		return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusBadRequest, Reason: "BadRequest"}}
	}
	f.n++
	id := fmt.Sprintf("Y2hhbm5lbC0%d=", f.n)
	f.created = append(f.created, id)
	return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusCreated}, ID: id}
}

func (f *fakeChannels) DeleteChannel(_ context.Context, _, _ string, id string) push.ChannelResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.del != nil {
		if r := f.del(id); r.Status != http.StatusNoContent {
			return r
		}
	}
	f.deleted = append(f.deleted, id)
	return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusNoContent}}
}

func (f *fakeChannels) state() (creates int, created, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, append([]string(nil), f.created...), append([]string(nil), f.deleted...)
}

const endPrint = `{"source":"printer","kind":"progress","key":"print/42","action":"end","content":{"status":"Done"}}`

func TestChannelStartNeedsNoToken(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	got := r.apns.take()
	if len(got) != 1 || got[0].event() != "start" {
		t.Fatalf("start pushes = %+v", got)
	}
	_, created, _ := r.chans.state()
	if len(created) != 1 {
		t.Fatalf("channels made = %v", created)
	}
	ch := created[0]
	// A push-to-start goes to the push-to-start token, never on a channel.
	if got[0].Token != startToken || got[0].Channel != "" {
		t.Errorf("start went to token %q channel %q", got[0].Token, got[0].Channel)
	}
	if got[0].APS["input-push-channel"] != ch || got[0].APS["input-push-token"] != nil {
		t.Errorf("start aps = %v, want input-push-channel %s and no input-push-token", got[0].APS, ch)
	}
	// Active at once, with no token report; the start timeout does not
	// apply.
	a := r.activity(t, "print/42")
	if len(a.Instances) != 1 || a.Instances[0].State != "active" {
		t.Fatalf("instances = %+v", a.Instances)
	}
	r.clk.add(live.DefaultStartTimeout)
	r.tick()
	r.post(t, update(0.5))
	got = r.apns.take()
	if len(got) != 1 || got[0].event() != "update" || got[0].Channel != ch || got[0].Token != "" {
		t.Fatalf("update pushes = %+v", got)
	}
	if got[0].Expiration <= r.clk.now().Unix() || got[0].Expiration > r.clk.now().Add(8*time.Hour).Unix() {
		t.Errorf("update expiration = %d", got[0].Expiration)
	}
	if a := r.activity(t, "print/42"); a.Instances[0].State != "active" {
		t.Fatalf("after the start timeout: %s", a.Instances[0].State)
	}
	// The end goes on the channel; the next tick deletes the channel, once.
	r.post(t, endPrint)
	got = r.apns.take()
	if len(got) != 1 || got[0].event() != "end" || got[0].Channel != ch || got[0].Expiration == 0 {
		t.Fatalf("end pushes = %+v", got)
	}
	r.tick()
	r.tick()
	if _, _, deleted := r.chans.state(); len(deleted) != 1 || deleted[0] != ch {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestChannelTokenReportIsHarmless(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	attrs, _ := r.apns.take()[0].APS["attributes"].(map[string]any)
	id := attrs["instance"].(string)
	in := r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	if in.State != "active" {
		t.Fatalf("state = %s", in.State)
	}
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("a token report on a channel instance pushed %+v", got)
	}
	r.clk.add(live.DefaultThrottle)
	r.post(t, update(0.2))
	if got := r.apns.take(); len(got) != 1 || got[0].Channel == "" {
		t.Fatalf("update = %+v, want it on the channel", got)
	}
}

func TestChannelCreateFailsFallsBackToToken(t *testing.T) {
	r := newChannelRig(t)
	r.chans.create = func() push.ChannelResult {
		return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusBadRequest, Reason: "FeatureNotEnabled"}}
	}
	r.phone(t)
	r.post(t, start)
	got := r.apns.take()
	if len(got) != 1 || got[0].APS["input-push-token"] != float64(1) || got[0].APS["input-push-channel"] != nil {
		t.Fatalf("fallback start = %+v", got)
	}
	attrs, _ := got[0].APS["attributes"].(map[string]any)
	r.report(t, attrs["instance"].(string), fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	if got := r.apns.take(); len(got) != 1 || got[0].Token != updateToken(1) || got[0].Channel != "" {
		t.Fatalf("token path update = %+v", got)
	}
	// Within the hour, a new activity does not ask APNs again.
	r.clk.add(10 * time.Minute)
	r.post(t, start)
	if creates, _, _ := r.chans.state(); creates != 1 {
		t.Fatalf("creates within the hour = %d", creates)
	}
	// After it, the server tries channels again.
	r.clk.add(time.Hour)
	r.chans.mu.Lock()
	r.chans.create = nil
	r.chans.mu.Unlock()
	r.apns.take()
	r.post(t, start)
	got = r.apns.take()
	starts := 0
	for _, p := range got {
		if p.event() == "start" {
			starts++
			if p.APS["input-push-channel"] == nil {
				t.Errorf("start after the pause has no channel: %v", p.APS)
			}
		}
	}
	if creates, _, _ := r.chans.state(); creates != 2 || starts != 1 {
		t.Fatalf("creates = %d, starts = %d", creates, starts)
	}
}

func TestChannelDroppedWhenStartFails(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.refuseStarts()
	r.post(t, start)
	_, created, deleted := r.chans.state()
	if len(created) != 1 || len(deleted) != 1 || deleted[0] != created[0] {
		t.Fatalf("created %v deleted %v: a refused start must not leave its channel", created, deleted)
	}
}

func TestChannelGoneFallsBackToToken(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	attrs, _ := r.apns.take()[0].APS["attributes"].(map[string]any)
	id := attrs["instance"].(string)
	r.report(t, id, fmt.Sprintf(`{"token":%q}`, updateToken(1)))
	r.apns.mu.Lock()
	r.apns.replyChannel = func(string) push.LiveResult {
		return push.LiveResult{Status: http.StatusBadRequest, Reason: "ChannelNotRegistered"}
	}
	r.apns.mu.Unlock()
	r.clk.add(live.DefaultThrottle)
	r.post(t, update(0.3))
	r.tick()
	got := r.apns.take()
	if len(got) != 2 || got[0].Channel == "" || got[1].Token != updateToken(1) || got[1].Channel != "" {
		t.Fatalf("pushes = %+v, want the channel refused, then the token", got)
	}
	if a := r.activity(t, "print/42"); a.Instances[0].State != "active" {
		t.Fatalf("state = %s", a.Instances[0].State)
	}
	// A lost channel is not deleted.
	r.post(t, endPrint)
	r.tick()
	if _, _, deleted := r.chans.state(); len(deleted) != 0 {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestChannelGoneWithoutTokenIsDead(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	r.apns.take()
	r.apns.mu.Lock()
	r.apns.replyChannel = func(string) push.LiveResult {
		return push.LiveResult{Status: http.StatusBadRequest, Reason: "ChannelNotRegistered"}
	}
	r.apns.mu.Unlock()
	r.clk.add(live.DefaultThrottle)
	r.post(t, update(0.3))
	if a := r.activity(t, "print/42"); a.Instances[0].State != "dead" {
		t.Fatalf("state = %s", a.Instances[0].State)
	}
	// Dead is never respawned.
	r.clk.add(live.DefaultThrottle)
	r.post(t, update(0.4))
	for _, p := range r.apns.take() {
		if p.event() == "start" {
			t.Fatalf("respawned: %+v", p)
		}
	}
}

func TestChannelDismissedIsDeleted(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	attrs, _ := r.apns.take()[0].APS["attributes"].(map[string]any)
	r.report(t, attrs["instance"].(string), `{"state":"dismissed"}`)
	r.tick()
	_, created, deleted := r.chans.state()
	if len(deleted) != 1 || deleted[0] != created[0] {
		t.Fatalf("created %v deleted %v", created, deleted)
	}
	r.clk.add(live.DefaultThrottle)
	r.post(t, update(0.5))
	if got := r.apns.take(); len(got) != 0 {
		t.Fatalf("pushes after a dismissal = %+v", got)
	}
}

func TestChannelDeleteRetries(t *testing.T) {
	r := newChannelRig(t)
	fail := true
	r.chans.del = func(string) push.ChannelResult {
		if fail {
			return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusServiceUnavailable}}
		}
		return push.ChannelResult{LiveResult: push.LiveResult{Status: http.StatusNoContent}}
	}
	r.phone(t)
	r.post(t, start)
	r.post(t, endPrint)
	r.tick()
	if _, _, deleted := r.chans.state(); len(deleted) != 0 {
		t.Fatalf("deleted = %v", deleted)
	}
	r.chans.mu.Lock()
	fail = false
	r.chans.mu.Unlock()
	r.tick() // still backing off
	if _, _, deleted := r.chans.state(); len(deleted) != 0 {
		t.Fatalf("deleted during the backoff = %v", deleted)
	}
	r.clk.add(time.Minute)
	r.tick()
	if _, _, deleted := r.chans.state(); len(deleted) != 1 {
		t.Fatalf("deleted after the backoff = %v", deleted)
	}
}

func TestChannelChainHandOff(t *testing.T) {
	r := newChannelRig(t)
	r.phone(t)
	r.post(t, start)
	r.apns.take()
	for e := time.Duration(0); e < 8*time.Hour; e += 10 * time.Minute {
		r.clk.add(10 * time.Minute)
		r.post(t, update(0.1))
		r.tick()
	}
	_, created, deleted := r.chans.state()
	if len(created) != 2 {
		t.Fatalf("channels = %v, want one per link", created)
	}
	if len(deleted) != 1 || deleted[0] != created[0] {
		t.Fatalf("deleted = %v, want the first link's", deleted)
	}
	a := r.activity(t, "print/42")
	states := map[string]int{}
	for _, in := range a.Instances {
		states[string(in.State)]++
	}
	if states["ended"] != 1 || states["active"] != 1 {
		t.Fatalf("instances = %v", states)
	}
}
