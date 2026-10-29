package store

import (
	"context"
	"encoding/json"
	"time"
)

// Setting is one stored server setting: a value the operator wrote through
// the API. internal/settings owns what the keys mean.
type Setting struct {
	Key       string
	Value     json.RawMessage
	UpdatedAt time.Time
	UpdatedBy string
}

// Settings returns every stored setting, by key.
func (s *Store) Settings(ctx context.Context) ([]Setting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, updated_at, updated_by FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var st Setting
		var v, at string
		if err := rows.Scan(&st.Key, &v, &at, &st.UpdatedBy); err != nil {
			return nil, err
		}
		st.Value, st.UpdatedAt = json.RawMessage(v), parseTime(at)
		out = append(out, st)
	}
	return out, rows.Err()
}

// PutSetting stores st, replacing the key's row.
func (s *Store) PutSetting(ctx context.Context, st Setting) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at,
		updated_by = excluded.updated_by`, st.Key, string(st.Value), fmtTime(st.UpdatedAt), st.UpdatedBy)
	return err
}

// DeleteSetting removes the key's row. It reports whether there was one.
func (s *Store) DeleteSetting(ctx context.Context, key string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
