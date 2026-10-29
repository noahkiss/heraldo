// Package routine says which events are routine: the ones the push table
// never pushes by kind and action. A check-in that did not fail, and every
// progress and status event, is routine. Failing check-ins, messages,
// digests and incidents are not. Clients keep routine events out of the
// main stream; GET /v1/events filters them with routine=exclude or only.
//
// store.RoutineWhere is the same rule in SQL; the store's tests check that
// the two agree.
package routine

import "github.com/noahkiss/heraldo/api"

// Filter values for GET /v1/events routine=.
const (
	Exclude = "exclude"
	Only    = "only"
)

// Is reports whether ev is routine.
func Is(ev *api.Event) bool {
	switch ev.Kind {
	case api.KindProgress, api.KindStatus:
		return true
	case api.KindCheckin:
		return !FailingCheckin(ev)
	}
	return false
}

// FailingCheckin reports whether ev is a check-in that pushes: action fail,
// or a non-zero exit code on anything but log.
func FailingCheckin(ev *api.Event) bool {
	if ev.Kind != api.KindCheckin {
		return false
	}
	action := ""
	if ev.Action != nil {
		action = string(*ev.Action)
	}
	if action == "fail" {
		return true
	}
	return action != "log" && ev.Checkin != nil && ev.Checkin.ExitCode != nil && *ev.Checkin.ExitCode != 0
}
