package server

import (
	"fmt"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/store"
)

// focusJSON is a stored Focus filter as the API shows it; nil for none.
func focusJSON(f *store.FocusFilter) *api.FocusFilter {
	if f == nil {
		return nil
	}
	bt := api.FocusFilterBreakThrough(f.BreakThrough)
	out := &api.FocusFilter{Active: true, BreakThrough: &bt}
	if f.Name != "" {
		out.Name = &f.Name
	}
	if len(f.Projects) > 0 {
		p := append([]string(nil), f.Projects...)
		out.Projects = &p
	}
	if !f.UpdatedAt.IsZero() {
		t := f.UpdatedAt
		out.UpdatedAt = &t
	}
	return out
}

// focusFrom checks a registration's Focus filter. It returns nil for an
// inactive one, which clears the stored filter, and the problems found.
func focusFrom(in *api.FocusFilter) (*store.FocusFilter, []string) {
	if in == nil || !in.Active {
		return nil, nil
	}
	var problems []string
	f := &store.FocusFilter{BreakThrough: string(api.FocusFilterBreakThroughTimeSensitive)}
	if in.BreakThrough != nil {
		switch *in.BreakThrough {
		case api.FocusFilterBreakThroughTimeSensitive, api.FocusFilterBreakThroughCritical:
			f.BreakThrough = string(*in.BreakThrough)
		default:
			problems = append(problems, "focus_filter.break_through: want time-sensitive or critical")
		}
	}
	if in.Name != nil {
		if len(*in.Name) > 64 {
			problems = append(problems, "focus_filter.name: at most 64 bytes")
		}
		f.Name = *in.Name
	}
	if in.Projects != nil {
		if len(*in.Projects) > 64 {
			problems = append(problems, "focus_filter.projects: at most 64")
		}
		for _, p := range *in.Projects {
			if p == "" || len(p) > 128 {
				problems = append(problems, fmt.Sprintf("focus_filter.projects: %q: want 1 to 128 bytes", p))
				break
			}
		}
		f.Projects = append([]string(nil), *in.Projects...)
	}
	return f, problems
}

// focusCopy is a Focus filter from GET /v1/standby/devices as the store
// keeps it: the primary's stamp stays.
func focusCopy(in *api.FocusFilter) *store.FocusFilter {
	f, problems := focusFrom(in)
	if f == nil || len(problems) > 0 {
		return nil
	}
	if in.UpdatedAt != nil {
		f.UpdatedAt = in.UpdatedAt.UTC().Truncate(time.Second)
	}
	return f
}
