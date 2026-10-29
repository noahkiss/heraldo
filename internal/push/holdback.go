package push

import (
	"context"
	"fmt"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
	"github.com/noahkiss/heraldo/internal/rules"
	"github.com/noahkiss/heraldo/internal/store"
)

// HeldBack reports why a mute or an inhibit rule holds back ev at now, or
// "" when nothing does. It is the mute and inhibit stages' lookup for work
// beside the pipeline, such as an escalation, which runs with push off.
// Keep it in step with muteStage and inhibitStage.
func HeldBack(ctx context.Context, st *store.Store, set *rules.Set, ev *api.Event, now time.Time) (string, error) {
	key := ""
	if ev.Key != nil {
		key = *ev.Key
	}
	project := ""
	if ev.Project != nil {
		project = set.Alias(*ev.Project)
	}
	m, ok, err := st.MatchMute(ctx, ev.Source, key, project, now)
	if err == nil && !ok && key != "" && ev.Kind == api.KindCheckin {
		m, ok, err = st.MatchMute(ctx, ev.Source, checkin.IncidentPrefix+key, project, now)
	}
	if err != nil {
		return "", err
	}
	if ok {
		why := "muted by " + m.ID
		if m.Project != "" {
			why += " (project)"
		}
		return why, nil
	}
	if key == "" || len(set.InhibitRules()) == 0 {
		return "", nil
	}
	open, err := st.OpenIncidentKeys(ctx)
	if err != nil {
		return "", err
	}
	if rule, parent, ok := set.Inhibited(key, open); ok {
		return fmt.Sprintf("inhibited by %s: %s is open", rule, parent), nil
	}
	return "", nil
}
