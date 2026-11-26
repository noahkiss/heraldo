package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/noahkiss/heraldo/api"
	"github.com/noahkiss/heraldo/internal/delivery"
)

// SetDeliveryOn makes f the source of delivery.enabled for the badge: while
// it reports false, the badge ignores every stamp, so stamps left from an
// earlier "on" do nothing. nil means off. The server sets it.
func (s *Store) SetDeliveryOn(f func() bool) {
	if f == nil {
		s.deliveryOn.Store(nil)
		return
	}
	s.deliveryOn.Store(&f)
}

// DeliveryOn reports delivery.enabled as SetDeliveryOn's source says.
func (s *Store) DeliveryOn() bool {
	f := s.deliveryOn.Load()
	return f != nil && (*f)()
}

// stamp is the part of an event's deliver field the store indexes.
type stamp struct {
	As    string `json:"as"`
	Badge string `json:"badge"`
}

// stampOf reads the deliver field of a stored event's fields: an
// *api.DeliverStamp from the server, or a map from an import.
func stampOf(f map[string]any) (stamp, bool) {
	v, ok := f["deliver"]
	if !ok || v == nil {
		return stamp{}, false
	}
	if d, ok := v.(*api.DeliverStamp); ok {
		return stamp{As: string(d.As), Badge: string(d.Badge)}, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return stamp{}, false
	}
	var st stamp
	if json.Unmarshal(b, &st) != nil || st.As == "" {
		return stamp{}, false
	}
	return st, true
}

// badgeCol is the badge column for a badge choice: on, off, or NULL for
// auto.
func badgeCol(b string) any {
	if b == string(delivery.BadgeOn) || b == string(delivery.BadgeOff) {
		return b
	}
	return nil
}

// ---- rows --------------------------------------------------------------

const deliveryCols = `id, position, match, deliver, badge, default_level, note, created_at, updated_at, updated_by`

func scanDelivery(r scanner) (delivery.Row, error) {
	var d delivery.Row
	var match string
	var deliver, level, note sql.NullString
	var badge, created, updated string
	if err := r.Scan(&d.ID, &d.Position, &match, &deliver, &badge, &level, &note, &created, &updated, &d.UpdatedBy); err != nil {
		return delivery.Row{}, err
	}
	if err := json.Unmarshal([]byte(match), &d.Match); err != nil {
		return delivery.Row{}, err
	}
	d.Deliver, d.Badge = delivery.Outcome(deliver.String), delivery.Badge(badge)
	d.DefaultLevel, d.Note = api.Level(level.String), note.String
	d.CreatedAt, d.UpdatedAt = parseTime(created), parseTime(updated)
	return d, nil
}

func deliveryArgs(d delivery.Row) []any {
	match, _ := json.Marshal(d.Match)
	badge := string(d.Badge)
	if badge == "" {
		badge = string(delivery.BadgeAuto)
	}
	return []any{d.ID, d.Position, string(match), nullStr(string(d.Deliver)), badge, nullStr(string(d.DefaultLevel)),
		nullStr(d.Note), fmtTime(d.CreatedAt), fmtTime(d.UpdatedAt), d.UpdatedBy}
}

// deliveryDomains are the domains a row change moves: the rows, and the
// badge, which a re-stamp may change (cursor and state answers carry it).
var deliveryDomains = []Domain{DomainDelivery, DomainCursor, DomainState}

// AddDelivery stores d. d.ID and the times must be set.
func (s *Store) AddDelivery(ctx context.Context, d delivery.Row) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO delivery_settings (`+deliveryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		deliveryArgs(d)...)
	return s.bumpIf(err, deliveryDomains...)
}

// PutDelivery replaces the row with d.ID, created_at kept.
func (s *Store) PutDelivery(ctx context.Context, d delivery.Row) error {
	a := deliveryArgs(d)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE delivery_settings SET position = ?, match = ?, deliver = ?, badge = ?,
		default_level = ?, note = ?, updated_at = ?, updated_by = ? WHERE id = ?`,
		a[1], a[2], a[3], a[4], a[5], a[6], a[8], a[9], a[0])
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.changed(deliveryDomains...)
	return nil
}

// DeleteDelivery removes the row with id.
func (s *Store) DeleteDelivery(ctx context.Context, id string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM delivery_settings WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.changed(deliveryDomains...)
	return nil
}

// GetDelivery returns the row with id.
func (s *Store) GetDelivery(ctx context.Context, id string) (delivery.Row, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM delivery_settings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.Row{}, ErrNotFound
	}
	return d, err
}

// DeliveryRows returns every row in order: position, then creation.
func (s *Store) DeliveryRows(ctx context.Context) ([]delivery.Row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM delivery_settings ORDER BY position, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []delivery.Row
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MirrorDelivery replaces every row with ds: a standby's copy of its
// primary's rows. It reports whether anything changed.
func (s *Store) MirrorDelivery(ctx context.Context, ds []delivery.Row) (bool, error) {
	old, err := s.DeliveryRows(ctx)
	if err != nil {
		return false, err
	}
	if sameRows(old, ds) {
		return false, nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM delivery_settings`); err != nil {
		return false, err
	}
	for _, d := range ds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_settings (`+deliveryCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			deliveryArgs(d)...); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.changed(deliveryDomains...)
	return true, nil
}

func sameRows(a, b []delivery.Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, _ := json.Marshal(deliveryArgs(a[i]))
		y, _ := json.Marshal(deliveryArgs(b[i]))
		if string(x) != string(y) {
			return false
		}
	}
	return true
}

// ---- sources -----------------------------------------------------------

// Source is one row of the sources table.
type Source struct {
	Source     string
	FirstSeen  time.Time
	LastSeen   time.Time
	ReviewedAt time.Time // zero: new
}

// New reports whether the source is new: not yet reviewed.
func (s Source) New() bool { return s.ReviewedAt.IsZero() }

// SourceNew reports whether source is new: never stored, or stored and not
// yet reviewed.
func (s *Store) SourceNew(ctx context.Context, source string) (bool, error) {
	var reviewed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT reviewed_at FROM sources WHERE source = ?`, strings.ToLower(source)).Scan(&reviewed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return !reviewed.Valid || reviewed.String == "", err
}

// noteSource records an event from source at at, in the event's
// transaction. It reports whether the source was first seen.
func noteSource(ctx context.Context, tx *sql.Tx, source string, at time.Time) (bool, error) {
	ts := at.UTC().Format(timeFormat)
	res, err := tx.ExecContext(ctx, `INSERT INTO sources (source, first_seen, last_seen) VALUES (?, ?, ?)
		ON CONFLICT (source) DO NOTHING`, strings.ToLower(source), ts, ts)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE sources SET last_seen = max(last_seen, ?), first_seen = min(first_seen, ?)
		WHERE source = ?`, ts, ts, strings.ToLower(source))
	return false, err
}

// Sources returns every source, new ones first, then by name.
func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, first_seen, last_seen, reviewed_at FROM sources
		ORDER BY reviewed_at IS NOT NULL, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		var first, last string
		var reviewed sql.NullString
		if err := rows.Scan(&x.Source, &first, &last, &reviewed); err != nil {
			return nil, err
		}
		x.FirstSeen, x.LastSeen, x.ReviewedAt = parseTime(first), parseTime(last), parseTime(reviewed.String)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ReviewSource marks source reviewed at at. ErrNotFound for a source never
// stored.
func (s *Store) ReviewSource(ctx context.Context, source string, at time.Time) (Source, error) {
	s.wmu.Lock()
	res, err := s.db.ExecContext(ctx, `UPDATE sources SET reviewed_at = ? WHERE source = ?`,
		fmtTime(at), strings.ToLower(source))
	s.wmu.Unlock()
	if err != nil {
		return Source{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Source{}, ErrNotFound
	}
	s.changed(DomainDelivery)
	all, err := s.Sources(ctx)
	if err != nil {
		return Source{}, err
	}
	for _, x := range all {
		if x.Source == strings.ToLower(source) {
			return x, nil
		}
	}
	return Source{}, ErrNotFound
}

// MirrorSources copies a primary's sources: each one's review state, and
// the earlier first and the later last time. A source the standby never
// stored is added.
func (s *Store) MirrorSources(ctx context.Context, srcs []Source) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	for _, x := range srcs {
		res, err := tx.ExecContext(ctx, `INSERT INTO sources (source, first_seen, last_seen, reviewed_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (source) DO UPDATE SET first_seen = min(first_seen, excluded.first_seen),
			  last_seen = max(last_seen, excluded.last_seen), reviewed_at = excluded.reviewed_at
			WHERE sources.reviewed_at IS NOT excluded.reviewed_at OR sources.first_seen > excluded.first_seen
			  OR sources.last_seen < excluded.last_seen`,
			strings.ToLower(x.Source), fmtTime(x.FirstSeen), fmtTime(x.LastSeen), fmtTime(x.ReviewedAt))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		s.changed(DomainDelivery)
	}
	return nil
}

// SourceCount is one source's events since a time: how many, and how many
// counted toward the badge.
type SourceCount struct {
	Events, BadgeIn int
}

// SourceCounts counts the events since since per source (folded), routine
// events left out, and how many of them count toward the badge under
// scope: by their badge column while delivery is on, else by the scope's
// rule as the badge reads it (an open incident event for attention; a
// pushed event, time-sensitive and up for attention).
func (s *Store) SourceCounts(ctx context.Context, scope string, since time.Time) (map[string]SourceCount, error) {
	in := "0"
	switch scope {
	case BadgeNone:
	case BadgePushed:
		in = pushedWhere
	default:
		in = `((kind = 'incident' AND coalesce(action, '') NOT IN ('ack', 'resolve'))
		  OR (` + pushedWhere + ` AND level IN ('time-sensitive', 'critical')))`
	}
	if s.DeliveryOn() && scope != BadgeNone {
		in = `(coalesce(badge, '') = 'on' OR (coalesce(badge, '') != 'off' AND ` + in + `))`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT lower(source), count(*), sum(CASE WHEN `+in+` THEN 1 ELSE 0 END)
		FROM events WHERE received_at >= ? AND NOT `+RoutineWhere+` GROUP BY lower(source)`,
		since.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SourceCount{}
	for rows.Next() {
		var src string
		var c SourceCount
		if err := rows.Scan(&src, &c.Events, &c.BadgeIn); err != nil {
			return nil, err
		}
		out[src] = c
	}
	return out, rows.Err()
}

// EventsSince returns the events received since since that are not
// routine, newest first. The delivery preview runs over them.
func (s *Store) EventsSince(ctx context.Context, since time.Time) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, id, event FROM events WHERE received_at >= ? AND NOT `+RoutineWhere+`
		ORDER BY seq DESC`, since.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var raw string
		if err := rows.Scan(&r.Seq, &r.ID, &raw); err != nil {
			return nil, err
		}
		r.Event = json.RawMessage(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- the badge re-stamp ------------------------------------------------

// Rebadge sets the badge choice of the unread events after seq and of the
// open incidents, after a row change: badge returns the choice for a stored
// event (on, off or auto), or "" to leave it. An event with a stamp gets
// the new choice in its deliver field too. It reports how many rows
// changed, and bumps the cursor and state domains when any did.
func (s *Store) Rebadge(ctx context.Context, seq int64, badge func(ev *api.Event) string) (int, error) {
	type change struct {
		id, badge string
		stamped   bool
	}
	var evs []change
	scan := func(q string, args ...any) ([]change, error) {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []change
		for rows.Next() {
			var id, raw string
			var cur sql.NullString
			if err := rows.Scan(&id, &raw, &cur); err != nil {
				return nil, err
			}
			var ev api.Event
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				continue
			}
			b := badge(&ev)
			if b == "" {
				continue
			}
			want := badgeCol(b)
			now, _ := want.(string)
			if now == cur.String {
				continue
			}
			out = append(out, change{id: id, badge: b, stamped: ev.Deliver != nil})
		}
		return out, rows.Err()
	}
	evs, err := scan(`SELECT id, event, badge FROM events WHERE seq > ? AND NOT `+RoutineWhere, seq)
	if err != nil {
		return 0, err
	}
	// An open incident takes the choice of its latest event.
	incs, err := scan(`SELECT i.key, e.event, i.badge FROM incidents i JOIN events e ON e.id = i.last_event
		WHERE i.state = 'open'`)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 && len(incs) == 0 {
		return 0, nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, c := range evs {
		q := `UPDATE events SET badge = ? WHERE id = ?`
		args := []any{badgeCol(c.badge), c.id}
		if c.stamped {
			q = `UPDATE events SET badge = ?, event = json_set(event, '$.deliver.badge', ?) WHERE id = ?`
			args = []any{badgeCol(c.badge), c.badge, c.id}
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return 0, err
		}
	}
	for _, c := range incs {
		if _, err := tx.ExecContext(ctx, `UPDATE incidents SET badge = ? WHERE key = ?`, badgeCol(c.badge), c.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.changed(DomainCursor, DomainState)
	return len(evs) + len(incs), nil
}
