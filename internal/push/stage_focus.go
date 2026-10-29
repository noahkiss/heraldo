package push

import (
	"context"
	"fmt"
	"slices"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// focusStage applies each device's Focus filter (devices.focus_filter, set
// by the app's SetFocusFilterIntent): on that device only, a push above the
// filter goes out at active, which the system Focus silences. Above the
// filter means time-sensitive under break_through critical, or anything
// above active from a project the filter does not list. An event sent as
// critical is never lowered. The stage records the devices it lowered; each
// one's delivery row says why.
func (p *Pusher) focusStage(_ context.Context, r *Run) Verdict {
	project := ""
	if r.Event.Project != nil {
		project = p.cfg.Rules().Alias(*r.Event.Project)
	}
	n := 0
	var lowered []map[string]any
	for _, dev := range r.Devices {
		if dev.FocusFilter == nil {
			continue
		}
		n++
		if sentCritical(r.Event) {
			continue
		}
		lvl, why := p.focusLevel(r.Decision.Level, dev.FocusFilter, project)
		if lvl == r.Decision.Level {
			continue
		}
		if r.DeviceLevel == nil {
			r.DeviceLevel, r.DeviceWhy = map[string]api.Level{}, map[string]string{}
		}
		r.DeviceLevel[dev.ID], r.DeviceWhy[dev.ID] = lvl, why
		lowered = append(lowered, map[string]any{"device": dev.ID, "level": lvl, "why": why})
	}
	switch {
	case n == 0:
		return Pass("no Focus filter")
	case sentCritical(r.Event):
		return Pass(fmt.Sprintf("critical: never lowered (%s in a Focus filter)", plural(n, "device")))
	case len(lowered) == 0:
		return Pass(fmt.Sprintf("%s breaks through the Focus filter on %s", r.Decision.Level, plural(n, "device")))
	}
	return Pass(fmt.Sprintf("focus: %s sent as active on %s", r.Decision.Level, plural(len(lowered), "device"))).
		With(map[string]any{"devices": lowered})
}

// focusLevel is the level a push at lvl goes out at on a device with
// filter f, for an event filed under project, and why it was lowered.
func (p *Pusher) focusLevel(lvl api.Level, f *store.FocusFilter, project string) (api.Level, string) {
	if levelRank[lvl] <= levelRank[api.LevelActive] {
		return lvl, ""
	}
	if len(f.Projects) > 0 {
		in := slices.ContainsFunc(f.Projects, func(fp string) bool { return p.cfg.Rules().Alias(fp) == project })
		if !in {
			return api.LevelActive, fmt.Sprintf("focus: %s sent as active (project not in the filter)", lvl)
		}
	}
	if f.BreakThrough == string(api.LevelCritical) && levelRank[lvl] < levelRank[api.LevelCritical] {
		return api.LevelActive, fmt.Sprintf("focus: %s sent as active", lvl)
	}
	return lvl, ""
}
