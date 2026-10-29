package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrViewName means another view has the name, without regard to case.
var ErrViewName = errors.New("another view has that name")

// ViewFilter is a saved view's event filter. Empty fields do not filter.
// It is stored as JSON in views.filter and matches the API's ViewFilter.
type ViewFilter struct {
	Project []string `json:"project,omitempty"`
	Source  string   `json:"source,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Host    string   `json:"host,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	Level   []string `json:"level,omitempty"`
	Q       string   `json:"q,omitempty"`
	Routine string   `json:"routine,omitempty"`
}

// Filter is f as a List filter. expand turns project names into the names
// to match, aliases included; nil keeps them as they are.
func (f ViewFilter) Filter(expand func([]string) []string) Filter {
	out := Filter{Source: f.Source, Kind: f.Kind, Host: f.Host, Tag: f.Tag, Q: f.Q, Routine: f.Routine,
		Levels: f.Level}
	if len(f.Project) > 0 {
		var names []string
		none := false
		for _, p := range f.Project {
			if p == NoProject {
				none = true
			} else {
				names = append(names, p)
			}
		}
		if len(names) > 0 && expand != nil {
			names = expand(names)
		}
		out.Projects = append([]string{}, names...)
		if none {
			out.Projects = append(out.Projects, NoProject)
		}
	}
	return out
}

// ViewBundle folds a burst of a view's pushes into one notification: within
// Window of the first push, the pushes after the first MaxVisible replace
// one another.
type ViewBundle struct {
	Window     time.Duration
	MaxVisible int
}

// bundleJSON is ViewBundle as stored and served: the window as a Go
// duration string.
type bundleJSON struct {
	Window     string `json:"window"`
	MaxVisible int    `json:"max_visible,omitempty"`
}

// WindowString is the window as a short Go duration: 15m, 2h, 90s.
func (b ViewBundle) WindowString() string {
	switch d := b.Window; {
	case d > 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d > 0 && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	default:
		return d.String()
	}
}

// MarshalJSON writes the window as a duration string such as 15m.
func (b ViewBundle) MarshalJSON() ([]byte, error) {
	return json.Marshal(bundleJSON{Window: b.WindowString(), MaxVisible: b.MaxVisible})
}

// UnmarshalJSON reads what MarshalJSON writes.
func (b *ViewBundle) UnmarshalJSON(raw []byte) error {
	var j bundleJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return err
	}
	d, err := time.ParseDuration(j.Window)
	if err != nil {
		return err
	}
	b.Window, b.MaxVisible = d, j.MaxVisible
	return nil
}

// View is a saved view: a named filter every client shares.
type View struct {
	ID       string
	Name     string
	Filter   ViewFilter
	Bundle   *ViewBundle
	Position int
	Icon     string
	// CreatedAt, UpdatedAt and CreatedBy are set by the store's caller.
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
}

const viewCols = `id, name, filter, bundle, position, icon, created_at, updated_at, created_by`

func scanView(r scanner) (View, error) {
	var v View
	var filter string
	var bundle, icon sql.NullString
	var created, updated string
	if err := r.Scan(&v.ID, &v.Name, &filter, &bundle, &v.Position, &icon, &created, &updated, &v.CreatedBy); err != nil {
		return View{}, err
	}
	if err := json.Unmarshal([]byte(filter), &v.Filter); err != nil {
		return View{}, err
	}
	if bundle.Valid && bundle.String != "" {
		v.Bundle = &ViewBundle{}
		if err := json.Unmarshal([]byte(bundle.String), v.Bundle); err != nil {
			return View{}, err
		}
	}
	v.Icon, v.CreatedAt, v.UpdatedAt = icon.String, parseTime(created), parseTime(updated)
	return v, nil
}

func viewArgs(v View) (filter string, bundle any, err error) {
	b, err := json.Marshal(v.Filter)
	if err != nil {
		return "", nil, err
	}
	if v.Bundle != nil {
		bb, err := json.Marshal(v.Bundle)
		if err != nil {
			return "", nil, err
		}
		bundle = string(bb)
	}
	return string(b), bundle, nil
}

func viewErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: views.name") {
		return ErrViewName
	}
	return err
}

// AddView stores v. v.ID and the times must be set.
func (s *Store) AddView(ctx context.Context, v View) error {
	filter, bundle, err := viewArgs(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.db.ExecContext(ctx, `INSERT INTO views (`+viewCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.Name, filter, bundle, v.Position, nullStr(v.Icon), fmtTime(v.CreatedAt), fmtTime(v.UpdatedAt), v.CreatedBy)
	return viewErr(s.bumpIf(err, DomainViews))
}

// PutView replaces the stored view with v.ID: name, filter, bundle,
// position, icon and updated_at.
func (s *Store) PutView(ctx context.Context, v View) error {
	filter, bundle, err := viewArgs(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE views SET name = ?, filter = ?, bundle = ?, position = ?, icon = ?,
		updated_at = ? WHERE id = ?`, v.Name, filter, bundle, v.Position, nullStr(v.Icon), fmtTime(v.UpdatedAt), v.ID)
	if err != nil {
		return viewErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.changed(DomainViews)
	return nil
}

// DeleteView removes the view with id.
func (s *Store) DeleteView(ctx context.Context, id string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM views WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.changed(DomainViews)
	return nil
}

// GetView returns the view whose id is ref, else the one whose name is ref
// without regard to case.
func (s *Store) GetView(ctx context.Context, ref string) (View, error) {
	v, err := scanView(s.db.QueryRowContext(ctx, `SELECT `+viewCols+` FROM views
		WHERE id = ? OR name = ? COLLATE NOCASE ORDER BY id = ? DESC LIMIT 1`, ref, ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, ErrNotFound
	}
	return v, err
}

// Views returns every view by position, then name.
func (s *Store) Views(ctx context.Context) ([]View, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+viewCols+` FROM views ORDER BY position, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Matches reports whether the stored event with seq passes f. It runs the
// List query, so a view matches at push time exactly as it lists.
func (s *Store) Matches(ctx context.Context, f Filter, seq int64) (bool, error) {
	f.AfterSeq, f.BeforeSeq, f.Limit, f.Desc = seq-1, seq+1, 1, false
	recs, err := s.List(ctx, f)
	return len(recs) == 1, err
}
