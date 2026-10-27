package push

import (
	"context"
	"net/http"
	"testing"
)

func TestSendLiveHeaders(t *testing.T) {
	r := newRig(t, 0)
	const laToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	res := r.p.SendLive(context.Background(), LivePush{
		Environment: "sandbox", Token: laToken, Topic: topic, Body: []byte(`{"aps":{}}`), Priority: 5, Expiration: 1700000000,
	})
	if !res.Sent() {
		t.Fatalf("result = %+v", res)
	}
	got := r.sand.requests()
	if len(got) != 1 {
		t.Fatalf("sandbox requests = %d", len(got))
	}
	q := got[0]
	if q.Path != "/3/device/"+laToken || q.PushType != "liveactivity" || q.Topic != topic+".push-type.liveactivity" ||
		q.Priority != "5" || q.Expiration != "1700000000" || q.Proto != 2 {
		t.Errorf("request = %+v", q)
	}
	if len(r.prod.requests()) != 0 {
		t.Error("a sandbox push went to production")
	}
}

func TestSendLiveAnswers(t *testing.T) {
	r := newRig(t, 0)
	r.prod.setReply(func(n int, _ fakeReq) (int, string) {
		switch n {
		case 0:
			return http.StatusServiceUnavailable, ""
		case 1:
			return http.StatusForbidden, "ExpiredProviderToken"
		case 2:
			return http.StatusOK, ""
		default:
			return http.StatusGone, "Unregistered"
		}
	})
	lp := LivePush{Environment: "production", Token: prodToken, Topic: topic, Body: []byte(`{}`), Priority: 10}
	// A 503 comes back at once, with no retry: the caller backs off.
	if res := r.p.SendLive(context.Background(), lp); res.Sent() || res.Gone() || res.Status != http.StatusServiceUnavailable {
		t.Fatalf("a 503 = %+v", res)
	}
	// A stale provider token is signed again and sent once more.
	if res := r.p.SendLive(context.Background(), lp); !res.Sent() {
		t.Fatalf("ExpiredProviderToken then 200 = %+v", res)
	}
	res := r.p.SendLive(context.Background(), lp)
	if res.Sent() || !res.Gone() || res.Error() != "Unregistered" {
		t.Fatalf("a 410 = %+v", res)
	}
	if n := len(r.prod.requests()); n != 4 {
		t.Errorf("requests = %d, want 4", n)
	}
}
