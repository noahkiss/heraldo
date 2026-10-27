package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Mute holds back the pushes of the events it matches until Until. An
// empty Source or Key matches any.
type Mute struct {
	ID        string
	Source    string
	Key       string
	Until     time.Time
	CreatedAt time.Time
	CreatedBy string
	// EndedAt is set when the mute was ended early.
	EndedAt time.Time
	EndedBy string
}

// Active reports whether the mute holds back pushes at now.
func (m Mute) Active(now time.Time) bool { return m.EndedAt.IsZero() && m.Until.After(now) }

// Matches reports whether the mute covers an event with source and key.
func (m Mute) Matches(source, key string) bool {
	return (m.Source == "" || m.Source == source) && (m.Key == "" || m.Key == key)
}

const muteCols = `id, source, key, until, created_at, created_by, ended_at, ended_by`

func scanMute(r scanner) (Mute, error) {
	var m Mute
	var source, key, endedBy sql.NullString
	var until, created, ended sql.NullString
	if err := r.Scan(&m.ID, &source, &key, &until, &created, &m.CreatedBy, &ended, &endedBy); err != nil {
		return Mute{}, err
	}
	m.Source, m.Key, m.EndedBy = source.String, key.String, endedBy.String
	m.Until, m.CreatedAt, m.EndedAt = parseTime(until), parseTime(created), parseTime(ended)
	return m, nil
}

// AddMute stores m. m.ID and m.CreatedAt must be set.
func (s *Store) AddMute(ctx context.Context, m Mute) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mutes (id, source, key, until, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?)`, m.ID, nullStr(m.Source), nullStr(m.Key), formatTime(m.Until),
		formatTime(m.CreatedAt), m.CreatedBy)
	return err
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

// MatchMute returns the active mute at now that covers an event with source
// and key, the one that lasts longest.
func (s *Store) MatchMute(ctx context.Context, source, key string, now time.Time) (Mute, bool, error) {
	m, err := scanMute(s.db.QueryRowContext(ctx, `SELECT `+muteCols+` FROM mutes
		WHERE ended_at IS NULL AND until > ?
		AND (source IS NULL OR source = ?) AND (key IS NULL OR key = ?)
		ORDER BY until DESC LIMIT 1`, now.UTC().Format(timeFormat), source, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Mute{}, false, nil
	}
	return m, err == nil, err
}

// EndMute ends the mute with id at now. A mute that already ended or
// expired is returned as it is.
func (s *Store) EndMute(ctx context.Context, id, by string, now time.Time) (Mute, error) {
	s.wmu.Lock()
	if _, err := s.db.ExecContext(ctx, `UPDATE mutes SET ended_at = ?, ended_by = ?
		WHERE id = ? AND ended_at IS NULL AND until > ?`,
		formatTime(now), by, id, now.UTC().Format(timeFormat)); err != nil {
		s.wmu.Unlock()
		return Mute{}, err
	}
	s.wmu.Unlock()
	return s.GetMute(ctx, id)
}
