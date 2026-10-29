package push

import (
	"context"
	"encoding/json"
	"time"

	"github.com/noahkiss/heraldo/api"
)

// Snooze reuses mutes: a mute with remind set holds back its key's pushes
// like any mute, and when it ends the pusher reminds of the key once, if
// its incident is still open.

// snoozeStage marks a snooze reminder in the trace.
func (p *Pusher) snoozeStage(_ context.Context, r *Run) Verdict {
	if r.Reminder == "" {
		return Pass("not a snooze reminder")
	}
	key := ""
	if r.Event.Key != nil {
		key = *r.Event.Key
	}
	return Pass("snooze " + r.Reminder + " ended; " + key + " is still open").With(map[string]any{"mute": r.Reminder})
}

// Remind sends the reminder of every snooze that ended by now: the last
// event of its key, again at its level, while the key's incident is open
// (not acked or resolved). Each snooze reminds once, however long the server
// was down. Run calls it on the re-alert ticker.
func (p *Pusher) Remind(ctx context.Context) {
	now := p.cfg.Now()
	due, err := p.cfg.Store.DueReminders(ctx, now)
	if err != nil {
		p.cfg.Logger.Error("push: due snoozes", "err", err)
		return
	}
	for _, m := range due {
		// Claim first: a failed push is not retried every tick.
		if ok, err := p.cfg.Store.ClaimReminder(ctx, m.ID, now); err != nil || !ok {
			if err != nil {
				p.cfg.Logger.Error("push: claim snooze", "mute", m.ID, "err", err)
			}
			continue
		}
		log := p.cfg.Logger.With("snooze", m.ID, "key", m.Key)
		inc, open, err := p.cfg.Store.OpenIncident(ctx, m.Key)
		if err != nil {
			log.Error("push: snooze incident", "err", err)
			continue
		}
		if !open {
			log.Info("push: snooze ended; the key is not open, no reminder")
			continue
		}
		rec, err := p.cfg.Store.Get(ctx, inc.LastEvent)
		if err != nil {
			log.Error("push: snooze event", "err", err)
			continue
		}
		var ev api.Event
		if err := json.Unmarshal(rec.Event, &ev); err != nil {
			log.Error("push: decode event", "err", err)
			continue
		}
		log = log.With("event", ev.Id)
		p.run(ctx, &Run{Rec: rec, Event: &ev, Now: now, OpenFor: max(now.Sub(inc.OpenedAt), time.Minute),
			Reminder: m.ID, Log: log})
	}
}
