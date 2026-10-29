package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Mute holds back the pushes of the events it matches until Until. An
// empty Source, Key or Project matches any.
type Mute struct {
	ID        string
	Source    string
	Key       string
	Project   string
	Until     time.Time
	CreatedAt time.Time
	CreatedBy string
	// EndedAt is set when the mute was ended early.
	EndedAt time.Time
	EndedBy string
	// Remind makes the mute a snooze: when it ends and its key's incident
	// is still open, the pusher pushes the key's last event again.
	Remind bool
	// RemindedAt is when the pusher took the reminder; zero before.
	RemindedAt time.Time
}

// Active reports whether the mute holds back pushes at now.
func (m Mute) Active(now time.Time) bool { return m.EndedAt.IsZero() && m.Until.After(now) }

// Matches reports whether the mute covers an event with source, key and
// project.
func (m Mute) Matches(source, key, project string) bool {
	return (m.Source == "" || m.Source == source) && (m.Key == "" || m.Key == key) &&
		(m.Project == "" || m.Project == project)
}

const muteCols = `id, source, key, project, until, created_at, created_by, ended_at, ended_by, remind, reminded_at`

func scanMute(r scanner) (Mute, error) {
	var m Mute
	var source, key, project, endedBy sql.NullString
	var until, created, ended, reminded sql.NullString
	if err := r.Scan(&m.ID, &source, &key, &project, &until, &created, &m.CreatedBy, &ended, &endedBy,
		&m.Remind, &reminded); err != nil {
		return Mute{}, err
	}
	m.RemindedAt = parseNullTime(reminded)
	m.Source, m.Key, m.Project, m.EndedBy = source.String, key.String, project.String, endedBy.String
	m.Until, m.CreatedAt, m.EndedAt = parseNullTime(until), parseNullTime(created), parseNullTime(ended)
	return m, nil
}

// AddMute stores m. m.ID and m.CreatedAt must be set.
func (s *Store) AddMute(ctx context.Context, m Mute) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mutes (id, source, key, project, until, created_at, created_by, remind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.ID, nullStr(m.Source), nullStr(m.Key), nullStr(m.Project), fmtTime(m.Until),
		fmtTime(m.CreatedAt), m.CreatedBy, m.Remind)
	return s.bumpIf(err, DomainMutes, DomainState)
}

// GetMute returns the mute with id, active or not.
func (s *Store) GetMute(ctx context.Context, id string) (Mute, error) {
	m, err := scanMute(s.db.QueryRowContext(ctx, `SELECT `+muteCols+` FROM mutes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Mute{}, ErrNotFound
	}
	return m, err
}

// Mutes returns the mutes active at now, soonest to end first.
func (s *Store) Mutes(ctx context.Context, now time.Time) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+muteCols+` FROM mutes
		WHERE ended_at IS NULL AND until > ? ORDER BY until, id`, now.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mute
	for rows.Next() {
		m, err := scanMute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MatchMute returns the active mute at now that covers an event with
// source, key and project, the one that lasts longest. An empty project
// matches no project mute.
func (s *Store) MatchMute(ctx context.Context, source, key, project string, now time.Time) (Mute, bool, error) {
	m, err := scanMute(s.db.QueryRowContext(ctx, `SELECT `+muteCols+` FROM mutes
		WHERE ended_at IS NULL AND until > ?
		AND (source IS NULL OR source = ?) AND (key IS NULL OR key = ?) AND (project IS NULL OR project = ?)
		ORDER BY until DESC LIMIT 1`, now.UTC().Format(timeFormat), source, key, project))
	if errors.Is(err, sql.ErrNoRows) {
		return Mute{}, false, nil
	}
	return m, err == nil, err
}

// EndMute ends the mute with id at now. A mute that already ended or
// expired is returned as it is.
func (s *Store) EndMute(ctx context.Context, id, by string, now time.Time) (Mute, error) {
	s.wmu.Lock()
	res, err := s.db.ExecContext(ctx, `UPDATE mutes SET ended_at = ?, ended_by = ?
		WHERE id = ? AND ended_at IS NULL AND until > ?`,
		fmtTime(now), by, id, now.UTC().Format(timeFormat))
	if err != nil {
		s.wmu.Unlock()
		return Mute{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.changed(DomainMutes, DomainState)
	}
	s.wmu.Unlock()
	return s.GetMute(ctx, id)
}

// DueReminders returns the snoozes that ended by now, by their until or
// early, and have not reminded yet, oldest end first.
func (s *Store) DueReminders(ctx context.Context, now time.Time) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+muteCols+` FROM mutes
		WHERE remind = 1 AND reminded_at IS NULL AND (ended_at IS NOT NULL OR until <= ?)
		ORDER BY coalesce(ended_at, until), id`, now.UTC().Format(timeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mute
	for rows.Next() {
		m, err := scanMute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClaimReminder marks the snooze id reminded at now. It reports false when
// it was reminded already, so a reminder fires once.
func (s *Store) ClaimReminder(ctx context.Context, id string, now time.Time) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE mutes SET reminded_at = ? WHERE id = ? AND remind = 1 AND reminded_at IS NULL`,
		fmtTime(now), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if n == 1 {
		s.changed(DomainMutes)
	}
	return n == 1, err
}

// OpenIncident is an incident that is not acked or resolved.
type OpenIncident struct {
	Key string
	// LastEvent is the id of the key's newest event.
	LastEvent string
	OpenedAt  time.Time
}

// OpenIncident returns the incident key when its state is open: not acked
// and not resolved. ok is false otherwise.
func (s *Store) OpenIncident(ctx context.Context, key string) (OpenIncident, bool, error) {
	var o OpenIncident
	var opened sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT key, last_event, opened_at FROM incidents WHERE key = ? AND state = 'open'`, key).
		Scan(&o.Key, &o.LastEvent, &opened)
	if errors.Is(err, sql.ErrNoRows) {
		return OpenIncident{}, false, nil
	}
	if err != nil {
		return OpenIncident{}, false, err
	}
	o.OpenedAt = parseNullTime(opened)
	return o, true, nil
}
