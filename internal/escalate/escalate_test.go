package escalate

import (
	"testing"
	"time"
)

func TestLimits(t *testing.T) {
	cases := []struct{ retry, expire, wantRetry, wantExpire time.Duration }{
		{time.Minute, time.Hour, time.Minute, time.Hour},
		{10 * time.Second, time.Hour, MinRetry, time.Hour},
		{time.Minute, 5 * time.Hour, time.Minute, MaxExpire},
		{10 * time.Minute, time.Minute, 10 * time.Minute, 10 * time.Minute},
	}
	for _, c := range cases {
		r, e := Limits(c.retry, c.expire)
		if r != c.wantRetry || e != c.wantExpire {
			t.Errorf("Limits(%v, %v) = %v, %v", c.retry, c.expire, r, e)
		}
	}
}

func TestCutRunes(t *testing.T) {
	if got := cutRunes("ééé", 2); got != "é…" {
		t.Errorf("cut = %q", got)
	}
	if got := cutRunes("ok", 2); got != "ok" {
		t.Errorf("cut = %q", got)
	}
}
