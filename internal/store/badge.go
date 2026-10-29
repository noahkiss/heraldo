package store

import (
	"context"

	"github.com/noahkiss/heraldo/internal/checkin"
)

// Badge scopes: what the badge on the iOS icon, the Mac Dock and the menu
// bar counts. The badge.scope setting picks one.
const (
	// BadgeAttention counts what needs the owner: unacknowledged open
	// incidents, checks that are down, and unread events at time-sensitive
	// or above. Each thing counts once: a down check's open incident, and
	// an unread event of an open incident or a down check, are not counted
	// again.
	BadgeAttention = "attention"
	// BadgePushed counts the unread events that pushed visibly, as
	// UnreadAfter does.
	BadgePushed = "pushed"
	// BadgeNone counts nothing.
	BadgeNone = "none"
)

// Badge counts the badge by scope after the read cursor seq. projects, when
// not empty, keeps the incidents, checks and events in these projects, as
// Filter.Projects does. An unknown scope counts as attention.
func (s *Store) Badge(ctx context.Context, scope string, projects []string, seq int64) (int, error) {
	if scope == BadgeNone {
		return 0, nil
	}
	ew, eargs := projectWhere("events.project", projects)
	if ew != "" {
		ew = " AND " + ew
	}
	if scope == BadgePushed {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq > ? AND `+pushedWhere+ew,
			append([]any{seq}, eargs...)...).Scan(&n)
		return n, err
	}
	iw, iargs := projectWhere("i.project", projects)
	if iw != "" {
		iw = " AND " + iw
	}
	cw, cargs := projectWhere("c.project", projects)
	if cw != "" {
		cw = " AND " + cw
	}
	const prefix = `'` + checkin.IncidentPrefix + `'`
	q := `SELECT
	  (SELECT count(*) FROM incidents i WHERE i.state = 'open'` + iw + `)
	+ (SELECT count(*) FROM checkins c WHERE c.state = 'down'` + cw + `
	    AND NOT EXISTS (SELECT 1 FROM incidents i WHERE i.key = ` + prefix + ` || c.key AND i.state = 'open'))
	+ (SELECT count(*) FROM events WHERE seq > ? AND ` + pushedWhere + ew + `
	    AND level IN ('time-sensitive', 'critical')
	    AND NOT EXISTS (SELECT 1 FROM incidents i WHERE i.state = 'open'
	      AND (i.key = events.key OR i.key = ` + prefix + ` || events.key))
	    AND NOT (kind = 'checkin' AND EXISTS (SELECT 1 FROM checkins c WHERE c.key = events.key AND c.state = 'down')))`
	args := append(append(append(iargs, cargs...), seq), eargs...)
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// BadgeFor counts the badge by scope after the read cursor called name. No
// cursor counts the incidents and checks of attention, and no events.
func (s *Store) BadgeFor(ctx context.Context, name, scope string, projects []string) (int, error) {
	seq, found, err := s.Cursor(ctx, name)
	if err != nil {
		return 0, err
	}
	if !found {
		if scope == BadgePushed {
			return 0, nil
		}
		h, err := s.Head(ctx)
		if err != nil {
			return 0, err
		}
		seq = h
	}
	return s.Badge(ctx, scope, projects, seq)
}
