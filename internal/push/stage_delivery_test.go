package push

import (
	"context"
	"strings"
	"testing"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/delivery"
)

// addRow stores a delivery setting as POST /v1/delivery would.
func (r *rig) addRow(t *testing.T, id string, m delivery.Match, as delivery.Outcome) {
	t.Helper()
	now := r.clock.now()
	if err := r.st.AddDelivery(context.Background(), delivery.Row{ID: id, Match: m, Deliver: as,
		Badge: delivery.BadgeAuto, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryStage(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.set(t, "delivery.enabled", "true")
	r.set(t, "digest.enabled", "true")
	r.addRow(t, "dlv_silent", delivery.Match{Source: "printer"}, delivery.Silent)
	r.addRow(t, "dlv_digest", delivery.Match{Source: "maint"}, delivery.Digest)
	r.addRow(t, "dlv_drop", delivery.Match{Source: "tests"}, delivery.Drop)
	r.addRow(t, "dlv_loud", delivery.Match{Source: "uptime"}, delivery.Drop)

	for _, c := range []struct {
		body, stages, why string
		pushed            bool
	}{
		{`{"source":"printer","title":"done","level":"active"}`, "hold:pass expiry:pass delivery:pass table:drop",
			"silent by dlv_silent (was active)", false},
		{`{"source":"maint","title":"due"}`, "hold:pass expiry:pass delivery:drop",
			"digest only: delivery setting dlv_digest", false},
		{`{"source":"tests","title":"t","key":"k1"}`, "hold:pass expiry:pass delivery:drop",
			"dropped by delivery setting dlv_drop", false},
		// The floor keeps a time-sensitive event at its level.
		{`{"source":"uptime","title":"down","level":"time-sensitive"}`, "",
			"delivery setting dlv_loud wanted drop; refused by the time-sensitive floor", true},
		// No row: no trace row, as before delivery settings.
		{`{"source":"other","title":"hi","level":"active"}`, "", "", true},
		// A new source without a level gets delivery.new_source_level.
		{`{"source":"fresh","title":"hi"}`, "hold:pass expiry:pass delivery:pass table:drop",
			"level passive filled: the event carried none and its source is new", false},
	} {
		before := len(r.prod.requests())
		id := r.post(t, writeTok, c.body)
		tr := r.why(t, id)
		got := stagesOf(tr)
		if c.stages != "" && !strings.HasPrefix(got, c.stages) {
			t.Errorf("%s: stages %s, want %s", c.body, got, c.stages)
		}
		var reason string
		for _, d := range tr.Decisions {
			if d.Stage == "delivery" {
				reason = d.Reason
			}
		}
		if reason != c.why {
			t.Errorf("%s: delivery reason %q, want %q", c.body, reason, c.why)
		}
		if pushed := len(r.prod.requests()) > before; pushed != c.pushed {
			t.Errorf("%s: pushed %v, want %v", c.body, pushed, c.pushed)
		}
	}
}

// A keyed event filed for the digest or dropped never replaces a visible
// push of its key; a keyed silent event does, quietly, as any passive
// update of a visible push does.
func TestDeliveryKeyedReplace(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	r.post(t, writeTok, `{"source":"job","key":"k","title":"first"}`)
	r.post(t, writeTok, `{"source":"job2","key":"k2","title":"first"}`)
	if n := len(r.prod.requests()); n != 2 {
		t.Fatalf("%d pushes", n)
	}
	r.set(t, "delivery.enabled", "true")
	r.addRow(t, "dlv_s", delivery.Match{Source: "job"}, delivery.Silent)
	r.addRow(t, "dlv_d", delivery.Match{Source: "job2"}, delivery.Drop)
	r.post(t, writeTok, `{"source":"job","key":"k","title":"second"}`)
	reqs := r.prod.requests()
	if len(reqs) != 3 {
		t.Fatalf("silent keyed: %d pushes, want 3", len(reqs))
	}
	if p := payloadOf(t, reqs[2]); p.APS.InterruptionLevel != "passive" {
		t.Fatalf("silent replace at %s", p.APS.InterruptionLevel)
	}
	r.post(t, writeTok, `{"source":"job2","key":"k2","title":"second"}`)
	if n := len(r.prod.requests()); n != 3 {
		t.Fatalf("dropped keyed event pushed: %d", n)
	}
	// Decide agrees with the pipeline.
	key := "k2"
	ev := api.Event{Source: "job2", Kind: api.KindMessage, Key: &key,
		Deliver: &api.DeliverStamp{As: "drop", Badge: "auto"}}
	if d := Decide(&ev, r.clock.now()); d.Push {
		t.Fatalf("Decide pushes a dropped event: %+v", d)
	}
}
