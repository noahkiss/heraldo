package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/noahkiss/heraldo/api"
)

type flapSnap struct {
	State     string    `json:"state"`
	Flapping  bool      `json:"flapping"`
	FlapCount int       `json:"flap_count"`
	FlapSince time.Time `json:"flap_since"`
}

func TestFlapState(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "h.db"))
	defer s.Close()
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	now := t0
	s.SetClock(func() time.Time { return now })
	s.SetFlapPolicy(func() FlapPolicy {
		return FlapPolicy{Enabled: true, Opens: 3, Window: time.Hour, Settle: 30 * time.Minute}
	})
	n := 0
	put := func(action string, at time.Time) flapSnap {
		t.Helper()
		n++
		now = at
		rec, _, err := s.Insert(ctx, typed(t, fmt.Sprintf("01K3ZX0000AAAAAAAAAAAAA%03d", n), at, api.KindIncident, "web", action))
		if err != nil {
			t.Fatal(err)
		}
		var e struct{ Incident flapSnap }
		if err := json.Unmarshal(rec.Event, &e); err != nil {
			t.Fatal(err)
		}
		return e.Incident
	}
	min := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	// Two opens in the window: not yet.
	put("open", min(0))
	put("resolve", min(5))
	if sn := put("open", min(10)); sn.Flapping {
		t.Fatalf("flapping after two opens: %+v", sn)
	}
	// An update does not count as an open.
	put("update", min(12))
	put("resolve", min(15))
	sn := put("open", min(20))
	if !sn.Flapping || sn.FlapCount != 3 || !sn.FlapSince.Equal(min(0)) {
		t.Fatalf("third open: %+v", sn)
	}
	put("resolve", min(25))
	if sn := put("open", min(30)); !sn.Flapping || sn.FlapCount != 4 {
		t.Fatalf("fourth open: %+v", sn)
	}
	if ok, _ := s.Flapping(ctx, "web", min(59)); !ok {
		t.Fatal("not flapping 29 min after the last open")
	}
	// Quiet for the settle time: not flapping, in Flapping and in State.
	if ok, _ := s.Flapping(ctx, "web", min(61)); ok {
		t.Fatal("flapping after the settle time")
	}
	now = min(61)
	st, err := s.State(ctx, nil)
	if err != nil || len(st.Incidents) != 1 {
		t.Fatalf("state %v %v", st, err)
	}
	var ev struct{ Incident map[string]any }
	_ = json.Unmarshal(st.Incidents[0], &ev)
	if _, ok := ev.Incident["flapping"]; ok || ev.Incident["state"] != "open" {
		t.Fatalf("settled state %s", st.Incidents[0])
	}
	// The next open starts over.
	put("resolve", min(62))
	if sn := put("open", min(63)); sn.Flapping {
		t.Fatalf("open after settle: %+v", sn)
	}
	// Opens outside the window do not add up.
	put("resolve", min(130))
	if sn := put("open", min(200)); sn.Flapping {
		t.Fatalf("spread-out opens: %+v", sn)
	}
	// Off: no flap state at all.
	s.SetFlapPolicy(nil)
	for i := 0; i < 4; i++ {
		put("resolve", min(201+2*i))
		if sn := put("open", min(202+2*i)); sn.Flapping {
			t.Fatalf("flapping with the policy off: %+v", sn)
		}
	}
}
