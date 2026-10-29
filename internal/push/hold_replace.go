package push

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/decide"
)

// A hold forward carries hold.collapse, the string its apns-collapse-id is
// made from: "hold:<entry>" on a forward of an event without a key, or the
// group's anchor on a forward the LLM folded (hold.fold). The LLM's
// decisions role uses it to replace a forwarded notification after the
// forward went out, so the forward itself never waits for the LLM.

// HoldCollapse is the collapse string of a hold forward, or "" when the
// push keeps its own (the key's, or none).
func HoldCollapse(ev *api.Event) string {
	if ev.Hold == nil {
		return ""
	}
	h := *ev.Hold
	c, _ := h["collapse"].(string)
	if c == "" {
		return ""
	}
	if fold, _ := h["fold"].(bool); fold || ev.Key == nil || *ev.Key == "" {
		return c
	}
	return ""
}

// Replace errors.
var (
	// ErrNotDecided: the pusher has not run the event yet.
	ErrNotDecided = decide.ErrNotDecided
	// ErrNotDelivered: the event reached no device.
	ErrNotDelivered = errors.New("the event reached no device")
)

// Replace sends a quiet replacement of event id's notification, through
// its collapse id, with title and summary ("" keeps the event's), to every
// active device, and returns how many took it. It records no delivery and
// changes no trace. It refuses an event the deliver stage did not deliver,
// so it never shows what a mute, quiet hours or the push table kept back,
// and an event with no collapse id.
func (p *Pusher) Replace(ctx context.Context, id, title, summary string) (int, error) {
	rows, err := p.cfg.Store.Decisions(ctx, id)
	if err != nil {
		return 0, err
	}
	decided, delivered := false, false
	for _, r := range rows {
		if r.Stage == "deliver" {
			decided, delivered = true, r.Outcome == string(OutcomePass)
		} else if r.Outcome == string(OutcomeDrop) || r.Outcome == string(OutcomeDelay) || r.Outcome == string(OutcomeMerge) {
			decided = true
		}
	}
	if !decided {
		return 0, ErrNotDecided
	}
	if !delivered {
		return 0, ErrNotDelivered
	}
	rec, err := p.cfg.Store.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	var ev api.Event
	if err := json.Unmarshal(rec.Event, &ev); err != nil {
		return 0, err
	}
	collapse := HoldCollapse(&ev)
	if collapse == "" && ev.Key != nil && *ev.Key != "" {
		collapse = *ev.Key
	}
	if collapse == "" {
		return 0, errors.New("the event has no collapse id")
	}
	devices, err := p.cfg.Store.Devices(ctx, true)
	if err != nil {
		return 0, err
	}
	if title != "" {
		ev.Title = &title
	}
	if summary != "" {
		ev.Summary = &summary
	}
	x := Extra{Icon: p.icon(ctx, &ev)}
	body, h, err := Build(&ev, Decision{Push: true, Level: api.LevelPassive}, x)
	if err != nil {
		return 0, err
	}
	x.ContentAvailable = true
	mac, _, err := Build(&ev, Decision{Push: true, Level: api.LevelPassive}, x)
	if err != nil {
		return 0, err
	}
	h.CollapseID = CollapseID(collapse)
	n := 0
	for _, dev := range devices {
		b := body
		if ContentAvailableFor(dev) {
			b = mac
		}
		if out := p.deliver(ctx, dev, b, h); out.Outcome == "sent" {
			n++
		}
	}
	return n, nil
}
