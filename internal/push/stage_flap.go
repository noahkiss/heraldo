package push

import (
	"context"
	"fmt"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// Flap grouping: the store marks an incident key flapping when it opens
// flap.opens times within flap.window (store.FlapPolicy, from the flap.*
// settings). The open that makes it flap pushes as usual, with a lead that
// says so. From then on its opens, updates and resolves go out passive with
// the key's collapse id: they update that one notification in place and do
// not page. Re-alerts stop. Once the key is quiet for flap.settle it is not
// flapping, and its next open pages again.

// FlapNote is the flap state a push shows.
type FlapNote struct {
	Count int
	// For is how long the key has flapped: from Since to the push.
	For   time.Duration
	Since time.Time
}

// flapStage applies flap grouping to an incident event.
func (p *Pusher) flapStage(ctx context.Context, r *Run) Verdict {
	ev := r.Event
	if ev.Kind != api.KindIncident || ev.Key == nil || ev.Incident == nil {
		return Pass("not an incident")
	}
	pol := p.cfg.Store.FlapPolicy()
	if !pol.Enabled {
		return Pass("flap grouping off")
	}
	if ev.Level != nil && *ev.Level == api.LevelCritical {
		return Pass("critical: flap grouping does not quiet it")
	}
	inc := ev.Incident
	flapping := inc.Flapping != nil && *inc.Flapping
	if r.OpenFor > 0 {
		// A re-alert or a reminder pushes a stored event: its snapshot may
		// be from before the key flapped, or from before it settled.
		live, err := p.cfg.Store.Flapping(ctx, *ev.Key, r.Now)
		if err != nil {
			r.Log.Error("push: flap state", "err", err)
			return Pass("flap lookup failed; pushed anyway")
		}
		flapping = live
	}
	if !flapping {
		return Pass("not flapping")
	}
	note := FlapNote{}
	if inc.FlapCount != nil {
		note.Count = *inc.FlapCount
	}
	if inc.FlapSince != nil {
		note.Since = *inc.FlapSince
		note.For = r.Now.Sub(note.Since)
	}
	detail := map[string]any{"flap_count": note.Count}
	if !note.Since.IsZero() {
		detail["flap_since"] = note.Since.UTC().Format(time.RFC3339)
	}
	switch {
	case r.Reminder != "":
		return Pass("flapping; a snooze reminder pushes anyway").With(detail)
	case r.OpenFor > 0:
		return Drop("flapping: no re-alert").With(detail)
	}
	if note.Count > 0 {
		r.Flap = &note
	}
	opening := ev.Action == nil || *ev.Action == api.EventActionOpen
	if opening && note.Count > 0 && note.Count <= pol.Opens {
		return Pass(fmt.Sprintf("now flapping: %d opens; later changes update this notification", note.Count)).With(detail)
	}
	// The flap's notification is the one to update: it goes out passive
	// under the key's collapse id even after a resolve, so the replace
	// check does not apply.
	r.Decision.Replace = false
	if r.Decision.Level == api.LevelPassive {
		return Pass("flapping: updates the notification in place").With(detail)
	}
	return Downgrade(api.LevelPassive, "flapping: updates the notification in place, no page").With(detail)
}
