package client

import (
	"strings"

	"github.com/noahkiss/heraldo/api"
)

// FocusText is a device's Focus filter in a few words, such as
// "focus Sleep: critical only, projects infra". Empty for none.
func FocusText(f *api.FocusFilter) string {
	if f == nil || !f.Active {
		return ""
	}
	s := "focus"
	if f.Name != nil && *f.Name != "" {
		s += " " + *f.Name
	}
	if f.BreakThrough != nil && *f.BreakThrough == api.FocusFilterBreakThroughCritical {
		s += ": critical only"
	} else {
		s += ": time-sensitive and up"
	}
	if f.Projects != nil && len(*f.Projects) > 0 {
		s += ", projects " + strings.Join(*f.Projects, ",")
	}
	return s
}
