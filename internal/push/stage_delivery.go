package push

import (
	"context"

	"github.com/noahkiss/heraldo/api"
)

// deliveryStops returns why a delivery setting keeps ev from pushing at
// all: filed for the digest or dropped. Such an event never pushes, not even
// as the passive replace of a visible push of its key. "" otherwise.
func deliveryStops(ev *api.Event) string {
	d := ev.Deliver
	if d == nil {
		return ""
	}
	switch d.As {
	case "digest":
		return "digest only: delivery setting " + by(d)
	case "drop":
		return "dropped by delivery setting " + by(d)
	}
	return ""
}

func by(d *api.DeliverStamp) string {
	if d.By == nil || *d.By == "" {
		return "(none)"
	}
	return *d.By
}

// deliveryStage applies the delivery setting the server stamped at ingest
// (the deliver field). digest and drop end the pipeline; silent passes, and
// the table then keeps it in the inbox, as it does any passive event.
func (p *Pusher) deliveryStage(_ context.Context, r *Run) Verdict {
	d := r.Event.Deliver
	if d == nil {
		return Skip()
	}
	detail := map[string]any{"as": d.As, "badge": d.Badge}
	if d.By != nil {
		detail["by"] = *d.By
	}
	if why := deliveryStops(r.Event); why != "" {
		return Drop(why).With(detail)
	}
	if d.Refused != nil {
		wanted := ""
		if d.Wanted != nil {
			wanted = string(*d.Wanted)
		}
		return Pass("delivery setting " + by(d) + " wanted " + wanted + "; refused by the " + *d.Refused).With(detail)
	}
	if d.As == "silent" {
		was := "none"
		if d.LevelWas != nil {
			was = string(*d.LevelWas)
		}
		return Pass("silent by " + by(d) + " (was " + was + ")").With(detail)
	}
	if d.Filled != nil && d.By == nil {
		why := "level " + string(*d.Filled) + " filled: the event carried none"
		if d.NewSource != nil && *d.NewSource {
			why += " and its source is new"
		}
		return Pass(why).With(detail)
	}
	return Pass("banner by " + by(d)).With(detail)
}
