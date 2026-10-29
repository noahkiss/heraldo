package push

import (
	"context"
	"fmt"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/store"
)

// The stages every push has had since stage 4, in pipeline order. The list
// is in pipeline.go.

// holdStage stops a store-only event and one the alert hold still holds.
func (p *Pusher) holdStage(_ context.Context, r *Run) Verdict {
	switch holdStops(r.Event) {
	case "store-only":
		return Drop("store-only: inbox only")
	case "held":
		return Drop("held for triage")
	}
	if r.Event.Hold == nil {
		return Pass("not held")
	}
	h := *r.Event.Hold
	st, _ := h["state"].(string)
	if c := HoldCollapse(r.Event); c != "" {
		// A hold forward the LLM may replace later (hold_replace.go).
		r.CollapseID = CollapseID(c)
	}
	if fwd, _ := h["forwarded"].(bool); fwd {
		return Pass("forwarded by the hold").With(map[string]any{"state": st})
	}
	return Pass("hold " + orNone(st)).With(map[string]any{"state": st})
}

// expiryStage stops an event past its expires_at.
func (p *Pusher) expiryStage(_ context.Context, r *Run) Verdict {
	if r.Event.ExpiresAt == nil {
		return Pass("no expiry")
	}
	detail := map[string]any{"expires_at": r.Event.ExpiresAt}
	if expired(r.Event, r.Now) {
		return Drop("expired before push").With(detail)
	}
	return Pass("not expired").With(detail)
}

// tableStage applies the push table (Decide) and sets r.Decision.
func (p *Pusher) tableStage(_ context.Context, r *Run) Verdict {
	d := decideTable(r.Event)
	r.Decision = d
	if !d.Push {
		if d.Reason == "passive" {
			return Drop("passive: inbox only")
		}
		return Drop("no push: " + d.Reason)
	}
	detail := map[string]any{"level": d.Level}
	if d.Downgraded {
		return Downgrade(d.Level, "critical sent as time-sensitive: no rule set it and the token lacks the critical capability").With(detail)
	}
	if d.Level == api.LevelCritical {
		detail["critical_by"] = CriticalBy(r.Event)
		return Pass("push table: critical, allowed by " + CriticalBy(r.Event)).With(detail)
	}
	if d.Replace {
		return Pass("passive: replaces a visible push of its key").With(detail)
	}
	return Pass("push table: " + string(d.Level)).With(detail)
}

// replaceStage sends a passive push only when it replaces a visible push
// of its key. A re-alert skips the check.
func (p *Pusher) replaceStage(ctx context.Context, r *Run) Verdict {
	if !r.Decision.Replace {
		return Pass("not a replace")
	}
	if r.OpenFor > 0 {
		return Pass("re-alert")
	}
	ok, err := p.cfg.Store.ReplacesVisible(ctx, *r.Event.Key, r.Rec.Seq)
	if err != nil {
		r.Log.Error("push: earlier pushes", "err", err)
		return Drop("replace check failed")
	}
	if !ok {
		return Drop("passive: inbox only (no visible push to replace)")
	}
	return Pass("replaces a visible push of " + *r.Event.Key)
}

// devicesStage loads the active devices.
func (p *Pusher) devicesStage(ctx context.Context, r *Run) Verdict {
	devices, err := p.cfg.Store.Devices(ctx, true)
	if err != nil {
		r.Log.Error("push: devices", "err", err)
		return Drop("device lookup failed")
	}
	if len(devices) == 0 {
		return Drop("no active devices")
	}
	r.Devices = devices
	return Pass(plural(len(devices), "active device"))
}

// muteStage holds back an event a mute matches: by source, key, or project.
func (p *Pusher) muteStage(ctx context.Context, r *Run) Verdict {
	ev := r.Event
	key := ""
	if ev.Key != nil {
		key = *ev.Key
	}
	// A project mute names the alias target; the event may carry the old
	// name.
	project := ""
	if ev.Project != nil {
		project = p.cfg.Rules().Alias(*ev.Project)
	}
	m, ok, err := p.cfg.Store.MatchMute(ctx, ev.Source, key, project, p.cfg.Now())
	if err == nil && !ok && key != "" && ev.Kind == api.KindCheckin {
		// A check-in's own events fall under its check's incident key, so
		// one mute on checkin:<slug> covers the incident and the runs.
		m, ok, err = p.cfg.Store.MatchMute(ctx, ev.Source, checkin.IncidentPrefix+key, project, p.cfg.Now())
	}
	if err != nil {
		r.Log.Error("push: mutes and inhibit rules", "err", err)
		r.skipInhibit = true
		return Pass("mute lookup failed; pushed anyway")
	}
	if !ok {
		return Pass("no mute")
	}
	why := "muted by " + m.ID
	if m.Project != "" {
		why += " (project)"
	}
	return p.holdBack(ctx, r, why).With(map[string]any{"mute": m.ID})
}

// inhibitStage holds back an event whose key an inhibit rule covers while
// the rule's parent incident is open.
func (p *Pusher) inhibitStage(ctx context.Context, r *Run) Verdict {
	if r.skipInhibit {
		return Pass("not checked")
	}
	set := p.cfg.Rules()
	if r.Event.Key == nil || *r.Event.Key == "" || len(set.InhibitRules()) == 0 {
		return Pass("no inhibit rule")
	}
	open, err := p.cfg.Store.OpenIncidentKeys(ctx)
	if err != nil {
		r.Log.Error("push: mutes and inhibit rules", "err", err)
		return Pass("inhibit lookup failed; pushed anyway")
	}
	rule, parent, ok := set.Inhibited(*r.Event.Key, open)
	if !ok {
		return Pass("not inhibited")
	}
	return p.holdBack(ctx, r, fmt.Sprintf("inhibited by %s: %s is open", rule, parent)).
		With(map[string]any{"rule": rule, "parent": parent})
}

// holdBack drops a push a mute or an inhibit rule holds back. A first push
// records skipped on every device; a re-alert leaves the event's delivery as
// it was.
func (p *Pusher) holdBack(ctx context.Context, r *Run, why string) Verdict {
	if r.OpenFor > 0 {
		r.Log.Info("push: re-alert held back", "why", why)
		return Drop(why)
	}
	for _, dev := range r.Devices {
		out := store.Delivery{EventID: r.Event.Id, DeviceID: dev.ID, Outcome: "skipped", Level: string(r.Decision.Level),
			Error: why, At: p.cfg.Now()}
		if err := p.cfg.Store.RecordDelivery(context.WithoutCancel(ctx), out); err != nil {
			r.Log.Error("push: record delivery", "device", dev.ID, "err", err)
		}
	}
	r.Log.Info("push: held back", "why", why)
	return Drop(why)
}

// deliverStage sends the push to every active device and records each
// outcome. A re-alert overwrites the event's delivery with its own.
func (p *Pusher) deliverStage(ctx context.Context, r *Run) Verdict {
	ev, d := r.Event, r.Decision
	x := Extra{Icon: p.icon(ctx, ev), OpenFor: r.OpenFor, Flap: r.Flap}
	scope, projects := p.badgeScope()
	if n, err := p.cfg.Store.BadgeFor(ctx, BadgeCursor, scope, projects); err != nil {
		r.Log.Error("push: badge", "err", err)
	} else {
		x.Badge = &n
	}
	// One payload per level, since the focus stage may lower a device's
	// push, and per platform, since a Mac gets content-available.
	type built struct {
		body []byte
		h    Headers
	}
	type variant struct {
		lvl api.Level
		mac bool
	}
	payloads := map[variant]built{}
	payload := func(lvl api.Level, mac bool) (built, error) {
		v := variant{lvl, mac}
		if b, ok := payloads[v]; ok {
			return b, nil
		}
		dl, xv := d, x
		dl.Level, xv.ContentAvailable = lvl, mac
		body, h, err := Build(ev, dl, xv)
		if r.CollapseID != "" {
			h.CollapseID = r.CollapseID
		}
		payloads[v] = built{body, h}
		return built{body, h}, err
	}
	if _, err := payload(d.Level, false); err != nil {
		r.Log.Error("push: payload", "err", err)
		return Drop("payload: " + err.Error())
	}
	r.delivered = true
	sent, lowered := 0, 0
	var devs []map[string]any
	for _, dev := range r.Devices {
		// A critical push goes as critical only to the devices that allow
		// critical alerts; the rest get it as time-sensitive.
		lvl, why := d.Level, ""
		if d.Downgraded {
			why = "critical sent as time-sensitive"
		}
		if l, ok := r.DeviceLevel[dev.ID]; ok {
			lvl, why = l, r.DeviceWhy[dev.ID]
		}
		if lvl == api.LevelCritical && !AllowsCritical(dev) {
			lvl, why = api.LevelTimeSensitive, "critical sent as time-sensitive: critical alerts are off on this device"
			lowered++
		}
		b, err := payload(lvl, ContentAvailableFor(dev))
		if err != nil {
			r.Log.Error("push: payload", "device", dev.ID, "err", err)
			b = payloads[variant{d.Level, false}]
			lvl = d.Level
		}
		out := p.deliver(ctx, dev, b.body, b.h)
		out.EventID, out.DeviceID, out.Level = ev.Id, dev.ID, string(lvl)
		if out.Outcome == "sent" && why != "" {
			out.Error = why
		}
		out.At = p.cfg.Now()
		if err := p.cfg.Store.RecordDelivery(context.WithoutCancel(ctx), out); err != nil {
			r.Log.Error("push: record delivery", "device", dev.ID, "err", err)
		}
		if out.Outcome != "sent" {
			r.Log.Warn("push: not delivered", "device", dev.ID, "error", out.Error, "attempts", out.Attempts)
		} else {
			sent++
		}
		row := map[string]any{"device": dev.ID, "outcome": out.Outcome, "attempts": out.Attempts}
		if out.Error != "" {
			row["error"] = out.Error
		}
		if lvl != d.Level {
			row["level"] = lvl
		}
		devs = append(devs, row)
	}
	detail := map[string]any{"level": d.Level, "devices": devs, "badge_scope": scope}
	if x.Badge != nil {
		detail["badge"] = *x.Badge
	}
	verb := "pushed"
	if r.OpenFor > 0 {
		verb = "re-alerted"
		detail["open_for_s"] = int(r.OpenFor.Seconds())
	}
	n := len(r.Devices)
	tail := ""
	if lowered > 0 {
		tail = fmt.Sprintf("; time-sensitive on %s without critical alerts", plural(lowered, "device"))
	}
	switch {
	case sent == 0:
		return Drop(fmt.Sprintf("push failed on %s", plural(n, "device"))).With(detail)
	case sent < n:
		return Pass(fmt.Sprintf("%s %s to %d of %s%s", verb, d.Level, sent, plural(n, "device"), tail)).With(detail)
	}
	return Pass(fmt.Sprintf("%s %s to %s%s", verb, d.Level, plural(n, "device"), tail)).With(detail)
}

// AllowsCritical reports whether dev reported that it allows critical
// alerts. Unknown counts as no: the app reports it only when its build has
// the critical-alerts entitlement.
func AllowsCritical(dev store.Device) bool {
	return dev.CriticalAlerts != nil && *dev.CriticalAlerts
}

// plural writes n and noun, adding an s unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
