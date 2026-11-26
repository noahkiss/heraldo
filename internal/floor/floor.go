// Package floor holds the floors: the events that a lowering decision may
// not touch. The LLM's decisions and the delivery settings share it, each
// with its own list (Mode).
package floor

import (
	"strings"

	"github.com/noahkiss/heraldo/api"
)

// Mode picks a list of floors.
type Mode int

const (
	// LLM is the list that keeps the LLM off an event entirely: heraldo's
	// own events, critical, an incident at time-sensitive or above that
	// opens or updates, and a failing check-in.
	LLM Mode = iota
	// Delivery is the list a delivery setting may not lower or hide:
	// heraldo's own events, any event at time-sensitive or above, any
	// incident event, and a failing check-in.
	Delivery
)

// Event is what a floor looks at.
type Event struct {
	Source string
	Kind   api.Kind
	Action string
	// Level is the event's level; empty reads as active.
	Level api.Level
	// Failed marks a failed check-in run.
	Failed bool
}

// levelRank orders levels from quiet to loud.
var levelRank = map[api.Level]int{
	api.LevelPassive: 0, api.LevelActive: 1, api.LevelTimeSensitive: 2, api.LevelCritical: 3,
}

// Rank is a level's place from quiet (0) to loud (3); -1 for none.
func Rank(l api.Level) int {
	if r, ok := levelRank[l]; ok {
		return r
	}
	return -1
}

// Loud reports whether l is time-sensitive or critical.
func Loud(l api.Level) bool { return Rank(l) >= Rank(api.LevelTimeSensitive) }

// Own reports whether source is heraldo's own: heraldo, or heraldo-<x>.
func Own(source string) bool {
	return source == "heraldo" || strings.HasPrefix(source, "heraldo-")
}

// Check names the floor of mode that protects e, or "".
func Check(mode Mode, e Event) string {
	if mode == Delivery {
		level := e.Level
		if level == "" {
			level = api.LevelActive
		}
		switch {
		case Own(e.Source):
			return "heraldo's own event"
		case level == api.LevelCritical:
			return "critical floor"
		case e.Kind == api.KindIncident:
			return "incident floor"
		case Loud(level):
			return "time-sensitive floor"
		case e.Kind == api.KindCheckin && e.Failed:
			return "check-in fail floor"
		}
		return ""
	}
	switch {
	case Own(e.Source):
		return "heraldo's own event"
	case e.Level == api.LevelCritical:
		return "critical floor"
	case e.Kind == api.KindIncident && Loud(e.Level) && (e.Action == "open" || e.Action == "update" || e.Action == ""):
		return "incident at time-sensitive floor"
	case e.Kind == api.KindCheckin && e.Failed:
		return "check-in fail floor"
	}
	return ""
}
