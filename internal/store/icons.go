package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Icon is one entry of the icon registry.
type Icon struct {
	Name        string
	ContentType string
	Data        []byte // nil in Icons
	Size        int
	SHA256      string
	UpdatedAt   time.Time
	UpdatedBy   string
}

// PutIcon stores or replaces the icon called ic.Name. It fills SHA256 and
// Size, UpdatedAt when it is zero, and reports whether the name is new.
func (s *Store) PutIcon(ctx context.Context, ic *Icon) (bool, error) {
	if ic.UpdatedAt.IsZero() {
		ic.UpdatedAt = s.now()
	}
	sum := sha256.Sum256(ic.Data)
	ic.SHA256, ic.Size = hex.EncodeToString(sum[:]), len(ic.Data)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM icons WHERE name = ?`, ic.Name).Scan(&n); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO icons (name, content_type, data, sha256, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET content_type = excluded.content_type, data = excluded.data,
		sha256 = excluded.sha256, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		ic.Name, ic.ContentType, ic.Data, ic.SHA256, formatTime(ic.UpdatedAt), ic.UpdatedBy); err != nil {
		return false, err
	}
	return n == 0, tx.Commit()
}

// GetIcon returns the icon called name, with its data.
func (s *Store) GetIcon(ctx context.Context, name string) (Icon, error) {
	var ic Icon
	var at sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT name, content_type, data, sha256, updated_at, updated_by FROM icons
		WHERE name = ?`, name).Scan(&ic.Name, &ic.ContentType, &ic.Data, &ic.SHA256, &at, &ic.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Icon{}, ErrNotFound
	}
	ic.UpdatedAt, ic.Size = parseTime(at), len(ic.Data)
	return ic, err
}

// Icons lists the registry in name order, without the image data.
func (s *Store) Icons(ctx context.Context) ([]Icon, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, content_type, length(data), sha256, updated_at, updated_by
		FROM icons ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Icon
	for rows.Next() {
		var ic Icon
		var at sql.NullString
		if err := rows.Scan(&ic.Name, &ic.ContentType, &ic.Size, &ic.SHA256, &at, &ic.UpdatedBy); err != nil {
			return nil, err
		}
		ic.UpdatedAt = parseTime(at)
		out = append(out, ic)
	}
	return out, rows.Err()
}

// HasIcon reports whether the registry holds an icon called name.
func (s *Store) HasIcon(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM icons WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// DeleteIcon removes the icon called name. ErrNotFound when there is none.
func (s *Store) DeleteIcon(ctx context.Context, name string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM icons WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
