package floor

import (
	"testing"

	"github.com/noahkiss/heraldo/api"
)

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		name        string
		e           Event
		llm, dlv    bool
		dlvFloorWas string
	}{
		{"own", Event{Source: "heraldo-cli", Kind: api.KindMessage, Level: api.LevelPassive}, true, true, "heraldo's own event"},
		{"critical message", Event{Source: "s", Kind: api.KindMessage, Level: api.LevelCritical}, true, true, "critical floor"},
		{"ts message", Event{Source: "s", Kind: api.KindMessage, Level: api.LevelTimeSensitive}, false, true, "time-sensitive floor"},
		{"active message", Event{Source: "s", Kind: api.KindMessage, Level: api.LevelActive}, false, false, ""},
		{"no level message", Event{Source: "s", Kind: api.KindMessage}, false, false, ""},
		{"ts incident open", Event{Source: "s", Kind: api.KindIncident, Action: "open", Level: api.LevelTimeSensitive}, true, true, "incident floor"},
		{"ts incident resolve", Event{Source: "s", Kind: api.KindIncident, Action: "resolve", Level: api.LevelTimeSensitive}, false, true, "incident floor"},
		{"active incident open", Event{Source: "s", Kind: api.KindIncident, Action: "open", Level: api.LevelActive}, false, true, "incident floor"},
		{"passive incident resolve", Event{Source: "s", Kind: api.KindIncident, Action: "resolve", Level: api.LevelPassive}, false, true, "incident floor"},
		{"failed check-in", Event{Source: "s", Kind: api.KindCheckin, Action: "fail", Level: api.LevelActive, Failed: true}, true, true, "check-in fail floor"},
		{"digest", Event{Source: "s", Kind: api.KindDigest, Level: api.LevelPassive}, false, false, ""},
	} {
		if got := Check(LLM, c.e) != ""; got != c.llm {
			t.Errorf("%s: LLM floor = %v, want %v", c.name, got, c.llm)
		}
		got := Check(Delivery, c.e)
		if (got != "") != c.dlv || got != c.dlvFloorWas {
			t.Errorf("%s: delivery floor = %q, want %q", c.name, got, c.dlvFloorWas)
		}
	}
}
