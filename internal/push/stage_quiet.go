package push

import (
	"context"
	"fmt"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/settings"
)

// quietStage lowers a push below quiet.floor to passive during quiet hours
// (quiet.enabled, quiet.start to quiet.end in quiet.tz). A passive push
// lands in Notification Center with no sound and no buzz. An event sent as
// critical is never lowered.
func (p *Pusher) quietStage(_ context.Context, r *Run) Verdict {
	m := p.prefs()
	if !settings.From[bool](m, "quiet.enabled") {
		return Pass("quiet hours off")
	}
	start, end := settings.From[string](m, "quiet.start"), settings.From[string](m, "quiet.end")
	tz := settings.From[string](m, "quiet.tz")
	floor := api.Level(settings.From[string](m, "quiet.floor"))
	detail := map[string]any{"window": start + "-" + end + " " + tz, "floor": floor}
	in, err := inQuiet(r.Now, start, end, tz)
	if err != nil {
		r.Log.Error("push: quiet hours", "err", err)
		return Pass("quiet hours: " + err.Error()).With(detail)
	}
	if !in {
		return Pass("outside quiet hours").With(detail)
	}
	if sentCritical(r.Event) {
		return Pass("quiet hours: critical is never lowered").With(detail)
	}
	lvl := r.Decision.Level
	if levelRank[lvl] >= levelRank[floor] {
		return Pass(fmt.Sprintf("quiet hours: %s is at or above the floor %s", lvl, floor)).With(detail)
	}
	if lvl == api.LevelPassive {
		return Pass("quiet hours: already passive").With(detail)
	}
	return Downgrade(api.LevelPassive, fmt.Sprintf("quiet hours: %s sent as passive", lvl)).With(detail)
}

// inQuiet reports whether now falls in the quiet window from start to end,
// HH:MM in the zone tz. A window whose end is before its start runs past
// midnight; one whose start and end are equal is empty.
func inQuiet(now time.Time, start, end, tz string) (bool, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return false, fmt.Errorf("time zone %q: %w", tz, err)
	}
	s, err := minutes(start)
	if err != nil {
		return false, err
	}
	e, err := minutes(end)
	if err != nil {
		return false, err
	}
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	switch {
	case s == e:
		return false, nil
	case s < e:
		return m >= s && m < e, nil
	default:
		return m >= s || m < e, nil
	}
}

// minutes is HH:MM as minutes after midnight.
func minutes(clock string) (int, error) {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return 0, fmt.Errorf("want HH:MM, got %q", clock)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// sentCritical reports whether the sender asked for critical. Quiet hours
// and Focus filters never lower such an event, whatever level the push
// table sends it at.
func sentCritical(ev *api.Event) bool {
	return ev.Level != nil && *ev.Level == api.LevelCritical
}
