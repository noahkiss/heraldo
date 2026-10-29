package server

import (
	"context"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/auth"
)

// CriticalMark is the key in details.heraldo that says who allowed a
// critical level: "rule:<name>" or "token:<name>". Only the server writes
// it; a sender's own value is removed. The pusher sends critical only to an
// event that carries it (push.CriticalAllowed).
const CriticalMark = "critical"

// markCritical removes any critical mark the sender wrote into f, then marks
// a critical event whose level a rule set (ruleLevel) or whose token carries
// the critical capability. Any other critical event stays unmarked and is
// pushed as time-sensitive.
func markCritical(ctx context.Context, f map[string]any, ruleLevel api.Level) {
	details, _ := f["details"].(map[string]any)
	own, _ := details["heraldo"].(map[string]any)
	if _, forged := own[CriticalMark]; forged {
		delete(own, CriticalMark)
	}
	if lv, _ := f["level"].(string); lv != string(api.LevelCritical) {
		return
	}
	by := ""
	switch t, _ := ctx.Value(tokenKey{}).(auth.Token); {
	case ruleLevel == api.LevelCritical:
		by = "rule"
		if h, _ := own["rule"].(string); h != "" {
			by += ":" + h
		}
	case t.Has(auth.CapCritical):
		by = "token:" + t.Name
	default:
		return
	}
	if details == nil {
		details = map[string]any{}
		f["details"] = details
	}
	if own == nil {
		own = map[string]any{}
		details["heraldo"] = own
	}
	own[CriticalMark] = by
}
