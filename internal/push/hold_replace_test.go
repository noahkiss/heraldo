package push

import (
	"errors"
	"testing"
)

func TestHoldForwardReplace(t *testing.T) {
	r := newRig(t, 0)
	r.register(t, prodToken, "production")
	// A hold forward with its collapse string pushes with that collapse id.
	id := r.post(t, bridgeTok, `{"source":"s","title":"[not triaged in 30 min] disk full","level":"time-sensitive",
		"details":{"tee":{"id":"n-1","hold":{"state":"forward","reason":"deadline","ids":["h-1"],"collapse":"hold:h-1"}}}}`)
	req, a := lastAlert(t, r.prod)
	if req.CollapseID != CollapseID("hold:h-1") || a.APS.InterruptionLevel != "time-sensitive" {
		t.Fatalf("forward %+v %+v", req, a)
	}
	// Replace sends quietly through the same collapse id.
	n, err := r.p.Replace(t.Context(), id, "Disks filling on nas", "Three disks are over 90%.")
	if err != nil || n != 1 {
		t.Fatalf("replace %d %v", n, err)
	}
	up, b := lastAlert(t, r.prod)
	if up.CollapseID != req.CollapseID || b.APS.Alert.Title != "Disks filling on nas" ||
		b.APS.Alert.Body != "Three disks are over 90%." || b.APS.InterruptionLevel != "passive" || b.APS.Sound != "" {
		t.Fatalf("replace %+v %+v", up, b)
	}
	if d := r.delivery(t, id); len(d) != 1 || d[0].Level == nil || string(*d[0].Level) != "time-sensitive" {
		t.Fatalf("delivery changed: %+v", d)
	}

	// A keyed event replaces through its key's collapse id.
	keyed := r.post(t, writeTok, `{"source":"web","title":"web down","key":"web:1"}`)
	if _, err := r.p.Replace(t.Context(), keyed, "", "still down"); err != nil {
		t.Fatal(err)
	}
	if up, _ := lastAlert(t, r.prod); up.CollapseID != CollapseID("web:1") {
		t.Fatalf("keyed replace %+v", up)
	}

	// Nothing that never reached a device is shown: an inbox-only event.
	quiet := r.post(t, writeTok, `{"source":"web","title":"fyi","level":"passive","key":"web:2"}`)
	before := len(r.prod.requests())
	if _, err := r.p.Replace(t.Context(), quiet, "x", "y"); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("replace of an inbox-only event: %v", err)
	}
	if len(r.prod.requests()) != before {
		t.Fatal("a refused replace pushed")
	}
}
