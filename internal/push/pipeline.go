package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// stages is the push pipeline, in order. The pusher runs each stage on an
// event until one returns a verdict other than pass or downgrade, and every
// stage that runs writes one row to the event's trace (push_decisions,
// GET /v1/events/{id}/why).
//
// To add a stage: write stage_<name>.go with
//
//	func (p *Pusher) <name>Stage(ctx context.Context, r *Run) Verdict
//
// and add one line below, at the place it acts. A stage reads and may
// change r; it returns Pass, Downgrade, Delay, Merge or Drop with a reason a
// person reads. A stage that returns Delay or Merge owns the later push: the
// pipeline stops and pushes nothing now. Downgrade never raises the level:
// the pipeline refuses a louder one. deliver stays last.
var stages = []stage{
	{"hold", (*Pusher).holdStage},
	{"expiry", (*Pusher).expiryStage},
	{"table", (*Pusher).tableStage},
	{"snooze", (*Pusher).snoozeStage},
	{"flap", (*Pusher).flapStage},
	{"replace", (*Pusher).replaceStage},
	{"devices", (*Pusher).devicesStage},
	{"mute", (*Pusher).muteStage},
	{"inhibit", (*Pusher).inhibitStage},
	{"bundle", (*Pusher).bundleStage},
	{"quiet", (*Pusher).quietStage},
	{"focus", (*Pusher).focusStage},
	{"deliver", (*Pusher).deliverStage},
}

// stage is one named step of the push pipeline.
type stage struct {
	name string
	run  func(p *Pusher, ctx context.Context, r *Run) Verdict
}

// Outcome is what a stage decided.
type Outcome string

// The outcomes a stage returns.
const (
	// OutcomePass lets the event go on to the next stage.
	OutcomePass Outcome = "pass"
	// OutcomeDowngrade lowers the push's level and goes on.
	OutcomeDowngrade Outcome = "downgrade"
	// OutcomeDelay stops the pipeline; the stage pushes later.
	OutcomeDelay Outcome = "delay"
	// OutcomeMerge stops the pipeline; the stage folds the event into a
	// bundle.
	OutcomeMerge Outcome = "merge"
	// OutcomeDrop stops the pipeline; nothing is pushed.
	OutcomeDrop Outcome = "drop"
)

// Verdict is one stage's answer.
type Verdict struct {
	Outcome Outcome
	// Reason is one line a person reads, such as "muted by mute_x".
	Reason string
	// Level is the new level of a downgrade.
	Level api.Level
	// Until is when a delayed event pushes.
	Until time.Time
	// Bundle names the bundle a merged event joins.
	Bundle string
	// Detail is extra data for the trace row; nil means none.
	Detail map[string]any
}

// Pass goes on to the next stage.
func Pass(reason string) Verdict { return Verdict{Outcome: OutcomePass, Reason: reason} }

// Drop stops the pipeline: the event does not push.
func Drop(reason string) Verdict { return Verdict{Outcome: OutcomeDrop, Reason: reason} }

// Downgrade lowers the push to level and goes on.
func Downgrade(level api.Level, reason string) Verdict {
	return Verdict{Outcome: OutcomeDowngrade, Level: level, Reason: reason}
}

// Delay stops the pipeline; the stage pushes the event at until.
func Delay(until time.Time, reason string) Verdict {
	return Verdict{Outcome: OutcomeDelay, Until: until, Reason: reason}
}

// Merge stops the pipeline; the stage folds the event into bundle.
func Merge(bundle, reason string) Verdict {
	return Verdict{Outcome: OutcomeMerge, Bundle: bundle, Reason: reason}
}

// With returns v with detail added to the trace row.
func (v Verdict) With(detail map[string]any) Verdict {
	v.Detail = detail
	return v
}

// Run is one pass of an event through the pipeline. Stages share it.
type Run struct {
	Rec   store.Record
	Event *api.Event
	// Decision is the push so far; the table stage sets it.
	Decision Decision
	Now      time.Time
	// OpenFor is how long the incident has been open, on a re-alert; 0 on
	// a first push.
	OpenFor time.Duration
	// Devices are the active devices; the devices stage sets them.
	Devices []store.Device
	// DeviceLevel lowers the push for one device, by device id, below
	// Decision.Level; DeviceWhy says why, for its delivery row. The focus
	// stage sets them.
	DeviceLevel map[string]api.Level
	DeviceWhy   map[string]string
	Log         *slog.Logger
	// CollapseID replaces the push's apns-collapse-id when set; the bundle
	// stage sets it.
	CollapseID string
	// Reminder is the snooze (a mute id) whose end pushes this event
	// again; "" otherwise. A reminder also sets OpenFor.
	Reminder string
	// Flap is the flap state the push shows; the flap stage sets it.
	Flap *FlapNote

	// after runs once the deliver stage delivered, after the trace is
	// recorded: work that must not delay the push, such as the bundle
	// text hook.
	after []func()

	// skipInhibit is set when the mute lookup failed: as before stages, the
	// push then goes out without an inhibit check.
	skipInhibit bool
	// delivered is set when the deliver stage ran.
	delivered bool
}

// levelRank orders levels from quiet to loud.
var levelRank = map[api.Level]int{
	api.LevelPassive: 0, api.LevelActive: 1, api.LevelTimeSensitive: 2, api.LevelCritical: 3,
}

// run sends r.Event through the stages and records the trace. A re-alert
// records its trace only when it reaches deliver, so a held-back re-alert
// leaves the trace of the push in force, as it leaves the delivery.
func (p *Pusher) run(ctx context.Context, r *Run) {
	var rows []store.DecisionRow
	for _, s := range stages {
		v := s.run(p, ctx, r)
		if v.Outcome == OutcomeDowngrade {
			if cur, ok := levelRank[r.Decision.Level]; ok && levelRank[v.Level] > cur {
				v = Pass("refused " + string(v.Level) + ": louder than " + string(r.Decision.Level)).With(v.Detail)
			} else {
				r.Decision.Level = v.Level
			}
		}
		rows = append(rows, row(s.name, v, p.cfg.Now()))
		if v.Outcome != OutcomePass && v.Outcome != OutcomeDowngrade {
			break
		}
	}
	if r.OpenFor > 0 && !r.delivered {
		return
	}
	if err := p.cfg.Store.RecordDecisions(context.WithoutCancel(ctx), r.Event.Id, rows); err != nil {
		r.Log.Error("push: record decisions", "err", err)
	}
	if r.delivered {
		for _, f := range r.after {
			f()
		}
	}
}

// row turns a verdict into a trace row.
func row(name string, v Verdict, at time.Time) store.DecisionRow {
	d := map[string]any{}
	for k, x := range v.Detail {
		d[k] = x
	}
	switch v.Outcome {
	case OutcomeDowngrade:
		d["level"] = v.Level
	case OutcomeDelay:
		d["until"] = v.Until.UTC().Format(time.RFC3339)
	case OutcomeMerge:
		d["bundle"] = v.Bundle
	}
	out := store.DecisionRow{Stage: name, Outcome: string(v.Outcome), Reason: v.Reason, At: at}
	if len(d) > 0 {
		out.Detail, _ = json.Marshal(d)
	}
	return out
}
