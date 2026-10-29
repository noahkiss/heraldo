package store

import (
	"strings"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/checkin"
)

// Domain names a part of the read API. The server keeps one revision per
// domain (GET /v1/revisions) and bumps it when a write may have changed what
// that part returns.
type Domain string

// The domains, as GET /v1/revisions names them.
const (
	DomainEvents     Domain = "events"
	DomainCursor     Domain = "cursor"
	DomainState      Domain = "state"
	DomainChecks     Domain = "checks"
	DomainProjects   Domain = "projects"
	DomainViews      Domain = "views"
	DomainHeld       Domain = "held"
	DomainActivities Domain = "activities"
	DomainLLM        Domain = "llm"
	DomainMutes      Domain = "mutes"
	DomainSettings   Domain = "settings"
)

// Domains is every domain, in the order GET /v1/revisions lists them.
var Domains = []Domain{
	DomainEvents, DomainCursor, DomainState, DomainChecks, DomainProjects, DomainViews,
	DomainHeld, DomainActivities, DomainLLM, DomainMutes, DomainSettings,
}

// OnChange sets the function every committed write calls with the domains
// it may have changed. It must not block or call back into the store. nil
// clears it.
func (s *Store) OnChange(f func(...Domain)) {
	if f == nil {
		s.onChange.Store(nil)
		return
	}
	s.onChange.Store(&f)
}

// changed tells the OnChange function about a committed write.
func (s *Store) changed(ds ...Domain) {
	if f := s.onChange.Load(); f != nil && len(ds) > 0 {
		(*f)(ds...)
	}
}

// bumpIf tells the OnChange function about ds when err is nil, and returns
// err.
func (s *Store) bumpIf(err error, ds ...Domain) error {
	if err == nil {
		s.changed(ds...)
	}
	return err
}

// eventDomains are the domains a newly stored event changes. Every event
// moves the head, so the unread count (cursor), the badge (state) and the
// project counts; a check-in event or a check's incident changes the
// checks; a progress event changes the activities.
func eventDomains(in *api.EventInput) []Domain {
	ds := []Domain{DomainEvents, DomainCursor, DomainState, DomainProjects}
	if in == nil || in.Kind == nil {
		return ds
	}
	key := ""
	if in.Key != nil {
		key = *in.Key
	}
	switch *in.Kind {
	case api.KindCheckin:
		ds = append(ds, DomainChecks)
	case api.KindIncident:
		if strings.HasPrefix(key, checkin.IncidentPrefix) {
			ds = append(ds, DomainChecks)
		}
	case api.KindProgress:
		ds = append(ds, DomainActivities)
	}
	return ds
}
