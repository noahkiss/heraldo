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
// Filter.Projects does. An unknown scope counts as attention. While
// delivery settings are on (SetDeliveryOn), the badge columns of events and
// incidents decide first: on counts, off never counts, and auto leaves it
// to the scope. A down check whose incident is off does not count.
func (s *Store) Badge(ctx context.Context, scope string, projects []string, seq int64) (int, error) {
	if scope == BadgeNone {
		return 0, nil
	}
	ew, eargs := projectWhere("events.project", projects)
	if ew != "" {
		ew = " AND " + ew
	}
	// With delivery settings on, each event's and incident's badge column
	// decides first: on counts, off does not, NULL (auto) leaves it to the
	// scope's rule. Off, the columns are ignored.
	on := s.DeliveryOn()
	pushed := pushedWhere
	if on {
		pushed = `(events.badge IS NOT 'off' AND (events.badge IS 'on' OR (` + pushedWhere + `)))`
	}
	if scope == BadgePushed {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE seq > ? AND `+pushed+ew,
			append([]any{seq}, eargs...)...).Scan(&n)
		return n, err
	}
	loud := `(` + pushedWhere + ` AND level IN ('time-sensitive', 'critical'))`
	incOn, checkOn := "", ""
	if on {
		loud = `(events.badge IS NOT 'off' AND (events.badge IS 'on' OR ` + loud + `))`
		incOn = ` AND i.badge IS NOT 'off'`
		checkOn = ` AND NOT EXISTS (SELECT 1 FROM incidents ci WHERE ci.key = '` + checkin.IncidentPrefix +
			`' || c.key AND ci.badge IS 'off')`
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
	  (SELECT count(*) FROM incidents i WHERE i.state = 'open'` + incOn + iw + `)
	+ (SELECT count(*) FROM checkins c WHERE c.state = 'down'` + checkOn + cw + `
	    AND NOT EXISTS (SELECT 1 FROM incidents i WHERE i.key = ` + prefix + ` || c.key AND i.state = 'open'))
	+ (SELECT count(*) FROM events WHERE seq > ? AND ` + loud + ew + `
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
