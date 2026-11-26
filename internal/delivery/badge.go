package delivery

import (
	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/floor"
)

// Badge scopes, as badge.scope names them (store.BadgeAttention and the
// others).
const (
	ScopeAttention = "attention"
	ScopePushed    = "pushed"
	ScopeNone      = "none"
)

// pushes reports whether e at level pushes a visible alert by the push
// table's kind, action and level rules, as store.pushedWhere does. It
// leaves out the hold, mutes and expiry, which depend on the moment.
func pushes(e Event, level api.Level) bool {
	if level == api.LevelPassive {
		return false
	}
	switch e.Kind {
	case api.KindMessage:
		return true
	case api.KindIncident:
		return e.Action != "ack" && e.Action != "resolve"
	case api.KindCheckin:
		return e.Failed
	}
	return false
}

// DefaultBadge reports whether e at level counts toward the badge by
// scope alone, while it is unread or its incident is open: the badge
// without delivery settings. An incident that opens or updates counts under
// attention whatever its level, as an open incident does.
func DefaultBadge(e Event, level api.Level, scope string) bool {
	if level == "" {
		level = api.LevelActive
	}
	switch scope {
	case ScopeNone:
		return false
	case ScopePushed:
		return pushes(e, level)
	}
	if e.Kind == api.KindIncident && e.Action != "ack" && e.Action != "resolve" {
		return true
	}
	return pushes(e, level) && floor.Loud(level)
}

// InBadge reports whether e counts toward the badge under scope after
// Resolve gave res: on counts, off does not, auto falls back to
// DefaultBadge at the delivered level. Scope none counts nothing.
func InBadge(e Event, res Result, scope string) bool {
	if scope == ScopeNone {
		return false
	}
	switch res.BadgeChoice() {
	case BadgeOn:
		return true
	case BadgeOff:
		return false
	}
	return DefaultBadge(e, res.Effective, scope)
}
