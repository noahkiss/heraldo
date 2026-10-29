package store

import (
	"context"
	"database/sql"
	"errors"
)

// StandbyValue returns the standby state called name, or "" when unset.
func (s *Store) StandbyValue(ctx context.Context, name string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM standby_state WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetStandbyValue stores the standby state called name.
func (s *Store) SetStandbyValue(ctx context.Context, name, value string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO standby_state (name, value) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	return err
}

// MirrorDevices replaces every device with ds, ids and retirements kept, as
// a standby copies the primary's registry. A device not in ds is deleted:
// the primary owns the registry, and the standby only pushes to it.
func (s *Store) MirrorDevices(ctx context.Context, ds []Device) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM devices`); err != nil {
		return err
	}
	for _, d := range ds {
		var retired any
		if !d.RetiredAt.IsZero() {
			retired = d.RetiredAt.UTC().Format(timeFormat)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO devices (`+deviceCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.Token, d.Environment, d.Topic, nullStr(d.Name), nullStr(d.Platform), nullStr(d.AppVersion),
			d.Alerts, d.TimeSensitive, d.LiveActivities, d.RegisteredAt.UTC().Format(timeFormat),
			d.UpdatedAt.UTC().Format(timeFormat), d.UpdatedBy, retired, nullStr(d.RetiredReason),
			nullStr(d.LiveActivityToken), focusJSON(d.FocusFilter)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
