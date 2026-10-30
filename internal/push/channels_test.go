package push

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestChannelCreateDelete(t *testing.T) {
	r := newRig(t, 0)
	r.p.cfg.ManageSandboxURL = r.sand.srv.URL
	r.sand.setReply(func(_ int, q fakeReq) (int, string) {
		if q.Method == http.MethodPost {
			return http.StatusCreated, ""
		}
		return http.StatusNoContent, ""
	})
	ctx := context.Background()
	c := r.p.CreateChannel(ctx, "sandbox", topic, ChannelStoreLatest)
	if !c.ChannelCreated() || c.ID == "" {
		t.Fatalf("create = %+v", c)
	}
	d := r.p.DeleteChannel(ctx, "sandbox", topic, c.ID)
	if !d.ChannelDeleted() {
		t.Fatalf("delete = %+v", d)
	}
	got := r.sand.requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d", len(got))
	}
	var body map[string]any
	_ = json.Unmarshal(got[0].Body, &body)
	if got[0].Method != http.MethodPost || got[0].Path != "/1/apps/"+topic+"/channels" ||
		body["message-storage-policy"] != float64(1) || body["push-type"] != "LiveActivity" || got[0].Auth == "" {
		t.Errorf("create request = %+v, body %v", got[0], body)
	}
	if got[1].Method != http.MethodDelete || got[1].Path != "/1/apps/"+topic+"/channels" || got[1].Channel != c.ID {
		t.Errorf("delete request = %+v", got[1])
	}
	if len(r.prod.requests()) != 0 {
		t.Error("a sandbox channel request went to production")
	}
}

func TestChannelCreateRefused(t *testing.T) {
	r := newRig(t, 0)
	r.p.cfg.ManageProductionURL = r.prod.srv.URL
	r.prod.setReply(func(int, fakeReq) (int, string) { return http.StatusBadRequest, "FeatureNotEnabled" })
	c := r.p.CreateChannel(context.Background(), "production", topic, ChannelStoreLatest)
	if c.ChannelCreated() || !c.FeatureOff() {
		t.Fatalf("create = %+v", c)
	}
}

func TestSendLiveBroadcast(t *testing.T) {
	r := newRig(t, 0)
	r.prod.setReply(func(n int, _ fakeReq) (int, string) {
		if n == 0 {
			return http.StatusForbidden, "ExpiredProviderToken"
		}
		if n == 2 {
			return http.StatusBadRequest, "ChannelNotRegistered"
		}
		return http.StatusOK, ""
	})
	lp := LivePush{Environment: "production", Channel: "dHN0LWNobmw=", Topic: topic, Body: []byte(`{"aps":{}}`), Priority: 5}
	if res := r.p.SendLive(context.Background(), lp); !res.Sent() {
		t.Fatalf("stale provider token then 200 = %+v", res)
	}
	got := r.prod.requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d", len(got))
	}
	q := got[1]
	// No apns-topic on a broadcast; apns-expiration is required, 0 here.
	if q.Path != "/4/broadcasts/apps/"+topic || q.Channel != lp.Channel || q.PushType != "liveactivity" ||
		q.Priority != "5" || q.Expiration != "0" || q.Topic != "" {
		t.Errorf("request = %+v", q)
	}
	if res := r.p.SendLive(context.Background(), lp); !res.ChannelGone() || res.Gone() {
		t.Fatalf("ChannelNotRegistered = %+v", res)
	}
}
